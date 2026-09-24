package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The OpenCode console exposes per-model usage aggregation behind a console
// session cookie (__Host-console_session) plus an org id (x-org-id), which is
// discoverable via /console/api/orgs. Endpoints and field names were verified
// live against the console:
//
//	GET https://opencode.ai/console/api/orgs
//	GET https://opencode.ai/console/api/usage/models?range=<24h|7d|30d|all>&pageSize=100
//	GET https://opencode.ai/console/api/usage/summary?range=<same>
//
// Numeric values arrive as JSON strings; cost is micro-cents (1e8 = 1 USD).
const (
	consoleAPIBase      = "https://opencode.ai/console/api"
	usageDetailDefault  = "all"
	defaultModelsEvery  = 30 * time.Minute
)

// ModelUsageRow is one per-model usage row from the console usage API.
type ModelUsageRow struct {
	Model             string  `json:"model"`
	Provider          string  `json:"provider,omitempty"`
	TotalRequests     int64   `json:"total_requests"`
	TotalInputTokens  int64   `json:"input_tokens"`
	TotalOutputTokens int64   `json:"output_tokens"`
	TotalCacheRead    int64   `json:"cache_read_tokens"`
	TotalCacheWrite   int64   `json:"cache_write_tokens"`
	TotalCostUSD      float64 `json:"cost_usd"`
}

// ModelUsageSummary aggregates the whole account for the selected range.
type ModelUsageSummary struct {
	TotalRequests     int64   `json:"total_requests"`
	TotalInputTokens  int64   `json:"input_tokens"`
	TotalOutputTokens int64   `json:"output_tokens"`
	TotalCostUSD      float64 `json:"cost_usd"`
}

// flexInt accepts JSON numbers as well as numeric strings.
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*f = flexInt(v)
	return nil
}

type modelUsageAPIRow struct {
	Model                string  `json:"model"`
	Provider             string  `json:"provider"`
	TotalRequests        flexInt `json:"totalRequests"`
	TotalInputTokens     flexInt `json:"totalInputTokens"`
	TotalOutputTokens    flexInt `json:"totalOutputTokens"`
	TotalCacheReadTokens flexInt `json:"totalCacheReadTokens"`
	TotalCacheWrite5m    flexInt `json:"totalCacheWrite5mTokens"`
	TotalCacheWrite1h    flexInt `json:"totalCacheWrite1hTokens"`
	TotalCostMicroCents  flexInt `json:"totalCostMicroCents"`
}

type usageDetailAPIResp struct {
	Items []modelUsageAPIRow `json:"items"`
	Rows  []modelUsageAPIRow `json:"rows"`
	Data  []modelUsageAPIRow `json:"data"`
}

func (r *usageDetailAPIResp) rowList() []modelUsageAPIRow {
	for _, list := range [][]modelUsageAPIRow{r.Items, r.Rows, r.Data} {
		if len(list) > 0 {
			return list
		}
	}
	return nil
}

type usageSummaryFields struct {
	TotalRequests       flexInt `json:"totalRequests"`
	TotalInputTokens    flexInt `json:"totalInputTokens"`
	TotalOutputTokens   flexInt `json:"totalOutputTokens"`
	TotalCostMicroCents flexInt `json:"totalCostMicroCents"`
}

type summaryAPIResp struct {
	Summary usageSummaryFields `json:"summary"`
	usageSummaryFields
}

func microCentsToUSD(m flexInt) float64 {
	return float64(m) / 1e8
}

// usageDetailCookie reads the effective console cookie for one account.
// Caller must hold p.mu. Accepts a raw __Host-console_session value (st_…),
// a pasted "name=value; name=value" cookie string, or the legacy auth value.
// The console API rejects mixed cookie sets (403), so when a console session
// cookie is present it is used exclusively.
func (p *pool) usageDetailCookie(acct *account) string {
	_, cookie, cookieFile := p.dashboardCredentials(acct)
	if cookie == "" && cookieFile != "" {
		raw, err := os.ReadFile(cookieFile)
		if err != nil {
			return ""
		}
		cookie = string(raw)
	}
	cookie = strings.TrimSpace(cookie)
	if cookie == "" {
		return ""
	}
	for _, seg := range strings.Split(cookie, ";") {
		seg = strings.TrimSpace(seg)
		if name, _, found := strings.Cut(seg, "="); found {
			if name == "__Host-console_session" || name == "console_session" {
				return seg
			}
		}
	}
	cookie = strings.TrimPrefix(cookie, "auth=")
	if strings.HasPrefix(cookie, "st_") {
		return "__Host-console_session=" + cookie
	}
	if strings.Contains(cookie, "=") {
		// Already a cookie header value (auth only).
		return cookie
	}
	return "auth=" + cookie
}

// consoleOrgID resolves the org used for usage queries. An explicitly
// configured workspace wins ONLY when the session actually belongs to it;
// otherwise we refuse rather than attribute another org's usage to this
// account. The pool lock must NOT be held.
func consoleOrgID(cookie, workspaceID string) (string, error) {
	body, err := consoleGet(cookie, "", "/orgs")
	if err != nil {
		return "", err
	}
	var orgs []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &orgs); err != nil {
		return "", fmt.Errorf("orgs parse: %w", err)
	}
	if len(orgs) == 0 {
		return "", fmt.Errorf("no orgs for session")
	}
	ids := make([]string, 0, len(orgs))
	for _, o := range orgs {
		ids = append(ids, o.ID)
		if workspaceID != "" && o.ID == workspaceID {
			return workspaceID, nil
		}
	}
	if workspaceID != "" {
		return "", fmt.Errorf("session orgs (%s) do not include %s", strings.Join(ids, ", "), workspaceID)
	}
	return orgs[0].ID, nil
}

// consoleGet performs one authenticated console API GET. orgID is sent as the
// x-org-id header when non-empty. The pool lock must NOT be held.
func consoleGet(cookie, orgID, path string) ([]byte, error) {
	headers := http.Header{
		"Cookie":     []string{cookie},
		"Accept":     []string{"application/json"},
		"User-Agent": []string{dashboardUserAgent},
	}
	if orgID != "" {
		headers.Set("x-org-id", orgID)
	}
	resp, errDo := hostHTTPDo(pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     consoleAPIBase + path,
		Headers: headers,
	})
	if errDo != nil {
		return nil, errDo
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// refreshAccountModels fetches per-model usage detail for one account via the
// console API. It is best-effort: without a configured (and live) console
// session cookie the detail stays empty and quota windows keep working through
// the API-key path. Returns true when fresh rows were stored.
func refreshAccountModels(p *pool, acct *account, rng string) bool {
	p.mu.Lock()
	cookie := p.usageDetailCookie(acct)
	workspaceID := acct.WorkspaceID
	if ui, ok := p.uiSettings[identityKey(acct)]; ok && ui.WorkspaceID != "" {
		workspaceID = ui.WorkspaceID
	}
	p.mu.Unlock()
	if cookie == "" {
		return false
	}
	orgID, errOrg := consoleOrgID(cookie, workspaceID)
	if errOrg != nil {
		applyUsageDetailError(p, acct, "console org lookup: "+errOrg.Error())
		return false
	}
	bodyModels, errModels := consoleGet(cookie, orgID, "/usage/models?range="+rng+"&pageSize=100")
	if errModels != nil {
		applyUsageDetailError(p, acct, "usage detail fetch: "+errModels.Error())
		return false
	}
	var payload usageDetailAPIResp
	if errParse := json.Unmarshal(bodyModels, &payload); errParse != nil {
		applyUsageDetailError(p, acct, "usage detail parse: "+errParse.Error())
		return false
	}
	rows := payload.rowList()
	if len(rows) == 0 {
		applyUsageDetailError(p, acct, "usage detail returned no rows")
		return false
	}
	out := make([]ModelUsageRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, ModelUsageRow{
			Model:             r.Model,
			Provider:          r.Provider,
			TotalRequests:     int64(r.TotalRequests),
			TotalInputTokens:  int64(r.TotalInputTokens),
			TotalOutputTokens: int64(r.TotalOutputTokens),
			TotalCacheRead:    int64(r.TotalCacheReadTokens),
			TotalCacheWrite:   int64(r.TotalCacheWrite5m + r.TotalCacheWrite1h),
			TotalCostUSD:      microCentsToUSD(r.TotalCostMicroCents),
		})
	}
	var summary ModelUsageSummary
	if bodySummary, errSummary := consoleGet(cookie, orgID, "/usage/summary?range="+rng); errSummary == nil {
		var sp summaryAPIResp
		if json.Unmarshal(bodySummary, &sp) == nil {
			s := sp.Summary
			if s.TotalRequests == 0 {
				s = sp.usageSummaryFields
			}
			summary = ModelUsageSummary{
				TotalRequests:     int64(s.TotalRequests),
				TotalInputTokens:  int64(s.TotalInputTokens),
				TotalOutputTokens: int64(s.TotalOutputTokens),
				TotalCostUSD:      microCentsToUSD(s.TotalCostMicroCents),
			}
		}
	}
	now := time.Now()
	p.mu.Lock()
	st := p.stateFor(acct)
	st.ModelUsage = out
	st.ModelUsageSummary = summary
	st.ModelUsageRange = rng
	st.ModelUsageRefreshedAt = now
	st.ModelUsageError = ""
	p.mu.Unlock()
	return true
}

func applyUsageDetailError(p *pool, acct *account, message string) {
	p.mu.Lock()
	st := p.stateFor(acct)
	st.ModelUsageError = message
	p.mu.Unlock()
	hostLog("warn", "model usage refresh failed", map[string]any{"account": acct.Name, "error": message})
}

// usageDetailRangeOrDefault validates a configured range against the values
// accepted by the console API.
func usageDetailRangeOrDefault(raw string) string {
	switch strings.TrimSpace(raw) {
	case "24h", "7d", "30d", "all":
		return strings.TrimSpace(raw)
	default:
		return usageDetailDefault
	}
}

// modelsDue reports whether the model usage detail of one account should be
// refreshed now. Caller must hold p.mu.
func modelsDue(st *accountState, interval time.Duration, now time.Time) bool {
	if interval <= 0 {
		interval = defaultModelsEvery
	}
	return st.ModelUsageRefreshedAt.IsZero() || now.Sub(st.ModelUsageRefreshedAt) >= interval
}
