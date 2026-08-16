package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/imhassla/open-agent/internal/budget"
	"github.com/imhassla/open-agent/internal/llm"
)

// compactKeepRecent is how many trailing messages to preserve verbatim.
const compactKeepRecent = 6

// compactModel is the cheap model used to summarize (family-specific when set).
func (a *Agent) compactModel() string {
	if a.CompactModel != "" {
		return a.CompactModel
	}
	return llm.ModelCheap
}

// compact summarizes older conversation turns into a single message via the cheap
// model, preserving the system prompt and the most recent turns. It only ever
// removes COMPLETE turns and never splits a tool_call from its tool_result (which
// would make the next request 400): the kept tail is walked back so it never
// starts on an orphaned tool message.
func (a *Agent) compact(ctx context.Context, bud *budget.Budget) error {
	if len(a.msgs) < compactKeepRecent+3 {
		return nil // too short to bother
	}

	// The preamble (date, working directory, fault hints) must SURVIVE compaction
	// verbatim: summarizing it away silently re-enables the exact failure modes it
	// exists to prevent (hallucinated absolute paths, training-cutoff anchoring)
	// for every step after the first compaction of a long run.
	head := 1 // system
	if a.Preamble != "" && len(a.msgs) > 1 && a.msgs[1].Role == "user" && a.msgs[1].Content == a.Preamble {
		head = 2 // system + preamble
	}

	keepFrom := len(a.msgs) - compactKeepRecent
	for keepFrom > head && a.msgs[keepFrom].Role == "tool" {
		keepFrom-- // never start the tail on a tool result (its tool_call must precede it)
	}
	if keepFrom <= head {
		return nil
	}

	middle := a.msgs[head:keepFrom]
	if len(middle) == 0 {
		return nil
	}

	var sb strings.Builder
	for _, m := range middle {
		content := m.Content
		if len(m.ToolCalls) > 0 {
			names := make([]string, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				names = append(names, tc.Function.Name)
			}
			content = "(called tools: " + strings.Join(names, ", ") + ") " + content
		}
		if strings.TrimSpace(content) == "" {
			continue
		}
		fmt.Fprintf(&sb, "%s: %s\n", m.Role, truncate(content, 2000))
	}

	resp, err := a.Client.Chat(ctx, []llm.Message{
		{Role: "system", Content: "Summarize the following agent transcript into a compact, " +
			"information-dense note that preserves all decisions, facts discovered, file paths, commands " +
			"run and their outcomes, and any open threads. Be concise but lossless on load-bearing detail."},
		{Role: "user", Content: sb.String()},
	}, llm.ChatOptions{Model: a.compactModel(), MaxTokens: 2048})
	if err != nil {
		return err
	}

	// Account for the summarization call against totals and the shared budget —
	// compaction is the long-run path and its spend must not escape the ceiling.
	cost := resp.Usage.Cost
	if cost == 0 {
		cost = llm.CostUSD(a.compactModel(), resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	}
	a.TotalCost += cost
	a.TotalTokens += resp.Usage.TotalTokens
	if bud != nil {
		bud.Charge(resp.Usage.TotalTokens, cost)
	}

	summary := strings.TrimSpace(resp.Message.Content)
	if summary == "" {
		return fmt.Errorf("compaction produced an empty summary")
	}

	compacted := make([]llm.Message, 0, head+1+compactKeepRecent)
	compacted = append(compacted, a.msgs[:head]...) // system (+ preamble, verbatim)
	compacted = append(compacted, llm.Message{Role: "user", Content: "[Summary of earlier context]\n" + summary})
	compacted = append(compacted, a.msgs[keepFrom:]...)
	a.msgs = compacted
	return nil
}
