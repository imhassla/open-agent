package llm

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Model slugs on OpenRouter, grouped by family (verified 2026-09-04 against the
// live catalog).
const (
	// Kimi (Moonshot)
	ModelFlagship = "moonshotai/kimi-k2.6"        // workhorse: long-horizon coding + multi-agent
	ModelCoder    = "moonshotai/kimi-k2.7-code"   // coding-focused, end-to-end programming
	ModelThinking = "moonshotai/kimi-k2-thinking" // long-horizon reasoning
	ModelAgentic  = "moonshotai/kimi-k2-0905"     // agentic update
	ModelCheap    = "moonshotai/kimi-k2.5"        // cheapest + native multimodal
	// ModelK3 is premium-priced (~5× k2.7-code) — deliberately NOT in any default
	// role route; pin it explicitly with -m when a task warrants frontier reasoning.
	ModelK3 = "moonshotai/kimi-k3" // 2.8T multimodal reasoning flagship, 1M ctx

	// GLM (Z.AI / Zhipu)
	GLMCoder    = "z-ai/glm-5.3"       // open-weights SOTA coding (2026-08-18); pricier — bench-watch cost/task
	GLMFlagship = "z-ai/glm-5.2"       // large-scale reasoning, 1M ctx
	GLMBase     = "z-ai/glm-5"         // flagship general/base
	GLMCheap    = "z-ai/glm-5.3-flash" // cheap bulk, one generation ahead of 4.7-flash at ~same price

	// Google (Gemini + Gemma)
	GoogleCode  = "google/gemini-3.8-flash"      // long-horizon coding/agents at HALF 3.5-flash's price
	GoogleAsk   = "google/gemini-3.1-flash-lite" // fast + cheap, 1M ctx
	GoogleCheap = "google/gemma-4-26b-a4b-it"    // open-weight cheap bulk

	// Grok (xAI). grok-4.20-multi-agent was dropped: OpenRouter now reports
	// tools:false for it — unusable in a tool-calling loop.
	GrokFlagship = "x-ai/grok-4.3"       // general workhorse, 1M ctx
	GrokReason   = "x-ai/grok-4.20"      // reasoning flagship, 2M ctx
	GrokCode     = "x-ai/grok-build-0.1" // coding-focused ("build")

	// DeepSeek (MIT open weights, ~1M ctx). RUGPULL 2026-08-16: the -0813
	// snapshot became the GA release and was repriced to $1.12/$3.35 — the old
	// "snapshot keeps the cheap price" rationale is DEAD, and the bare v4-pro
	// alias ($1.04/$2.09) is now the cheaper of the two. Worker-tier coding
	// moved to v4-flash-0731 (~17× cheaper, re-post-trained for coding/agents);
	// v4-pro serves the reasoning-grade roles.
	DeepSeekCoder  = "deepseek/deepseek-v4-flash-0731" // worker-tier coding/agents
	DeepSeekReason = "deepseek/deepseek-v4-pro"        // reasoning-grade (alias; repoint risk accepted)
	DeepSeekCheap  = "deepseek/deepseek-v4-flash-0731" // cheap bulk

	// Qwen (long context; refreshed to the 3.7/3.8 generation 2026-08-15)
	QwenCoder    = "qwen/qwen3-coder-next"   // newest coder line, 262K ctx, cheaper than qwen3-coder
	QwenReason   = "qwen/qwen3-max-thinking" // dedicated reasoner
	QwenFlagship = "qwen/qwen3.7-plus"       // balanced general workhorse, 1M ctx
	QwenCheap    = "qwen/qwen3.7-flash"      // very cheap bulk, 1M ctx
	// QwenMax is premium-priced ($2/$6 per M) — like ModelK3, deliberately NOT in
	// any default role route; pin it explicitly with -m for frontier-tier tasks.
	QwenMax = "qwen/qwen3.8-max" // frontier flagship, 1M ctx

	// MiniMax. m2.5's 2026-08 reprice erased its price edge; M3 costs the same
	// as m2.7 with 1M ctx and a full bench generation of headroom (SWE-Bench
	// Pro 59.0). m2.1 stays as the planning prior (M3 planning latency unbenched).
	MiniMaxCoder    = "minimax/minimax-m3"   // coding + general
	MiniMaxReason   = "minimax/minimax-m2.1" // reasoning/planning prior
	MiniMaxFlagship = "minimax/minimax-m3"   // general (same slug as coder)

	// Mistral
	MistralCoder    = "mistralai/codestral-2508"                 // coding
	MistralFlagship = "mistralai/mistral-large-2512"             // general + reasoning
	MistralCheap    = "mistralai/mistral-small-3.2-24b-instruct" // cheap bulk

	// OpenAI (added 2026-09-04 after the ~80% price cut): one slug serves every
	// role — the luna tier is worker-priced with strong agentic benches
	// (TAU-Bench 70.7). terra/sol stay pin-only premium like kimi-k3/qwen3.8-max.
	// NOTE: luna accepts NO temperature/top_p (reasoning-model surface, verified
	// via supported_parameters) — adaptiveForModel must send nothing.
	OpenAILuna = "openai/gpt-5.6-luna"

	// ByteDance Seed (added 2026-09-04, PROVISIONAL — vendor-reported benches;
	// keep only if our own bench matrix validates it).
	SeedCoder = "bytedance-seed/seed-2.0-code" // agentic-coding line
	SeedTurbo = "bytedance-seed/seed-2-1-turbo"
	SeedMini  = "bytedance-seed/seed-2.0-mini"
)

// pricing holds [input, output] USD per token for cost estimation.
var pricing = map[string][2]float64{
	ModelFlagship: {0.00000095, 0.000004}, // repriced up 2026-09
	ModelCoder:    {0.00000066, 0.0000034},
	ModelThinking: {0.0000006, 0.0000025},
	ModelAgentic:  {0.0000006, 0.0000025},
	ModelCheap:    {0.00000045, 0.00000225},
	ModelK3:       {0.000003, 0.000015},

	GLMCoder:    {0.0000014, 0.0000044},
	GLMFlagship: {0.000000962, 0.000003023},
	GLMBase:     {0.0000006, 0.00000192},
	GLMCheap:    {0.000000075, 0.00000025},

	GoogleCode:  {0.00000075, 0.00000375},
	GoogleAsk:   {0.00000025, 0.0000015},
	GoogleCheap: {0.00000007, 0.00000034},

	GrokFlagship: {0.00000125, 0.0000025},
	GrokReason:   {0.00000125, 0.0000025},
	GrokCode:     {0.000001, 0.000002},

	DeepSeekCoder:  {0.000000065, 0.00000018},
	DeepSeekReason: {0.000001042, 0.000002085},

	QwenCoder:    {0.00000012, 0.0000008},
	QwenReason:   {0.00000078, 0.0000039},
	QwenFlagship: {0.00000032, 0.00000128},
	QwenCheap:    {0.00000003, 0.00000013},
	QwenMax:      {0.000002, 0.000006},

	MiniMaxCoder:  {0.0000003, 0.0000012}, // m3 (also MiniMaxFlagship — same slug)
	MiniMaxReason: {0.0000003, 0.0000012},

	MistralCoder:    {0.0000003, 0.0000009},
	MistralFlagship: {0.0000005, 0.0000015},
	MistralCheap:    {0.000000075, 0.0000002},

	OpenAILuna: {0.0000002, 0.0000012},

	SeedCoder: {0.0000005, 0.000003},
	SeedTurbo: {0.0000005, 0.0000025},
	SeedMini:  {0.0000001, 0.0000004},
}

var pricingMu sync.RWMutex

// WebSearchModel is the grounded web-search backend for the web_search tool:
// Perplexity Sonar returns current, cited answers (message.annotations) at ~$0.005/query.
const WebSearchModel = "perplexity/sonar"

// PriceRank orders models by list price for the cost-ladder router: the sum of
// per-token input+output prices. Models with no pricing data rank LAST (never
// preferred over a known-price model) — this includes :free variants, which are
// deliberately absent from the table since the ladder dropped the free tier;
// they are only reachable via an explicit -m pin, which bypasses ranking.
func PriceRank(model string) float64 {
	pricingMu.RLock()
	p, ok := pricing[model]
	pricingMu.RUnlock()
	if !ok {
		return math.MaxFloat64
	}
	return p[0] + p[1]
}

// OutputPricePerToken returns the per-token completion price (0 when unknown),
// for budget-driven completion clamps.
func OutputPricePerToken(model string) float64 {
	pricingMu.RLock()
	p, ok := pricing[model]
	pricingMu.RUnlock()
	if !ok {
		return 0
	}
	return p[1]
}

// CostUSD estimates request cost from token counts. Returns 0 for unknown models.
func CostUSD(model string, promptTokens, completionTokens int) float64 {
	pricingMu.RLock()
	p, ok := pricing[model]
	pricingMu.RUnlock()
	if !ok {
		return 0
	}
	return float64(promptTokens)*p[0] + float64(completionTokens)*p[1]
}

// KnownModel reports whether a slug has pricing metadata (i.e. is a recognized model).
func KnownModel(model string) bool {
	pricingMu.RLock()
	_, ok := pricing[model]
	pricingMu.RUnlock()
	return ok
}

// RefreshPricing pulls the live OpenRouter /models catalog and updates the pricing
// table so any routed slug (incl. --model overrides and newly-added models) is
// costed accurately instead of silently estimating $0. Best-effort: a fetch/parse
// error leaves the static table in place. Call once at startup before workers run.
func RefreshPricing(ctx context.Context, c *Client) error {
	url := strings.Replace(c.BaseURL, "/chat/completions", "/models", 1)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil // best-effort
	}
	var out struct {
		Data []struct {
			ID      string `json:"id"`
			Pricing struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	pricingMu.Lock()
	defer pricingMu.Unlock()
	for _, m := range out.Data {
		in, _ := strconv.ParseFloat(m.Pricing.Prompt, 64)
		comp, _ := strconv.ParseFloat(m.Pricing.Completion, 64)
		// Zero-priced catalog rows are skipped ON PURPOSE: a glitched/free 0/0
		// entry must never clobber a real static price for a paid model. :free
		// slugs therefore stay out of the table (see PriceRank's contract).
		if in > 0 || comp > 0 {
			pricing[m.ID] = [2]float64{in, comp}
		}
	}
	return nil
}
