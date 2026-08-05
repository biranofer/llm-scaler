package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAnalyzerCatalogConfigMap(t *testing.T) {
	data := map[string]string{
		"ttft-slo": "engines:\n" +
			"  vllm:\n    query: q_vllm\n    threshold: \"0.5\"\n" +
			"  sglang:\n    query: q_sglang\n    threshold: \"0.4\"\n",
		"pool-queue": "query: q_agnostic\nthreshold: \"1.0\"\n",
		"broken":     "not-a-mapping",
	}

	cat := ParseAnalyzerCatalogConfigMap(data)

	// A malformed entry is skipped, not fatal.
	require.Contains(t, cat, "ttft-slo")
	require.Contains(t, cat, "pool-queue")
	assert.NotContains(t, cat, "broken")

	// Per-engine bodies.
	assert.Equal(t, "q_vllm", cat["ttft-slo"].Engines["vllm"].Query)
	assert.Equal(t, "0.5", cat["ttft-slo"].Engines["vllm"].Threshold)
	assert.Equal(t, "q_sglang", cat["ttft-slo"].Engines["sglang"].Query)

	// Engine-agnostic body (top-level query/threshold, no engines map).
	assert.Equal(t, "q_agnostic", cat["pool-queue"].Query)
	assert.Equal(t, "1.0", cat["pool-queue"].Threshold)
	assert.Empty(t, cat["pool-queue"].Engines)
}

func TestParseAnalyzerCatalogConfigMap_Nil(t *testing.T) {
	cat := ParseAnalyzerCatalogConfigMap(nil)
	assert.NotNil(t, cat)
	assert.Empty(t, cat)
}

func TestConfig_ExternalAnalyzerCatalog_RoundTripAndIsolation(t *testing.T) {
	cfg := NewTestConfig()

	assert.Empty(t, cfg.ExternalAnalyzerCatalog())

	cfg.UpdateExternalAnalyzerCatalog(ExternalAnalyzerCatalog{
		"a": {Engines: map[string]ExternalAnalyzerBody{"vllm": {Query: "q", Threshold: "1"}}},
	})

	got := cfg.ExternalAnalyzerCatalog()
	require.Contains(t, got, "a")
	assert.Equal(t, "q", got["a"].Engines["vllm"].Query)

	// Mutating the returned copy must not affect the stored catalog.
	got["a"].Engines["vllm"] = ExternalAnalyzerBody{Query: "mutated"}
	again := cfg.ExternalAnalyzerCatalog()
	assert.Equal(t, "q", again["a"].Engines["vllm"].Query)
}
