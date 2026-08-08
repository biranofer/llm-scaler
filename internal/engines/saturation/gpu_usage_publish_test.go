package saturation

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// The scale-from-zero engine's capacity check reads decision.DefaultGPUUsage and
// treats an ABSENT snapshot as "unknown", skipping the check rather than denying
// a wake. These specs pin what the saturation engine does and does not publish,
// because both directions have been got wrong here:
//
//   - publishing from inside selectV2Optimizer, after its optimizer guard, meant
//     no snapshot on a default (CostAware) deployment; and
//   - publishing an EMPTY snapshot on the empty-population path conflated "no
//     GPUs are in use" with "nothing could be measured", which is the same
//     branch — a failed Prometheus collection also yields no requests.
var _ = Describe("GPU usage snapshot publication", func() {
	BeforeEach(func() {
		// Reset rather than reassigning the package global: PublishGPUUsage and
		// LatestGPUUsage read that variable unsynchronized, so swapping the
		// pointer races with any concurrent user.
		decision.DefaultGPUUsage.Reset()
	})
	AfterEach(func() {
		decision.DefaultGPUUsage.Reset()
	})

	It("publishes usage independently of which optimizer is selected", func() {
		// enableLimiter defaults to false, so the optimizer is CostAware and
		// selectV2Optimizer returns at its guard. Publication must not be owned
		// by anything downstream of that guard, or the capacity check goes blind
		// on every default deployment.
		e := &Engine{optimizer: pipeline.NewCostAwareOptimizer()}

		opt, constraints := e.selectV2Optimizer(ctx, nil)
		Expect(opt).To(Equal(e.optimizer), "a non-GreedyByScore optimizer is left untouched")
		Expect(constraints).To(BeNil())

		_, ok := decision.LatestGPUUsage()
		Expect(ok).To(BeFalse(),
			"selectV2Optimizer must not own the publish; optimizeV2 publishes ahead of it")
	})

	It("keeps an absent snapshot distinguishable from a measured-empty one", func() {
		// Absence is the signal for "could not measure", and the consumer must be
		// able to tell it apart from a real reading. An empty measured snapshot is
		// a legitimate value and reports ok=true.
		_, ok := decision.LatestGPUUsage()
		Expect(ok).To(BeFalse(), "nothing published yet")

		decision.PublishGPUUsage(map[string]int{}, map[string]map[string]int{})
		snap, ok := decision.LatestGPUUsage()
		Expect(ok).To(BeTrue(), "a measured-empty snapshot is still a snapshot")
		Expect(snap.ByType).To(BeEmpty())
	})

	It("publishes what the population is holding, in both views at once", func() {
		// Driven through the production publish, not a restatement of it: the
		// per-type and per-namespace views must come from the SAME population or
		// a consumer comparing them sees a cluster that does not add up.
		publishPopulationGPUUsage([]pipeline.ModelScalingRequest{
			satReq("chat",
				[]domain.VariantMetadata{{VariantName: "chat-a", AcceleratorName: "A100"}},
				[]domain.VariantReplicaState{{VariantName: "chat-a", CurrentReplicas: 3, GPUsPerReplica: 2}}),
			satReq("batch",
				[]domain.VariantMetadata{{VariantName: "batch-a", AcceleratorName: "A100"}},
				[]domain.VariantReplicaState{{VariantName: "batch-a", CurrentReplicas: 1, GPUsPerReplica: 4}}),
		})

		snap, ok := decision.LatestGPUUsage()
		Expect(ok).To(BeTrue())
		Expect(snap.ByType).To(HaveKeyWithValue("A100", 10),
			"the cluster view sums both namespaces")
		Expect(snap.ByNamespace).To(HaveKeyWithValue("chat", HaveKeyWithValue("A100", 6)))
		Expect(snap.ByNamespace).To(HaveKeyWithValue("batch", HaveKeyWithValue("A100", 4)))
	})

	It("publishes nothing from a cycle that measured no model", func() {
		// The path that matters most and is easiest to get wrong. Reaching
		// optimizeV2 with no usable request does NOT mean the cluster is idle —
		// a fleet parked at zero returns from optimize long before this — it means
		// every active model failed collection. Publishing zeros here would tell
		// the scale-from-zero engine there is room at exactly the moment WVA has
		// lost sight of the population.
		//
		// Here the models are enumerated but their namespace has no saturation
		// config loaded, which is the bootstrap shape of that failure.
		measured := []pipeline.ModelScalingRequest{
			satReq("chat",
				[]domain.VariantMetadata{{VariantName: "chat-a", AcceleratorName: "A100"}},
				[]domain.VariantReplicaState{{VariantName: "chat-a", CurrentReplicas: 3, GPUsPerReplica: 2}}),
		}
		publishPopulationGPUUsage(measured)

		e := &Engine{Config: config.NewTestConfig(), optimizer: pipeline.NewCostAwareOptimizer()}
		decisions := e.optimizeV2(ctx, map[string][]llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
			"chat/m": {{
				ObjectMeta: metav1.ObjectMeta{Name: "chat-a", Namespace: "chat"},
				Spec:       llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{ModelID: "m"},
			}},
		}, nil)
		Expect(decisions).To(BeEmpty())

		snap, ok := decision.LatestGPUUsage()
		Expect(ok).To(BeTrue(), "the last measured snapshot must survive a blind cycle")
		Expect(snap.ByType).To(HaveKeyWithValue("A100", 6),
			"a blind cycle must not overwrite the last real measurement with zeros")
	})
})
