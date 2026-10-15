package skills

import (
	"sort"
	"strings"
)

// Inventory is the user's unfiltered view of available skills. It does not
// grant invocation eligibility or load procedures into a conversation.
type Inventory struct {
	Complete    bool             `json:"complete"`
	Skills      []InventoryEntry `json:"skills"`
	Diagnostics []Diagnostic     `json:"diagnostics"`
}

type InventoryEntry struct {
	Metadata
	UserOnly        bool `json:"user_only"`
	EstimatedTokens int  `json:"estimated_tokens"`
}

type Diagnostic struct {
	Category string   `json:"category"`
	Message  string   `json:"message"`
	Paths    []string `json:"paths"`
}

func (c *Catalog) Inventory() Inventory {
	result := Inventory{Complete: !c.incomplete, Skills: []InventoryEntry{}, Diagnostics: []Diagnostic{}}
	for _, name := range c.names() {
		if m, err := c.Resolve(name); err == nil {
			// Approximate UTF-8 bytes / 4, rounded up to ten tokens. This describes
			// metadata only; it is not a model tokenizer or an accounting figure.
			estimate := ((len(m.Name) + 1 + len(m.Description) + 39) / 40) * 10
			result.Skills = append(result.Skills, InventoryEntry{Metadata: m, UserOnly: m.UserOnly, EstimatedTokens: estimate})
		}
	}
	result.Diagnostics = append(result.Diagnostics, c.diagnostics...)
	sort.Slice(result.Diagnostics, func(i, j int) bool {
		a, b := result.Diagnostics[i], result.Diagnostics[j]
		if a.Category != b.Category {
			return a.Category < b.Category
		}
		if a.Message != b.Message {
			return a.Message < b.Message
		}
		return strings.Join(a.Paths, "\x00") < strings.Join(b.Paths, "\x00")
	})
	return result
}
