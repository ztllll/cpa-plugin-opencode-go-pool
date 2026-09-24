package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The OpenCode console exposes per-model usage aggregation behind a console
// session cookie. Endpoints observed in the console SPA bundle:
//
//	GET https://opencode.ai/api/usage/models?range=<24h|7d|30d|all>
//	GET https://opencode.ai/api/usage/summary?range=<same>
//
// Model rows carry camelCase fields (model, provider, totalRequests,
// totalInputTokens, totalOutputTokens, totalCacheReadTokens,
// totalCacheWrite5m/1hTokens, totalCostMicroCents). Money is reported in
// micro-cents (1e6 micro-cents = 1 cent, 1e8 = 1 USD).
const (
	usageDetailBase    = "https://opencode.ai/api/usage"
	usageDetailDefault = "7d"
	defaultModelsEvery = 30 * time.Minute
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

type modelUsageAPIRow struct {
	Model                string  `json:"model"`
	Provider             string  `json:"provider"`
	TotalRequests        int64   `json:"totalRequests"`
	TotalInputTokens     int64   `json:"totalInputTokens"`
	TotalOutputTokens    int64   `json:"totalOutputTokens"`
	TotalCacheReadTokens int64   `json:"totalCacheReadTokens"`
	TotalCacheWrite5m    int64   `json:"totalCacheWrite5mTokens"`
	TotalCacheWrite1h    int64   `json:"totalCacheWrite1hTokens"`
	TotalCostMicroCents  float64 `json:"totalCostMicroCents"`
}

type usageDetailAPIResp struct {
	Models []modelUsageAPIRow `json:"models"`
	Items  []modelUsageAPIRow `json:"items"`
	Rows   []modelUsageAPIRow `json:"rows"`
	Data   []modelUsageAPIRow `json:"data"`
}

// rows returns the first populated row slice from any known envelope, or nil.
func (r *usageDetailAPIResp) rows() []modelUsageAPIRow {
	for _, list := range [][]modelUsageAPIRow{r.Models, r.Items, r.Rows, r.Data} {
		if len(list) > 0 {
			return list
		}
	}
	return nil
}

type usageSummaryFields struct {
	TotalRequests       int64   `json:"totalRequests"`
	TotalInputTokens    int64   `json:"totalInputTokens"`
	TotalOutputTokens   int64   `json:"totalOutputTokens"`
	TotalCostMicroCents float64 `json:"totalCostMicroCents"`
}

type summaryAPIResp struct {
	Summary usageSummaryFields `json:"summary"`
	usageSummaryFields
}

func microCentsToUSD(m float64) float64 {
	return m / 1e8
}

// usageDetailCookie reads the effective cookie for one account. Caller must
// hold p.mu. Returns "" when the account has no usable cookie.
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
	return strings.TrimPrefix(cookie, "auth=")
}

// fetchUsageDetailJSON performs one authenticated console API GET. The pool
// lock must NOT be held.
func fetchUsageDetailJSON(cookie, path string) ([]byte, error) {
	resp, errDo := hostHTTPDo(pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    usageDetailBase + path,
		Headers: http.Header{
			"Cookie":     []string{"auth=" + cookie},
			"Accept":     []string{"application/json"},
			"User-Agent": []string{dashboardUserAgent},
		},
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
// cookie the detail stays empty and quota windows keep working through the
// API-key path. Returns true when fresh rows were stored.
func refreshAccountModels(p *pool, acct *account, rng string) bool {
	p.mu.Lock()
	cookie := p.usageDetailCookie(acct)
	p.mu.Unlock()
	if cookie == "" {
		return false
	}
	bodyModels, errModels := fetchUsageDetailJSON(cookie, "/models?range="+rng)
	if errModels != nil {
		applyUsageDetailError(p, acct, "usage detail fetch: "+errModels.Error())
		return false
	}
	var payload usageDetailAPIResp
	if errParse := json.Unmarshal(bodyModels, &payload); errParse != nil {
		applyUsageDetailError(p, acct, "usage detail parse: "+errParse.Error())
		return false
	}
	rows := payload.rows()
	if rows == nil {
		// Bare-array envelope.
		_ = json.Unmarshal(bodyModels, &payload.Models)
		rows = payload.Models
	}
	if len(rows) == 0 {
		applyUsageDetailError(p, acct, "usage detail returned no rows")
		return false
	}
	out := make([]ModelUsageRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, ModelUsageRow{
			Model:             r.Model,
			Provider:          r.Provider,
			TotalRequests:     r.TotalRequests,
			TotalInputTokens:  r.TotalInputTokens,
			TotalOutputTokens: r.TotalOutputTokens,
			TotalCacheRead:    r.TotalCacheReadTokens,
			TotalCacheWrite:   r.TotalCacheWrite5m + r.TotalCacheWrite1h,
			TotalCostUSD:      microCentsToUSD(r.TotalCostMicroCents),
		})
	}
	var summary ModelUsageSummary
	if bodySummary, errSummary := fetchUsageDetailJSON(cookie, "/summary?range="+rng); errSummary == nil {
		var sp summaryAPIResp
		if json.Unmarshal(bodySummary, &sp) == nil {
			s := sp.Summary
			if s.TotalRequests == 0 && sp.TotalRequests != 0 {
				s = sp.usageSummaryFields
			}
			summary = ModelUsageSummary{
				TotalRequests:     s.TotalRequests,
				TotalInputTokens:  s.TotalInputTokens,
				TotalOutputTokens: s.TotalOutputTokens,
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
