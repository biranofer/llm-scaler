package external_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/external"
)

// fakeSource is a minimal source.MetricsSource for tests: a real QueryList (so
// query registration is exercised) plus canned Refresh results.
type fakeSource struct {
	ql      *source.QueryList
	results map[string]*source.MetricResult
	err     error
}

func newFakeSource() *fakeSource {
	return &fakeSource{ql: source.NewQueryList(), results: map[string]*source.MetricResult{}}
}

func (f *fakeSource) QueryList() *source.QueryList { return f.ql }

func (f *fakeSource) Refresh(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.results, nil
}

func (f *fakeSource) Get(_ string, _ map[string]string) *source.CachedValue { return nil }

// agnosticQueryName is the registered name for an engine-agnostic body.
const agnosticQueryName = "external:ttft-slo:demand"

func agnosticDef() external.Definition {
	return external.Definition{
		Label: "ttft-slo",
		Bodies: map[string]external.Body{
			"": {Query: `sum(vllm:x{namespace="{{.namespace}}",model_name="{{.modelID}}"})`, Threshold: 2.0},
		},
	}
}

var _ = Describe("external.Analyzer", func() {
	var fs *fakeSource

	BeforeEach(func() {
		fs = newFakeSource()
	})

	Describe("New", func() {
		It("registers an engine-agnostic body's query and returns a named analyzer", func() {
			a, err := external.New(agnosticDef(), fs)
			Expect(err).NotTo(HaveOccurred())
			Expect(a.Name()).To(Equal("ttft-slo"))
			Expect(fs.ql.Get(agnosticQueryName)).NotTo(BeNil())
		})

		It("registers one engine-scoped query per body", func() {
			def := external.Definition{
				Label: "ttft-slo",
				Bodies: map[string]external.Body{
					"vllm":   {Query: "vllm_q", Threshold: 2.0},
					"sglang": {Query: "sglang_q", Threshold: 5.0},
				},
			}
			_, err := external.New(def, fs)
			Expect(err).NotTo(HaveOccurred())
			Expect(fs.ql.Get("external:ttft-slo:demand:vllm")).NotTo(BeNil())
			Expect(fs.ql.Get("external:ttft-slo:demand:sglang")).NotTo(BeNil())
		})

		It("rejects an empty label", func() {
			def := agnosticDef()
			def.Label = ""
			_, err := external.New(def, fs)
			Expect(err).To(HaveOccurred())
		})

		It("rejects an empty body set", func() {
			_, err := external.New(external.Definition{Label: "l", Bodies: map[string]external.Body{}}, fs)
			Expect(err).To(HaveOccurred())
		})

		It("rejects a body with an empty query", func() {
			_, err := external.New(external.Definition{Label: "l", Bodies: map[string]external.Body{"": {Threshold: 1}}}, fs)
			Expect(err).To(HaveOccurred())
		})

		It("rejects a body with a non-positive threshold", func() {
			_, err := external.New(external.Definition{Label: "l", Bodies: map[string]external.Body{"": {Query: "x", Threshold: 0}}}, fs)
			Expect(err).To(HaveOccurred())
		})

		It("rejects a nil source", func() {
			_, err := external.New(agnosticDef(), nil)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("Analyze", func() {
		It("sums the demand series and applies the body's threshold as P", func() {
			a, err := external.New(agnosticDef(), fs)
			Expect(err).NotTo(HaveOccurred())
			fs.results[agnosticQueryName] = &source.MetricResult{
				Values: []source.MetricValue{{Value: 3}, {Value: 5}},
			}

			res, err := a.Analyze(context.Background(), domain.AnalyzerInput{
				ModelID:   "m",
				Namespace: "ns",
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "v1", CurrentReplicas: 4, PendingReplicas: 1},
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(res.TotalDemand).To(Equal(8.0)) // 3 + 5
			Expect(res.VariantCapacities).To(HaveLen(1))
			vc := res.VariantCapacities[0]
			Expect(vc.PerReplicaCapacity).To(Equal(2.0))
			Expect(vc.ReplicaCount).To(Equal(3)) // 4 current − 1 pending
			Expect(res.TotalSupply).To(Equal(6.0))
			Expect(res.TotalAnticipatedSupply).To(Equal(8.0))
			Expect(vc.Cost).To(BeZero())
			Expect(vc.AcceleratorName).To(BeEmpty())
		})

		It("selects the query body matching the model's engine", func() {
			def := external.Definition{
				Label: "ttft-slo",
				Bodies: map[string]external.Body{
					"vllm":   {Query: "vllm_q", Threshold: 2.0},
					"sglang": {Query: "sglang_q", Threshold: 5.0},
				},
			}
			a, err := external.New(def, fs)
			Expect(err).NotTo(HaveOccurred())
			fs.results["external:ttft-slo:demand:sglang"] = &source.MetricResult{
				Values: []source.MetricValue{{Value: 10}},
			}

			res, err := a.Analyze(context.Background(), domain.AnalyzerInput{
				ModelID:   "m",
				Namespace: "ns",
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "v1", CurrentReplicas: 1, Engine: "sglang"},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.TotalDemand).To(Equal(10.0))
			Expect(res.VariantCapacities[0].PerReplicaCapacity).To(Equal(5.0)) // sglang threshold
		})

		It("returns a nil result when no body matches the model's engine", func() {
			def := external.Definition{
				Label:  "ttft-slo",
				Bodies: map[string]external.Body{"vllm": {Query: "vllm_q", Threshold: 2.0}},
			}
			a, err := external.New(def, fs)
			Expect(err).NotTo(HaveOccurred())

			res, err := a.Analyze(context.Background(), domain.AnalyzerInput{
				ModelID:   "m",
				Namespace: "ns",
				VariantStates: []domain.VariantReplicaState{
					{VariantName: "v1", CurrentReplicas: 1, Engine: "sglang"},
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(BeNil())
		})

		It("treats a failed query result as zero demand", func() {
			a, _ := external.New(agnosticDef(), fs)
			fs.results[agnosticQueryName] = &source.MetricResult{Error: errors.New("query failed")}

			res, err := a.Analyze(context.Background(), domain.AnalyzerInput{ModelID: "m", Namespace: "ns"})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.TotalDemand).To(BeZero())
		})

		It("propagates a source Refresh error", func() {
			a, _ := external.New(agnosticDef(), fs)
			fs.err = errors.New("prometheus unreachable")
			_, err := a.Analyze(context.Background(), domain.AnalyzerInput{ModelID: "m", Namespace: "ns"})
			Expect(err).To(HaveOccurred())
		})
	})
})
