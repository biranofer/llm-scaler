package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

func TestRecordAnalyzerDemandAndTarget(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics failed: %v", err)
	}
	emitter := NewMetricsEmitter()

	emitter.RecordAnalyzerDemand("saturation", "ns", "m", "", 42)
	emitter.RecordAnalyzerDemand("ttft-slo", "ns", "m", "prefill", 7)
	emitter.RecordAnalyzerTarget("ttft-slo", "ns", "m", "v1", 0.5)

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	var gotDemand, gotTarget bool
	for _, mf := range mfs {
		switch mf.GetName() {
		case constants.WVAAnalyzerDemand:
			for _, m := range mf.GetMetric() {
				if getLabelValue(m, constants.LabelAnalyzerName) == "saturation" &&
					getLabelValue(m, constants.LabelRole) == "" {
					gotDemand = true
					if v := m.GetGauge().GetValue(); v != 42 {
						t.Errorf("wva_analyzer_demand{analyzer=saturation} = %v, want 42", v)
					}
				}
			}
		case constants.WVAAnalyzerTarget:
			for _, m := range mf.GetMetric() {
				if getLabelValue(m, constants.LabelVariantName) == "v1" {
					gotTarget = true
					if v := m.GetGauge().GetValue(); v != 0.5 {
						t.Errorf("wva_analyzer_target{variant_name=v1} = %v, want 0.5", v)
					}
				}
			}
		}
	}
	if !gotDemand {
		t.Errorf("metric %s (analyzer=saturation) not found", constants.WVAAnalyzerDemand)
	}
	if !gotTarget {
		t.Errorf("metric %s (variant_name=v1) not found", constants.WVAAnalyzerTarget)
	}
}

// TestRecordAnalyzerMetrics_NilGuard ensures the Record methods are a no-op when
// InitMetrics has not run (the gauges are nil), matching the other emitters.
func TestRecordAnalyzerMetrics_NilGuard(t *testing.T) {
	analyzerDemand = nil
	analyzerTarget = nil
	emitter := NewMetricsEmitter()
	// Must not panic.
	emitter.RecordAnalyzerDemand("a", "ns", "m", "", 1)
	emitter.RecordAnalyzerTarget("a", "ns", "m", "v", 1)
}
