package codex

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDefaultModelsIncludesGPT6(t *testing.T) {
	models := DefaultModels()
	for _, model := range []string{"gpt-6-sol", "gpt-6-luna", "gpt-6-astra", "gpt-6.1-sol"} {
		require.Contains(t, models, model)
	}

	seen := make(map[string]bool, len(models))
	for _, model := range models {
		require.False(t, seen[model], "duplicate model: %s", model)
		seen[model] = true
	}
}
