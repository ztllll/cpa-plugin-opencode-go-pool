package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const dashboardUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Gecko/20100101 Firefox/148.0"

// SolidJS SSR hydration patterns; field order varies between deploys.
var (
	ssrPctFirst = map[string]*regexp.Regexp{
		windowFiveHour: regexp.MustCompile(`rollingUsage:\$R\[\d+\]=\{[^}]*usagePercent:(-?\d+(?:\.\d+)?)[^}]*resetInSec:(-?\d+(?:\.\d+)?)[^}]*\}`),
		windowWeekly:   regexp.MustCompile(`weeklyUsage:\$R\[\d+\]=\{[^}]*usagePercent:(-?\d+(?:\.\d+)?)[^}]*resetInSec:(-?\d+(?:\.\d+)?)[^}]*\}`),
		windowMonthly:  regexp.MustCompile(`monthlyUsage:\$R\[\d+\]=\{[^}]*usagePercent:(-?\d+(?:\.\d+)?)[^}]*resetInSec:(-?\d+(?:\.\d+)?)[^}]*\}`),
	}
	ssrResetFirst = map[string]*regexp.Regexp{
		windowFiveHour: regexp.MustCompile(`rollingUsage:\$R\[\d+\]=\{[^}]*resetInSec:(-?\d+(?:\.\d+)?)[^}]*usagePercent:(-?\d+(?:\.\d+)?)[^}]*\}`),
		windowWeekly:   regexp.MustCompile(`weeklyUsage:\$R\[\d+\]=\{[^}]*resetInSec:(-?\d+(?:\.\d+)?)[^}]*usagePercent:(-?\d+(?:\.\d+)?)[^}]*\}`),
		windowMonthly:  regexp.MustCompile(`monthlyUsage:\$R\[\d+\]=\{[^}]*resetInSec:(-?\d+(?:\.\d+)?)[^}]*usagePercent:(-?\d+(?:\.\d+)?)[^}]*\}`),
	}
	slotLabelPattern   = regexp.MustCompile(`data-slot="usage-label">([^<]+)<`)
	slotValuePattern   = regexp.MustCompile(`data-slot="usage-value">[^0-9]*(\d+(?:\.\d+)?)`)
	slotResetPattern   = regexp.MustCompile(`data-slot="(reset-time|reset-now)">([\s\S]*?)</span>`)
	humanDayPattern    = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*days?`)
	humanHourPattern   = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*hours?`)
	humanMinPattern    = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*minutes?`)
	humanSecPattern    = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*seconds?`)
	resetPrefixPattern = regexp.MustCompile(`(?i)resets?\s*in\s*`)
)

var humanDurationParts = [...]struct {
	pattern *regexp.Regexp
	seconds float64
}{
	{humanDayPattern, 86400},
	{humanHourPattern, 3600},
	{humanMinPattern, 60},
	{humanSecPattern, 1},
}

type windowUsage struct {
	UsagePercent int
	ResetInSec   int64
}

// OpenCode Go exposes a stable JSON usage endpoint keyed by the account's own
// API key; it needs no dashboard cookie and survives upstream page redesigns.
const usageAPIURL = "https://opencode.ai/zen/go/v1/usage"
const usageAPIUserAgent = "opencode/1.0"

type usageAPIWindow struct {
	Status   string  `json:"status"`
	Percent  float64 `json:"percent"`
	ResetsAt string  `json:"resetsAt"`
}

type usageAPIResponse struct {
	Usage map[string]usageAPIWindow `json:"usage"`
}

var usageAPIWindowMap = map[string]string{
	"rolling": windowFiveHour,
	"weekly":  windowWeekly,
	"monthly": windowMonthly,
}

// refreshAccountViaAPI fetches quota via the /v1/usage API (API-key auth) and
// applies it the same way the dashboard path does. It returns false when the
// account has no API key or the API call cannot be used, so the caller falls
// back to the dashboard (cookie) path. The pool lock must NOT be held.
func refreshAccountViaAPI(p *pool, acct *account) bool {
	p.mu.Lock()
	apiKey := acct.apiKey
	p.mu.Unlock()
	if apiKey == "" {
		return false
	}
	resp, errDo := hostHTTPDo(pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    usageAPIURL,
		Headers: http.Header{
			"Authorization": []string{"Bearer " + apiKey},
			"Accept":        []string{"application/json"},
			"User-Agent":    []string{usageAPIUserAgent},
		},
	})
	if errDo != nil {
		hostLog("warn", "usage API fetch failed, falling back to dashboard", map[string]any{"account": acct.Name, "error": errDo.Error()})
		return false
	}
	if resp.StatusCode != http.StatusOK {
		hostLog("warn", "usage API HTTP error, falling back to dashboard", map[string]any{"account": acct.Name, "status": resp.StatusCode})
		return false
	}
	var payload usageAPIResponse
	if errUnmarshal := json.Unmarshal(resp.Body, &payload); errUnmarshal != nil {
		hostLog("warn", "usage API parse failed, falling back to dashboard", map[string]any{"account": acct.Name, "error": errUnmarshal.Error()})
		return false
	}
	now := time.Now()
	p.mu.Lock()
	st := p.stateFor(acct)
	st.DashboardRefreshedAt = now
	st.DashboardError = ""
	applied := 0
	for window, usage := range payload.Usage {
		name, ok := usageAPIWindowMap[window]
		if !ok {
			continue
		}
		w := st.Windows[name]
		w.UsagePercent = atoiFloor(strconv.FormatFloat(usage.Percent, 'f', -1, 64))
		if resetAt, errParse := time.Parse(time.RFC3339, usage.ResetsAt); errParse == nil {
			w.ResetAt = resetAt
		} else {
			w.ResetAt = now
		}
		w.UpdatedAt = now
		// A fresh reading below threshold clears an earlier hard block: the
		// window has reset.
		if w.UsagePercent < p.cfg.ThresholdPercent && w.BlockedUntil.After(now) {
			w.BlockedUntil = time.Time{}
		}
		applied++
	}
	p.mu.Unlock()
	if applied == 0 {
		hostLog("warn", "usage API returned no known windows, falling back to dashboard", map[string]any{"account": acct.Name})
		return false
	}
	return true
}

// parseDashboardHTML extracts per-window usage from the workspace Go page,
// trying the SSR hydration format first and the data-slot markup second.
func parseDashboardHTML(html string) map[string]windowUsage {
	out := make(map[string]windowUsage)
	for _, name := range windowNames {
		if m := ssrPctFirst[name].FindStringSubmatch(html); len(m) == 3 {
			out[name] = windowUsage{UsagePercent: atoiFloor(m[1]), ResetInSec: atoi64(m[2])}
			continue
		}
		if m := ssrResetFirst[name].FindStringSubmatch(html); len(m) == 3 {
			out[name] = windowUsage{UsagePercent: atoiFloor(m[2]), ResetInSec: atoi64(m[1])}
		}
	}
	if len(out) == len(windowNames) {
		return out
	}

	// data-slot fallback format.
	items := strings.Split(html, `data-slot="usage-item"`)
	for i := 1; i < len(items); i++ {
		content := items[i]
		labelMatch := slotLabelPattern.FindStringSubmatch(content)
		if labelMatch == nil {
			continue
		}
		label := strings.ToLower(strings.TrimSpace(labelMatch[1]))
		var name string
		switch {
		case strings.Contains(label, "rolling"):
			name = windowFiveHour
		case strings.Contains(label, "weekly"):
			name = windowWeekly
		case strings.Contains(label, "monthly"):
			name = windowMonthly
		default:
			continue
		}
		if _, done := out[name]; done {
			continue
		}
		valueMatch := slotValuePattern.FindStringSubmatch(content)
		resetMatch := slotResetPattern.FindStringSubmatch(content)
		if valueMatch == nil || resetMatch == nil {
			continue
		}
		resetSec := int64(0)
		if resetMatch[1] == "reset-time" {
			cleaned := strings.NewReplacer("<!--$-->", "", "<!--/-->", "").Replace(resetMatch[2])
			cleaned = resetPrefixPattern.ReplaceAllString(cleaned, "")
			parsed, ok := parseHumanDuration(cleaned)
			if !ok {
				continue
			}
			resetSec = parsed
		}
		out[name] = windowUsage{UsagePercent: atoiFloor(valueMatch[1]), ResetInSec: resetSec}
	}
	return out
}

func parseHumanDuration(raw string) (int64, bool) {
	normalized := strings.ToLower(strings.Join(strings.Fields(raw), " "))
	switch normalized {
	case "reset-now", "reset now", "now", "resets now":
		return 0, true
	}
	total := float64(0)
	found := false
	for _, part := range humanDurationParts {
		if m := part.pattern.FindStringSubmatch(normalized); m != nil {
			v, _ := strconv.ParseFloat(m[1], 64)
			total += v * part.seconds
			found = true
		}
	}
	if !found {
		return 0, false
	}
	return int64(total), true
}

func atoiFloor(raw string) int {
	v, _ := strconv.ParseFloat(raw, 64)
	if v < 0 {
		return 0
	}
	return int(v)
}

func atoi64(raw string) int64 {
	v, _ := strconv.ParseFloat(raw, 64)
	if v < 0 {
		return 0
	}
	return int64(v)
}

// refreshAccount fetches and applies quota for one account, preferring the
// stable /v1/usage API (API key) and falling back to the dashboard page
// (cookie). The pool lock must NOT be held; results are applied under the
// lock afterwards.
func refreshAccount(p *pool, acct *account) {
	if refreshAccountViaAPI(p, acct) {
		return
	}
	p.mu.Lock()
	workspaceID, cookie, cookieFile := p.dashboardCredentials(acct)
	p.mu.Unlock()
	if workspaceID == "" {
		applyDashboardError(p, acct, "workspace ID is not configured")
		return
	}
	if cookie == "" && cookieFile != "" {
		cookieRaw, errRead := os.ReadFile(cookieFile)
		if errRead != nil {
			applyDashboardError(p, acct, "read cookie file: "+errRead.Error())
			return
		}
		cookie = string(cookieRaw)
	}
	cookie = strings.TrimSpace(cookie)
	if cookie == "" {
		applyDashboardError(p, acct, "auth cookie is not configured")
		return
	}
	if !strings.HasPrefix(cookie, "auth=") {
		cookie = "auth=" + cookie
	}

	url := "https://opencode.ai/workspace/" + workspaceID + "/go"
	resp, errDo := hostHTTPDo(pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    url,
		Headers: http.Header{
			"Cookie":     []string{cookie},
			"Accept":     []string{"text/html"},
			"User-Agent": []string{dashboardUserAgent},
		},
	})
	if errDo != nil {
		applyDashboardError(p, acct, "dashboard fetch: "+errDo.Error())
		return
	}
	if resp.StatusCode != http.StatusOK {
		applyDashboardError(p, acct, fmt.Sprintf("dashboard HTTP %d (cookie expired?)", resp.StatusCode))
		return
	}
	parsed := parseDashboardHTML(string(resp.Body))
	if len(parsed) == 0 {
		applyDashboardError(p, acct, "dashboard HTML parse failed (format changed?)")
		return
	}

	now := time.Now()
	p.mu.Lock()
	st := p.stateFor(acct)
	st.DashboardRefreshedAt = now
	st.DashboardError = ""
	for name, usage := range parsed {
		w := st.Windows[name]
		w.UsagePercent = usage.UsagePercent
		w.ResetAt = now.Add(time.Duration(usage.ResetInSec) * time.Second)
		w.UpdatedAt = now
		// A fresh dashboard reading below threshold clears an earlier hard
		// block: the window has reset.
		if usage.UsagePercent < p.cfg.ThresholdPercent && w.BlockedUntil.After(now) {
			w.BlockedUntil = time.Time{}
		}
	}
	p.mu.Unlock()
}

func applyDashboardError(p *pool, acct *account, message string) {
	p.mu.Lock()
	st := p.stateFor(acct)
	st.DashboardError = message
	p.mu.Unlock()
	hostLog("warn", "dashboard refresh failed", map[string]any{"account": acct.Name, "error": message})
}

// poller owns the background refresh goroutine. It is started idempotently on
// register/reconfigure and stopped on plugin shutdown.
type poller struct {
	mu      sync.Mutex
	stop    chan struct{}
	kick    chan struct{}
	running bool
}

var globalPoller = &poller{}

func startPoller() {
	globalPoller.mu.Lock()
	defer globalPoller.mu.Unlock()
	if globalPoller.running {
		return
	}
	globalPoller.stop = make(chan struct{})
	globalPoller.kick = make(chan struct{}, 1)
	globalPoller.running = true
	go pollLoop(globalPoller.stop, globalPoller.kick)
}

func stopPoller() {
	globalPoller.mu.Lock()
	defer globalPoller.mu.Unlock()
	if !globalPoller.running {
		return
	}
	close(globalPoller.stop)
	globalPoller.running = false
}

// kickPoller requests an immediate refresh pass (used by the management API).
func kickPoller() {
	globalPoller.mu.Lock()
	kick := globalPoller.kick
	running := globalPoller.running
	globalPoller.mu.Unlock()
	if running {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
}

func pollLoop(stop <-chan struct{}, kick <-chan struct{}) {
	var lastDashboard time.Time
	for {
		p := currentPool()
		refreshHostCooldowns(p)

		p.mu.Lock()
		interval := p.cfg.RefreshInterval
		if interval <= 0 {
			interval = defaultRefreshEvery
		}
		var targets []*account
		for _, acct := range p.accounts {
			workspaceID, cookie, cookieFile := p.dashboardCredentials(acct)
			// API-key accounts refresh via /v1/usage and need no dashboard
			// credentials; cookie accounts keep the dashboard path.
			if !acct.Disabled && (acct.apiKey != "" || (workspaceID != "" && (cookie != "" || cookieFile != ""))) {
				targets = append(targets, acct)
			}
		}
		p.mu.Unlock()

		if time.Since(lastDashboard) >= interval && len(targets) > 0 {
			lastDashboard = time.Now()
			for _, acct := range targets {
				refreshAccount(p, acct)
			}
		}

		select {
		case <-stop:
			return
		case <-kick:
			lastDashboard = time.Time{}
		case <-time.After(cooldownScanInterval):
		}
	}
}
