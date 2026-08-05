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

const demandQueryName = "external:ttft-slo:demand"

func validDef() external.Definition {
	return external.Definition{
		Label:       "ttft-slo",
		DemandQuery: `sum(vllm:x{namespace="{{.namespace}}",model_name="{{.modelID}}"})`,
		Threshold:   2.0,
	}
}

var _ = Describe("external.Analyzer", func() {
	var fs *fakeSource

	BeforeEach(func() {
		fs = newFakeSource()
	})

	Describe("New", func() {
		It("registers the demand query and returns a named analyzer", func() {
			a, err := external.New(validDef(), fs)
			Expect(err).NotTo(HaveOccurred())
			Expect(a.Name()).To(Equal("ttft-slo"))
			Expect(fs.ql.Get(demandQueryName)).NotTo(BeNil())
		})

		It("rejects an empty label", func() {
			def := validDef()
			def.Label = ""
			_, err := external.New(def, fs)
			Expect(err).To(HaveOccurred())
		})

		It("rejects an empty demand query", func() {
			def := validDef()
			def.DemandQuery = ""
			_, err := external.New(def, fs)
			Expect(err).To(HaveOccurred())
		})

		It("rejects a non-positive threshold", func() {
			def := validDef()
			def.Threshold = 0
			_, err := external.New(def, fs)
			Expect(err).To(HaveOccurred())
		})

		It("rejects a nil source", func() {
			_, err := external.New(validDef(), nil)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("Analyze", func() {
		var a *external.Analyzer

		BeforeEach(func() {
			var err error
			a, err = external.New(validDef(), fs)
			Expect(err).NotTo(HaveOccurred())
		})

		It("sums the demand series and applies the constant threshold as P", func() {
			fs.results[demandQueryName] = &source.MetricResult{
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
			Expect(vc.VariantName).To(Equal("v1"))
			Expect(vc.PerReplicaCapacity).To(Equal(2.0))
			Expect(vc.ReplicaCount).To(Equal(3))              // 4 current − 1 pending
			Expect(res.TotalSupply).To(Equal(6.0))            // 3 ready × 2
			Expect(res.TotalAnticipatedSupply).To(Equal(8.0)) // (3+1) × 2

			// The wrapper must not launder identity — the builder fills it.
			Expect(vc.Cost).To(BeZero())
			Expect(vc.AcceleratorName).To(BeEmpty())
		})

		It("treats a failed query result as zero demand", func() {
			fs.results[demandQueryName] = &source.MetricResult{Error: errors.New("query failed")}

			res, err := a.Analyze(context.Background(), domain.AnalyzerInput{ModelID: "m", Namespace: "ns"})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.TotalDemand).To(BeZero())
		})

		It("treats an absent query result as zero demand", func() {
			// No entry for the query name in the results map.
			res, err := a.Analyze(context.Background(), domain.AnalyzerInput{ModelID: "m", Namespace: "ns"})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.TotalDemand).To(BeZero())
		})

		It("propagates a source Refresh error", func() {
			fs.err = errors.New("prometheus unreachable")
			_, err := a.Analyze(context.Background(), domain.AnalyzerInput{ModelID: "m", Namespace: "ns"})
			Expect(err).To(HaveOccurred())
		})
	})
})
