package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed assets/appicon.png
var iconData []byte

// ── Data types ───────────────────────────────────────────────

type ProviderUsage struct {
	Name       string   `json:"name"`
	Metrics    []Metric `json:"metrics"`
	ResetIn    string   `json:"resetIn"`
	ResetEpoch int64    `json:"resetEpoch"`
}

type Metric struct {
	Label string  `json:"label"`
	Used  float64 `json:"used"`
	Max   float64 `json:"max"`
	Pct   float64 `json:"pct"`
}

type AllUsage struct {
	UpdatedAt string          `json:"updatedAt"`
	Providers []ProviderUsage `json:"providers"`
}

// ── Wails events ────────────────────────────────────────────

func init() {
	application.RegisterEvent[AllUsage]("usage")
}

// tmuxPath ensures we find tmux even when launched from macOS GUI (no shell PATH)
var tmuxPath = func() string {
	for _, p := range []string{"/opt/homebrew/bin/tmux", "/usr/local/bin/tmux", "/usr/bin/tmux"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "tmux" // fallback to PATH lookup
}()

// ── DoToken service (bound to frontend) ─────────────────

type DoToken struct{}

type AppConfig struct {
	ZaiToken        string   `json:"zaiToken"`
	ClaudeSession   string   `json:"claudeSession"`
	OpenCodeCookie  string   `json:"openCodeCookie"`
	ProviderOrder   []string `json:"providerOrder"`
}

func getConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".dotoken.json")
}

func (t *DoToken) SaveSettings(zaiToken, claudeSession, openCodeCookie string) (string, error) {
	var warning string
	if claudeSession != "" {
		if err := exec.Command(tmuxPath, "has-session", "-t", claudeSession).Run(); err != nil {
			warning = fmt.Sprintf("tmux session '%s' not found. Run: tmux new-session -d -s %s \"claude\"", claudeSession, claudeSession)
		}
	}

	cfg := AppConfig{ZaiToken: zaiToken, ClaudeSession: claudeSession, OpenCodeCookie: openCodeCookie}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return warning, err
	}
	err = os.WriteFile(getConfigPath(), data, 0644)
	if err == nil {
		go refreshUsage()
	}
	return warning, err
}

func (t *DoToken) GetSettings() AppConfig {
	data, err := os.ReadFile(getConfigPath())
	if err != nil {
		return AppConfig{}
	}
	var cfg AppConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return AppConfig{}
	}
	return cfg
}

func (t *DoToken) QuitApp() {
	os.Exit(0)
}

func (t *DoToken) SaveProviderOrder(order []string) error {
	cfg := t.GetSettings()
	cfg.ProviderOrder = order
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(getConfigPath(), data, 0644)
}

func applyOrder(providers []ProviderUsage, order []string) []ProviderUsage {
	if len(order) == 0 {
		return providers
	}
	rank := map[string]int{}
	for i, name := range order {
		rank[name] = i
	}
	reordered := make([]ProviderUsage, len(providers))
	copy(reordered, providers)
	for i := 0; i < len(reordered); i++ {
		for j := i + 1; j < len(reordered); j++ {
			ri := rank[reordered[i].Name]
			rj := rank[reordered[j].Name]
			// Unknown providers go to the end
			if _, ok := rank[reordered[j].Name]; !ok {
				continue
			}
			if _, ok := rank[reordered[i].Name]; !ok || ri > rj {
				reordered[i], reordered[j] = reordered[j], reordered[i]
			}
		}
	}
	return reordered
}

var cachedUsage AllUsage
var appWindow *application.WebviewWindow

func refreshUsage() {
	// Fetch fast providers first, emit immediately
	var providers []ProviderUsage
	if oc := fetchOpenCodeUsage(); oc != nil {
		providers = append(providers, *oc)
	}
	if zai := fetchZaiUsage(); zai != nil {
		providers = append(providers, *zai)
	}

	// Claude: use cached data if available, otherwise show loading placeholder
	cfg := AppConfig{}
	if data, err := os.ReadFile(getConfigPath()); err == nil {
		json.Unmarshal(data, &cfg)
	}
	if cfg.ClaudeSession != "" {
		sessionAlive := exec.Command(tmuxPath, "has-session", "-t", cfg.ClaudeSession).Run() == nil
		if sessionAlive {
			hasClaudeCache := false
			for _, p := range cachedUsage.Providers {
				if p.Name == "Claude" {
					providers = append([]ProviderUsage{p}, providers...)
					hasClaudeCache = true
					break
				}
			}
			if !hasClaudeCache {
				providers = append([]ProviderUsage{{
					Name:    "Claude",
					Metrics: []Metric{{Label: "Session", Pct: -1}},
					ResetIn: "loading…",
				}}, providers...)
			}
		}
	}

	if len(providers) > 0 {
		providers = applyOrder(providers, cfg.ProviderOrder)
		cachedUsage = AllUsage{
			UpdatedAt: time.Now().Format("15:04:05"),
			Providers: providers,
		}
		if appWindow != nil {
			appWindow.EmitEvent("usage", cachedUsage)
		}
	}

	// Fetch Claude in background, emit again when done
	go func() {
		claude := fetchClaudeUsage()
		if claude == nil {
			return
		}

		// Replace placeholder with real data
		for i, p := range cachedUsage.Providers {
			if p.Name == "Claude" {
				cachedUsage.Providers[i] = *claude
				cachedUsage.UpdatedAt = time.Now().Format("15:04:05")
				cachedUsage.Providers = applyOrder(cachedUsage.Providers, (&DoToken{}).GetSettings().ProviderOrder)
				if appWindow != nil {
					appWindow.EmitEvent("usage", cachedUsage)
				}
				return
			}
		}
		// Claude wasn't in cache yet, prepend it
		cachedUsage.Providers = append([]ProviderUsage{*claude}, cachedUsage.Providers...)
		cachedUsage.Providers = applyOrder(cachedUsage.Providers, (&DoToken{}).GetSettings().ProviderOrder)
		cachedUsage.UpdatedAt = time.Now().Format("15:04:05")
		if appWindow != nil {
			appWindow.EmitEvent("usage", cachedUsage)
		}
	}()
}

func (t *DoToken) FetchUsage() AllUsage {
	go refreshUsage()
	return cachedUsage
}

// StartPolling is kept for compatibility but does nothing
func (t *DoToken) StartPolling() {}

// ── Claude (tmux /usage) ──────────────────────────────────

func claudeRunningInSession(sessionName string) bool {
	// Get the pane PID of the first pane in the session
	out, err := exec.Command(tmuxPath, "list-panes", "-t", sessionName, "-F", "#{pane_pid}").Output()
	if err != nil {
		return false
	}
	pid := strings.TrimSpace(string(out))
	if pid == "" {
		return false
	}

	// Use pgrep to find any 'claude' descendant of the pane's shell
	out, err = exec.Command("pgrep", "-P", pid, "claude").Output()
	return err == nil
}

func fetchClaudeUsage() *ProviderUsage {
	sessionName := (&DoToken{}).GetSettings().ClaudeSession
	if sessionName == "" {
		return nil
	}

	// Verify the session is alive
	if err := exec.Command(tmuxPath, "has-session", "-t", sessionName).Run(); err != nil {
		return nil
	}

	// Verify Claude Code is running inside the session
	if !claudeRunningInSession(sessionName) {
		return nil
	}

	// 1. Dismiss satisfaction survey if present
	out, _ := exec.Command(tmuxPath, "capture-pane", "-t", sessionName, "-p").Output()
	if strings.Contains(string(out), "How is Claude") {
		exec.Command(tmuxPath, "send-keys", "-t", sessionName, "0").Run()
		time.Sleep(500 * time.Millisecond)
	}

	// 2. Try /usage up to 2 times (handles dismissed case)
	var output string
	for attempt := 0; attempt < 2; attempt++ {
		exec.Command(tmuxPath, "send-keys", "-t", sessionName, "Escape").Run()
		time.Sleep(100 * time.Millisecond)
		exec.Command(tmuxPath, "send-keys", "-t", sessionName, "C-u").Run()
		time.Sleep(100 * time.Millisecond)
		exec.Command(tmuxPath, "clear-history", "-t", sessionName).Run()

		exec.Command(tmuxPath, "send-keys", "-t", sessionName, "/usage", "Enter").Run()

		for i := 0; i < 10; i++ {
			time.Sleep(500 * time.Millisecond)
			out, err := exec.Command(tmuxPath, "capture-pane", "-t", sessionName, "-p").Output()
			if err != nil {
				continue
			}
			output = string(out)
			lines := strings.Split(output, "\n")
			lastUsageIdx := -1
			for j, line := range lines {
				if strings.TrimSpace(line) == "❯ /usage" {
					lastUsageIdx = j
				}
			}
			if lastUsageIdx >= 0 {
				fresh := strings.Join(lines[lastUsageIdx+1:], "\n")
				if strings.Contains(fresh, "% used") || strings.Contains(fresh, "dismissed") {
					break
				}
			}
		}

		exec.Command(tmuxPath, "send-keys", "-t", sessionName, "Escape").Run()
		time.Sleep(100 * time.Millisecond)

		if strings.Contains(output, "% used") {
			break
		}
	}

	if !strings.Contains(output, "% used") {
		return nil
	}
	return parseClaudeUsageOutput(output)
}

var (
	rePctUsed  = regexp.MustCompile(`(\d+)%\s+used`)
	reResetsIn = regexp.MustCompile(`Resets\s+(.+)`)
)

type usageBlock struct {
	label string
	pct   float64
	reset string
	key   string // "session" or "week"
}

func parseClaudeUsageOutput(output string) *ProviderUsage {
	lines := strings.Split(output, "\n")
	var blocks []usageBlock

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		var labelKey string
		var shortLabel string
		if strings.HasPrefix(trimmed, "Current session") {
			labelKey = "session"
			shortLabel = "Session"
		} else if strings.HasPrefix(trimmed, "Current week") {
			labelKey = "week"
			shortLabel = "Weekly"
		}
		if labelKey == "" {
			continue
		}

		var resetAcc string
		foundPct := false
		for j := i + 1; j < i+8 && j < len(lines); j++ {
			nextLine := strings.TrimSpace(lines[j])
			if nextLine == "" {
				continue
			}

			if m := rePctUsed.FindStringSubmatch(nextLine); m != nil {
				var pct float64
				fmt.Sscanf(m[1], "%f", &pct)
				// Remove previous duplicate for this label
				for k := len(blocks) - 1; k >= 0; k-- {
					if blocks[k].key == labelKey {
						blocks = append(blocks[:k], blocks[k+1:]...)
					}
				}
				blocks = append(blocks, usageBlock{label: shortLabel, pct: pct, key: labelKey, reset: resetAcc})
				foundPct = true
				continue
			}

			// After finding % used, keep scanning for reset time
			if foundPct {
				if m := reResetsIn.FindStringSubmatch(nextLine); m != nil {
					blocks[len(blocks)-1].reset = strings.TrimSpace(m[1])
					break
				}
				if strings.HasPrefix(nextLine, "Current session") || strings.HasPrefix(nextLine, "Current week") {
					break
				}
			} else {
				if m := reResetsIn.FindStringSubmatch(nextLine); m != nil {
					resetAcc += " " + strings.TrimSpace(m[1])
				}
			}
		}
	}

	if len(blocks) == 0 {
		return nil
	}

	var metrics []Metric
	var resetStr string

	for _, b := range blocks {
		metrics = append(metrics, Metric{
			Label: b.label,
			Pct:   b.pct,
		})

		if b.reset != "" && resetStr == "" {
			resetStr = b.reset
		}
	}

	return &ProviderUsage{
		Name:    "Claude",
		Metrics: metrics,
		ResetIn: resetStr,
	}
}

// ── OpenCode Go (web scraping) ──────────────────────────

func fetchOpenCodeUsage() *ProviderUsage {
	token := os.Getenv("OPENCODE_AUTH_COOKIE")
	if token == "" {
		token = (&DoToken{}).GetSettings().OpenCodeCookie
	}
	if token == "" {
		return nil
	}

	serverID := "c7389bd0e731f80f49593e5ee53835475f4e28594dd6bd83eb229bab753498cd"
	args := `{"t":{"t":9,"i":0,"l":1,"a":[{"t":1,"s":"wrk_01KSQKC93GMP038V8J9RBEFM8D"}],"o":0},"f":31,"m":[]}`
	url := fmt.Sprintf("https://opencode.ai/_server?id=%s&args=%s", serverID, url.QueryEscape(args))

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Cookie", "auth="+token)
	req.Header.Set("X-Server-Id", serverID)
	req.Header.Set("X-Server-Instance", "server-fn:8")

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("opencode: request failed: %v", err)
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}

	text := string(body)

	// Parse rollingUsage, weeklyUsage, monthlyUsage
	type usageEntry struct {
		label  string
		pct    float64
		resetS int
	}
	reUsage := regexp.MustCompile(`(rollingUsage|weeklyUsage|monthlyUsage).*?resetInSec:(\d+).*?usagePercent:(\d+(?:\.\d+)?)`)
	matches := reUsage.FindAllStringSubmatch(text, -1)

	if len(matches) == 0 {
		return nil
	}

	var metrics []Metric
	var nearestReset int64

	nameMap := map[string]string{"rollingUsage": "5h rolling", "weeklyUsage": "Weekly", "monthlyUsage": "Monthly"}
	for _, m := range matches {
		var resetS int
		fmt.Sscanf(m[2], "%d", &resetS)
		var pct float64
		fmt.Sscanf(m[3], "%f", &pct)

		metrics = append(metrics, Metric{Label: nameMap[m[1]], Pct: pct})

		if nearestReset == 0 || int64(resetS) < nearestReset {
			nearestReset = int64(resetS)
		}
	}

	var resetStr string
	if nearestReset > 0 {
		resetStr = formatResetTime(time.Now().Unix() + nearestReset)
	}

	return &ProviderUsage{
		Name:    "OpenCode",
		Metrics: metrics,
		ResetIn: resetStr,
	}
}

// ── Z.ai (API) ──────────────────────────────────────────────

type zaiLimit struct {
	Type          string  `json:"type"`
	Percentage    float64 `json:"percentage"`
	NextResetTime int64   `json:"nextResetTime"`
}

type zaiResponse struct {
	Data struct {
		Limits []zaiLimit `json:"limits"`
	} `json:"data"`
}

func fetchZaiUsage() *ProviderUsage {
	token := os.Getenv("ZAI_TOKEN")
	if token == "" {
		token = (&DoToken{}).GetSettings().ZaiToken
	}
	if token == "" {
		return nil
	}

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", "https://z.ai/api/monitor/usage/quota/limit", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("z.ai: request failed: %v", err)
		return nil
	}
	defer resp.Body.Close()

	var result zaiResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("z.ai: decode failed: %v", err)
		return nil
	}

	var metrics []Metric
	var resetEpoch int64
	var nearestReset time.Duration

	for _, limit := range result.Data.Limits {
		label := limit.Type
		if limit.Type == "TIME_LIMIT" {
			label = "Queries"
		} else if limit.Type == "TOKENS_LIMIT" {
			label = "Tokens"
		}

		metrics = append(metrics, Metric{Label: label, Pct: limit.Percentage})

		resetTime := time.UnixMilli(limit.NextResetTime).UTC()
		until := time.Until(resetTime)
		if until > 0 && (nearestReset == 0 || until < nearestReset) {
			nearestReset = until
			resetEpoch = limit.NextResetTime / 1000
		}
	}

	if len(metrics) == 0 {
		return nil
	}

	var resetIn string
	if resetEpoch > 0 {
		resetIn = formatResetTime(resetEpoch)
	}

	return &ProviderUsage{
		Name:       "Z.ai",
		Metrics:    metrics,
		ResetIn:    resetIn,
		ResetEpoch: resetEpoch,
	}
}

// ── Helpers ─────────────────────────────────────────────────

func formatResetTime(epoch int64) string {
	if epoch == 0 {
		return ""
	}
	t := time.Unix(epoch, 0).Local()
	now := time.Now().Local()

	// Same day
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("3:04pm")
	}

	// Different day
	return t.Format("Jan _2 at 3:04pm")
}

// ── Main ────────────────────────────────────────────────────

func main() {
	app := application.New(application.Options{
		Name:        "DoToken",
		Description: "AI token usage monitor",
		Services: []application.Service{
			application.NewService(&DoToken{}),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ActivationPolicy: application.ActivationPolicyAccessory,
		},
	})

	appWindow = app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:     "DoToken",
		Width:     300,
		Height:    400,
		Frameless: true,
		Mac: application.MacWindow{
			TitleBar: application.MacTitleBarHidden,
			Backdrop: application.MacBackdropTransparent,
		},
		BackgroundColour: application.NewRGB(0, 0, 0),
		Hidden:           true,
		AlwaysOnTop:      true,
		URL:              "/",
		Windows: application.WindowsWindow{
			HiddenOnTaskbar: true,
		},
	})

	// System tray
	tray := app.SystemTray.New()

	tray.SetIcon(iconData)

	tray.AttachWindow(appWindow).WindowOffset(5)

	menu := app.NewMenu()
	menu.Add("Quit").OnClick(func(ctx *application.Context) {
		app.Quit()
	})
	tray.SetMenu(menu)

	err := app.Run()
	if err != nil {
		log.Fatal(err)
	}
}
