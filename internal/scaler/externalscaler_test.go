package scaler_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/scaler"
)

// testNamespace is the single namespace these specs operate in. The handler
// resolves a ScaledObject and its decision by (namespace, name), so the refs,
// the seeded ScaledObjects and the decision-store entries must all agree on it —
// naming it keeps that pairing explicit rather than repeating a bare literal.
const testNamespace = "chat"

func ref(name string, metadata map[string]string) *pb.ScaledObjectRef {
	return &pb.ScaledObjectRef{Namespace: testNamespace, Name: name, ScalerMetadata: metadata}
}

func ptr(v int32) *int32 { return &v }

var _ = Describe("External scaler handler", func() {
	var (
		ctx   context.Context
		store *decision.Store
	)

	// newHandler builds a Handler backed by a fake client seeded with objs and
	// the fresh per-spec decision store.
	newHandler := func(objs ...client.Object) *scaler.Handler {
		s := runtime.NewScheme()
		Expect(kedav1alpha1.AddToScheme(s)).To(Succeed())
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
		return scaler.NewHandler(c, store)
	}

	scaledObject := func(namespace, name, target string) *kedav1alpha1.ScaledObject {
		return &kedav1alpha1.ScaledObject{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
			Spec: kedav1alpha1.ScaledObjectSpec{
				ScaleTargetRef: &kedav1alpha1.ScaleTarget{Name: target},
			},
		}
	}

	BeforeEach(func() {
		ctx = context.Background()
		store = decision.NewStore()
	})

	Describe("GetMetricSpec", func() {
		It("advertises the WVA metric with a target of 1", func() {
			h := newHandler()
			resp, err := h.GetMetricSpec(ctx, ref("chat-decode", nil))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.MetricSpecs).To(HaveLen(1))
			Expect(resp.MetricSpecs[0].MetricName).To(Equal(scaler.MetricName))
			Expect(resp.MetricSpecs[0].TargetSize).To(Equal(int64(1)))
		})
	})

	Describe("GetMetrics", func() {
		It("returns the desired replicas resolved via the ScaledObject's scaleTargetRef", func() {
			h := newHandler(scaledObject(testNamespace, "chat-decode", "chat-decode-deploy"))
			store.Set(testNamespace, "chat-decode-deploy", 5)

			resp, err := h.GetMetrics(ctx, &pb.GetMetricsRequest{
				ScaledObjectRef: ref("chat-decode", nil),
				MetricName:      scaler.MetricName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.MetricValues).To(HaveLen(1))
			Expect(resp.MetricValues[0].MetricName).To(Equal(scaler.MetricName))
			Expect(resp.MetricValues[0].MetricValue).To(Equal(int64(5)))
		})

		It("returns 0 before any optimization decision exists", func() {
			h := newHandler(scaledObject(testNamespace, "chat-decode", "chat-decode-deploy"))

			resp, err := h.GetMetrics(ctx, &pb.GetMetricsRequest{
				ScaledObjectRef: ref("chat-decode", nil),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.MetricValues[0].MetricValue).To(Equal(int64(0)))
		})

		It("honours a variantName override without reading the ScaledObject", func() {
			h := newHandler() // no ScaledObject present
			store.Set(testNamespace, "direct-target", 7)

			resp, err := h.GetMetrics(ctx, &pb.GetMetricsRequest{
				ScaledObjectRef: ref("ignored", map[string]string{"variantName": "direct-target"}),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.MetricValues[0].MetricValue).To(Equal(int64(7)))
		})

		It("errors when the ScaledObject is missing", func() {
			h := newHandler()

			_, err := h.GetMetrics(ctx, &pb.GetMetricsRequest{
				ScaledObjectRef: ref("missing", nil),
			})
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("IsActive", func() {
		DescribeTable("gates on WVA's decision",
			func(seed *int32, wantActive bool) {
				h := newHandler(scaledObject(testNamespace, "chat-decode", "chat-decode-deploy"))
				if seed != nil {
					store.Set(testNamespace, "chat-decode-deploy", *seed)
				}
				resp, err := h.IsActive(ctx, ref("chat-decode", nil))
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.Result).To(Equal(wantActive))
			},
			Entry("desired > 0 -> active", ptr(3), true),
			Entry("desired == 0 -> inactive", ptr(0), false),
			Entry("no decision yet -> active", nil, true),
		)
	})
})
