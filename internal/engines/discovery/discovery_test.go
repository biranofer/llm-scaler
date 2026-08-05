package discovery_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/discovery"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// testNamespace and testModelID are shared across the discovery specs.
const (
	testNamespace = "ns"
	testModelID   = "m1"
)

// buildVA constructs a synthesized VariantAutoscaling for tests.
func buildVA(name, scaleTargetName, variantCost string, minR *int32, maxR int32) llmdvariant.VariantAutoscaling {
	return llmdvariant.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: llmdvariant.VariantAutoscalingSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: scaleTargetName},
			ModelID:        testModelID,
			MinReplicas:    minR,
			MaxReplicas:    maxR,
			VariantAutoscalingConfigSpec: llmdvariant.VariantAutoscalingConfigSpec{
				VariantCost: variantCost,
			},
		},
	}
}

// buildDeployment constructs a Deployment scale target with the given replica
// counts and optional role label.
func buildDeployment(specReplicas, statusReplicas, readyReplicas int32, role string) *appsv1.Deployment {
	d := &appsv1.Deployment{}
	d.Spec.Replicas = ptr.To(specReplicas)
	d.Status.Replicas = statusReplicas
	d.Status.ReadyReplicas = readyReplicas
	if role != "" {
		d.Spec.Template.Labels = map[string]string{"llm-d.ai/role": role}
	}
	return d
}

// targets wraps a single deployment accessor keyed as Discover expects.
func targets(scaleTargetName string, d *appsv1.Deployment) map[string]scaletarget.ScaleTargetAccessor {
	return map[string]scaletarget.ScaleTargetAccessor{
		utils.GetNamespacedKey(testNamespace, scaleTargetName): scaletarget.NewDeploymentAccessor(d),
	}
}

var _ = Describe("Discover", func() {
	ctx := context.Background()

	It("resolves identity, replica state, and cost from the VA + scale target", func() {
		va := buildVA("v1", "dep1", "2.5", ptr.To(int32(1)), 8)
		d := buildDeployment(3, 3, 2, "prefill")

		metas := discovery.Discover(ctx, []llmdvariant.VariantAutoscaling{va}, targets("dep1", d), nil)

		Expect(metas).To(HaveLen(1))
		m := metas[0]
		Expect(m.VariantName).To(Equal("v1"))
		Expect(m.ModelID).To(Equal(testModelID))
		Expect(m.Namespace).To(Equal("ns"))
		Expect(m.Role).To(Equal("prefill"))
		Expect(m.Cost).To(Equal(2.5))
		Expect(m.CurrentReplicas).To(Equal(3))
		Expect(m.ReadyReplicas).To(Equal(2))
		Expect(m.PendingReplicas).To(Equal(1))
		Expect(m.MinReplicas).NotTo(BeNil())
		Expect(*m.MinReplicas).To(Equal(1))
		Expect(m.MaxReplicas).NotTo(BeNil())
		Expect(*m.MaxReplicas).To(Equal(8))
	})

	It("defaults cost when unset or unparseable", func() {
		empty := buildVA("v1", "dep1", "", nil, 0)
		bad := buildVA("v2", "dep2", "not-a-number", nil, 0)
		d := buildDeployment(1, 1, 1, "")

		emptyMetas := discovery.Discover(ctx, []llmdvariant.VariantAutoscaling{empty}, targets("dep1", d), nil)
		badMetas := discovery.Discover(ctx, []llmdvariant.VariantAutoscaling{bad}, targets("dep2", d), nil)

		Expect(emptyMetas).To(HaveLen(1))
		Expect(emptyMetas[0].Cost).To(Equal(discovery.DefaultVariantCost))
		Expect(badMetas).To(HaveLen(1))
		Expect(badMetas[0].Cost).To(Equal(discovery.DefaultVariantCost))
	})

	It("falls back to spec replicas when status replicas is zero", func() {
		va := buildVA("v1", "dep1", "1", nil, 0)
		d := buildDeployment(2, 0, 0, "")

		metas := discovery.Discover(ctx, []llmdvariant.VariantAutoscaling{va}, targets("dep1", d), nil)

		Expect(metas).To(HaveLen(1))
		Expect(metas[0].CurrentReplicas).To(Equal(2))
		Expect(metas[0].PendingReplicas).To(Equal(2))
	})

	It("clamps pending replicas when ready exceeds current", func() {
		va := buildVA("v1", "dep1", "1", nil, 0)
		d := buildDeployment(1, 1, 5, "") // readyReplicas > statusReplicas

		metas := discovery.Discover(ctx, []llmdvariant.VariantAutoscaling{va}, targets("dep1", d), nil)

		Expect(metas).To(HaveLen(1))
		Expect(metas[0].PendingReplicas).To(Equal(0))
	})

	It("leaves MaxReplicas nil when unset (zero)", func() {
		va := buildVA("v1", "dep1", "1", nil, 0)
		d := buildDeployment(1, 1, 1, "")

		metas := discovery.Discover(ctx, []llmdvariant.VariantAutoscaling{va}, targets("dep1", d), nil)

		Expect(metas).To(HaveLen(1))
		Expect(metas[0].MinReplicas).To(BeNil())
		Expect(metas[0].MaxReplicas).To(BeNil())
	})

	It("skips a VA whose scale target cannot be resolved", func() {
		va := buildVA("v1", "dep1", "1", nil, 0)
		// No cached scale target and the live fetch returns NotFound → the VA is
		// skipped rather than producing a zero-valued record.
		scheme := runtime.NewScheme()
		Expect(appsv1.AddToScheme(scheme)).To(Succeed())
		c := fake.NewClientBuilder().WithScheme(scheme).Build()

		metas := discovery.Discover(ctx, []llmdvariant.VariantAutoscaling{va}, nil, c)

		Expect(metas).To(BeEmpty())
	})
})

var _ = Describe("RoleFromScaleTarget", func() {
	It("returns 'both' for a nil scale target", func() {
		Expect(discovery.RoleFromScaleTarget(nil)).To(Equal("both"))
	})

	It("maps the llm-d.ai/role label", func() {
		Expect(discovery.RoleFromScaleTarget(scaletarget.NewDeploymentAccessor(buildDeployment(1, 1, 1, "prefill")))).To(Equal("prefill"))
		Expect(discovery.RoleFromScaleTarget(scaletarget.NewDeploymentAccessor(buildDeployment(1, 1, 1, "decode")))).To(Equal("decode"))
		Expect(discovery.RoleFromScaleTarget(scaletarget.NewDeploymentAccessor(buildDeployment(1, 1, 1, "weird")))).To(Equal("both"))
		Expect(discovery.RoleFromScaleTarget(scaletarget.NewDeploymentAccessor(buildDeployment(1, 1, 1, "")))).To(Equal("both"))
	})
})

var _ = Describe("VariantMetadata.ToReplicaState", func() {
	It("projects the replica-state fields and drops identity/economics", func() {
		m := discovery.VariantMetadata{
			VariantName:     "v1",
			ModelID:         "m1",
			Namespace:       "ns",
			Role:            "decode",
			Cost:            2.5,
			AcceleratorName: "H100",
			GPUsPerReplica:  2,
			CurrentReplicas: 4,
			DesiredReplicas: 5,
			ReadyReplicas:   3,
			PendingReplicas: 1,
			MinReplicas:     ptr.To(1),
			MaxReplicas:     ptr.To(8),
		}

		rs := m.ToReplicaState()
		Expect(rs.VariantName).To(Equal("v1"))
		Expect(rs.Role).To(Equal("decode"))
		Expect(rs.GPUsPerReplica).To(Equal(2))
		Expect(rs.CurrentReplicas).To(Equal(4))
		Expect(rs.DesiredReplicas).To(Equal(5))
		Expect(rs.PendingReplicas).To(Equal(1))
		Expect(rs.MinReplicas).To(Equal(m.MinReplicas))
		Expect(rs.MaxReplicas).To(Equal(m.MaxReplicas))
	})
})
