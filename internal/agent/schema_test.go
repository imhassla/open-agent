package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestToolSchemasNoNullRequired guards against tool parameter schemas that marshal
// "required": null — strict providers (xAI/grok) reject those with HTTP 400,
// hard-failing the run. Every CoreTools schema must emit "required" as an array.
func TestToolSchemasNoNullRequired(t *testing.T) {
	for _, def := range CoreTools().Defs() {
		b, err := json.Marshal(def.Function.Parameters)
		if err != nil {
			t.Fatalf("%s: %v", def.Function.Name, err)
		}
		if strings.Contains(string(b), `"required":null`) {
			t.Errorf("tool %q has a null required (grok rejects it): %s", def.Function.Name, b)
		}
	}
}
