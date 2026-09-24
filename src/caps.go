package main

import "strings"

// Official OpenCode Go plan caps, from the official billing table
// (https://opencode.ai/docs/go, snapshot 2026-09-24).
//
// Usage limits are monthly dollar amounts per model; the 5-hour window is 20%
// of the monthly limit and the weekly limit is 50%. The console usage API
// reports spend at exactly these token prices (verified live: glm-5.3-flash
// hand-computed $0.014732 == console totalCostMicroCents/1e8), so caps minus
// month-to-date cost is the official remaining quota.
//
// ponytail: static snapshot of the upstream pricing table; re-check
// https://opencode.ai/docs/go when models/promos change (DeepSeek 4x promo
// ends 2026-09-27, then v4.1-flash returns to $15).

type modelCap struct {
	LimitUSD     float64 // official monthly dollar limit
	Unlimited    bool    // e.g. free promo models
	Known        bool
}

var modelCaps = map[string]modelCap{
	"glm-5.3-flash":                 {60, false, true},
	"glm-5.3":                       {15, false, true},
	"glm-5.2":                       {60, false, true},
	"glm-5.1":                       {60, false, true},
	"kimi-k3":                       {15, false, true},
	"kimi-k2.7-code":                {60, false, true},
	"kimi-k2.6":                     {60, false, true},
	"longcat-2.0":                   {60, false, true},
	"mimo-v2.6-flash":               {60, false, true},
	"mimo-v2.6-pro":                 {15, false, true},
	"mimo-v2.5":                     {60, false, true},
	"mimo-v2.5-pro":                 {15, false, true},
	"minimax-m3":                    {60, false, true},
	"minimax-m2.7":                  {60, false, true},
	"minimax-m2.5":                  {60, false, true},
	"muse-spark-1.3-contributor":    {60, false, true},
	"muse-spark-1.2-contributor":    {60, false, true},
	"qwen3.8-max":                   {15, false, true},
	"qwen3.8-flash":                 {30, false, true},
	"qwen3.7-max":                   {30, false, true},
	"qwen3.7-plus":                  {60, false, true},
	"qwen3.6-plus":                  {60, false, true},
	"deepseek-v4.1-flash":           {60, false, true}, // 4x promo until 2026-09-27, base $15
	"deepseek-v4-pro":               {15, false, true},
	"deepseek-v4-flash":             {30, false, true},
	"deepseek-v4-flash-vision-exp":  {15, false, true},
	"hy4-preview":                   {30, false, true},
	"hy3":                           {60, false, true},
	"space-bunny-free":              {0, true, true},
	"grok-4.7":                      {15, false, true},
	"grok-4.6":                      {15, false, true},
	"gpt-6-luna":                    {15, false, true},
	"gpt-5.6-luna":                  {15, false, true},
}

// capFor returns the official monthly cap for a console model name, plus the
// configured override when present.
func capFor(model string, overrides map[string]float64) modelCap {
	// Per-model tier rows in the official table (context-length variants)
	// share one cap; match by prefix before falling back to unknown.
	if cap, ok := modelCaps[strings.ToLower(model)]; ok {
		return cap
	}
	for name, cap := range modelCaps {
		if strings.HasPrefix(model, name) {
			return cap
		}
	}
	if cap, ok := overrides[strings.ToLower(model)]; ok {
		return modelCap{cap, false, true}
	}
	return modelCap{}
}
