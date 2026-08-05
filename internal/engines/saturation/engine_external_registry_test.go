package saturation

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
)

var _ = Describe("external analyzer runtime registry", func() {
	makeEngine := func() *Engine {
		sat := &fakeAnalyzerWithResult{analyzerName: domain.SaturationAnalyzerName, result: &domain.AnalyzerResult{}}
		return &Engine{
			saturationV2Analyzer: sat,
			analyzersSnapshot:    []analyzerEntry{{name: domain.SaturationAnalyzerName, analyzer: sat}},
			externalAnalyzers:    make(map[string]domain.Analyzer),
			started:              true,
		}
	}

	cfgWithExt := config.SaturationScalingConfig{
		ScaleUpThreshold:  0.85,
		ScaleDownBoundary: 0.70,
		Analyzers: []config.AnalyzerScoreConfig{
			{Name: domain.SaturationAnalyzerName},
			{Name: "ext-demand"},
		},
	}

	extAnalyzer := func() *fakeAnalyzerWithResult {
		return &fakeAnalyzerWithResult{analyzerName: "ext-demand", result: &domain.AnalyzerResult{AnalyzerName: "ext-demand"}}
	}

	names := func(rs []pipeline.NamedAnalyzerResult) []string {
		out := make([]string, 0, len(rs))
		for _, r := range rs {
			out = append(out, r.Name)
		}
		return out
	}

	run := func(e *Engine, cfg config.SaturationScalingConfig) []pipeline.NamedAnalyzerResult {
		results, err := e.runAnalyzersAndScore(context.Background(), "m", "ns", nil, cfg, nil, nil, nil, nil, 0)
		Expect(err).NotTo(HaveOccurred())
		return results
	}

	It("runs an external analyzer upserted at runtime when it is enabled in config", func() {
		e := makeEngine()
		e.UpsertExternalAnalyzer("ext-demand", extAnalyzer())
		Expect(names(run(e, cfgWithExt))).To(ContainElement("ext-demand"))
	})

	It("stops running it after RemoveExternalAnalyzer", func() {
		e := makeEngine()
		e.UpsertExternalAnalyzer("ext-demand", extAnalyzer())
		e.RemoveExternalAnalyzer("ext-demand")
		Expect(names(run(e, cfgWithExt))).NotTo(ContainElement("ext-demand"))
	})

	It("does not run an upserted analyzer that is absent from config (opt-in)", func() {
		e := makeEngine()
		e.UpsertExternalAnalyzer("ext-demand", extAnalyzer())
		cfgNoExt := config.SaturationScalingConfig{
			ScaleUpThreshold:  0.85,
			ScaleDownBoundary: 0.70,
			Analyzers:         []config.AnalyzerScoreConfig{{Name: domain.SaturationAnalyzerName}},
		}
		Expect(names(run(e, cfgNoExt))).NotTo(ContainElement("ext-demand"))
	})

	It("does not duplicate a built-in when an external name collides with it", func() {
		e := makeEngine()
		e.UpsertExternalAnalyzer(domain.SaturationAnalyzerName, extAnalyzer())
		count := 0
		for _, n := range names(run(e, cfgWithExt)) {
			if n == domain.SaturationAnalyzerName {
				count++
			}
		}
		Expect(count).To(Equal(1))
	})
})
