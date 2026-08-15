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

// Model slugs on OpenRouter, grouped by family (verified 2026-08-15).
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
	GLMCoder    = "z-ai/glm-5.1"       // major coding-capability leap
	GLMFlagship = "z-ai/glm-5.2"       // large-scale reasoning, 1M ctx
	GLMBase     = "z-ai/glm-5"         // flagship general/base
	GLMCheap    = "z-ai/glm-4.7-flash" // 30B-class cheap bulk

	// Google (Gemini + Gemma)
	GoogleCode  = "google/gemini-3.5-flash"      // strong agentic all-rounder, 1M ctx
	GoogleAsk   = "google/gemini-3.1-flash-lite" // fast + cheap, 1M ctx
	GoogleCheap = "google/gemma-4-26b-a4b-it"    // open-weight cheap bulk

	// Grok (xAI)
	GrokFlagship = "x-ai/grok-4.3"              // general workhorse, 1M ctx
	GrokReason   = "x-ai/grok-4.20"             // reasoning flagship, 2M ctx
	GrokCode     = "x-ai/grok-build-0.1"        // coding-focused ("build")
	GrokAgentic  = "x-ai/grok-4.20-multi-agent" // agentic, multi-agent

	// DeepSeek (MIT open weights, ~1M ctx)
	DeepSeekCoder  = "deepseek/deepseek-v4-pro"   // best all-rounder per dollar
	DeepSeekReason = "deepseek/deepseek-r1-0528"  // dedicated reasoner
	DeepSeekCheap  = "deepseek/deepseek-v4-flash" // cheap bulk

	// Qwen (long context; refreshed to the 3.7/3.8 generation 2026-08-15)
	QwenCoder    = "qwen/qwen3-coder-next"   // newest coder line, 262K ctx, cheaper than qwen3-coder
	QwenReason   = "qwen/qwen3-max-thinking" // dedicated reasoner
	QwenFlagship = "qwen/qwen3.7-plus"       // balanced general workhorse, 1M ctx
	QwenCheap    = "qwen/qwen3.7-flash"      // very cheap bulk, 1M ctx
	// QwenMax is premium-priced ($2/$6 per M) — like ModelK3, deliberately NOT in
	// any default role route; pin it explicitly with -m for frontier-tier tasks.
	QwenMax = "qwen/qwen3.8-max" // frontier flagship, 1M ctx

	// MiniMax (cheapest credible coding tier)
	MiniMaxCoder    = "minimax/minimax-m2.5" // frontier-class coding at low price
	MiniMaxReason   = "minimax/minimax-m2.1" // reasoning
	MiniMaxFlagship = "minimax/minimax-m2.7" // general

	// Mistral
	MistralCoder    = "mistralai/codestral-2508"                 // coding
	MistralFlagship = "mistralai/mistral-large-2512"             // general + reasoning
	MistralCheap    = "mistralai/mistral-small-3.2-24b-instruct" // cheap bulk
)

// pricing holds [input, output] USD per token for cost estimation.
var pricing = map[string][2]float64{
	ModelFlagship: {0.000000684, 0.00000342},
	ModelCoder:    {0.00000085, 0.0000038},
	ModelThinking: {0.0000006, 0.0000025},
	ModelAgentic:  {0.0000006, 0.0000025},
	ModelCheap:    {0.00000057, 0.00000285},
	ModelK3:       {0.000003, 0.000015},

	GLMCoder:    {0.000000966, 0.000003036},
	GLMFlagship: {0.000000962, 0.000003023},
	GLMBase:     {0.00000095, 0.00000255},
	GLMCheap:    {0.000000061, 0.0000004},

	GoogleCode:  {0.0000015, 0.000009},
	GoogleAsk:   {0.00000025, 0.0000015},
	GoogleCheap: {0.00000007, 0.00000034},

	GrokFlagship: {0.00000125, 0.0000025},
	GrokReason:   {0.00000125, 0.0000025},
	GrokCode:     {0.000001, 0.000002},
	GrokAgentic:  {0.00000125, 0.0000025},

	DeepSeekCoder:  {0.000000435, 0.00000087},
	DeepSeekReason: {0.0000005, 0.00000215},
	DeepSeekCheap:  {0.000000098, 0.000000196},

	QwenCoder:    {0.00000012, 0.0000008},
	QwenReason:   {0.00000078, 0.0000039},
	QwenFlagship: {0.00000032, 0.00000128},
	QwenCheap:    {0.00000003, 0.00000013},
	QwenMax:      {0.000002, 0.000006},

	MiniMaxCoder:    {0.00000015, 0.0000009},
	MiniMaxReason:   {0.0000003, 0.0000012},
	MiniMaxFlagship: {0.00000025, 0.000001},

	MistralCoder:    {0.0000003, 0.0000009},
	MistralFlagship: {0.0000005, 0.0000015},
	MistralCheap:    {0.0000001, 0.0000003},
}

var pricingMu sync.RWMutex

// WebSearchModel is the grounded web-search backend for the web_search tool:
// Perplexity Sonar returns current, cited answers (message.annotations) at ~$0.005/query.
const WebSearchModel = "perplexity/sonar"

// PriceRank orders models by list price for the cost-ladder router: the sum of
// per-token input+output prices (0 for :free variants — they sort first). Models
// with no pricing data rank LAST (never preferred over a known-price model).
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
		if in > 0 || comp > 0 {
			pricing[m.ID] = [2]float64{in, comp}
		}
	}
	return nil
}
