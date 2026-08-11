package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

// The property under test is one sentence: a controller cannot grant itself the
// right to scale without limits.
//
// It matters for a specific deployment shape — a namespace-scoped controller
// running IN the namespace it manages — because there the namespace's own
// administrator owns the controller's Deployment, and therefore its flags, its
// env and its ServiceAccount. Every input that decides the outcome below is a
// cluster-scoped object for exactly that reason: a namespace admin holds RBAC
// inside their namespace, and can neither create a Namespace nor annotate one.
var _ = Describe("ResolvePolicyNamespace", func() {
	const tenantNS = "team-a"

	newScheme := func() *runtime.Scheme {
		s := runtime.NewScheme()
		Expect(corev1.AddToScheme(s)).To(Succeed())
		return s
	}

	namespace := func(name string, annotations map[string]string) *corev1.Namespace {
		return &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations},
		}
	}

	// selfManaged builds the tenant-owned shape: POD_NAMESPACE and the watched
	// namespace are the same, which is what the namespace-scoped overlay produces
	// by default.
	selfManaged := func() *config.Config {
		c := &config.Config{}
		config.SetWatchNamespaceForTest(c, tenantNS)
		return c
	}

	BeforeEach(func() {
		GinkgoT().Setenv("POD_NAMESPACE", tenantNS)
	})

	Context("when the controller manages the namespace it runs in", func() {
		// The default namespace-scoped install — which is what OpenShift gets
		// without asking for it — must come up on a cluster that has never heard
		// of a policy namespace.
		It("starts normally when no policy has been published anywhere", func() {
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithObjects(namespace(tenantNS, nil)).Build()

			cfg := selfManaged()
			Expect(ResolvePolicyNamespace(context.Background(), c, cfg)).To(Succeed())

			Expect(cfg.PolicySource()).To(Equal(config.PolicySourceLocal))
			Expect(cfg.PolicyNamespaceIsSeparate()).To(BeFalse())
		})

		It("takes the policy namespace an admin annotated onto the namespace", func() {
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithObjects(namespace(tenantNS, map[string]string{
					constants.PolicyNamespaceAnnotationKey: "platform-policy",
				})).Build()

			cfg := selfManaged()
			Expect(ResolvePolicyNamespace(context.Background(), c, cfg)).To(Succeed())

			Expect(cfg.PolicyNamespace()).To(Equal("platform-policy"))
			Expect(cfg.PolicyNamespaceIsSeparate()).To(BeTrue())
		})

		It("uses the well-known namespace when it exists", func() {
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithObjects(
					namespace(tenantNS, nil),
					namespace(config.WellKnownPolicyNamespace, nil),
				).Build()

			cfg := selfManaged()
			Expect(ResolvePolicyNamespace(context.Background(), c, cfg)).To(Succeed())

			Expect(cfg.PolicyNamespace()).To(Equal(config.WellKnownPolicyNamespace))
			Expect(cfg.PolicySource()).To(Equal(config.PolicySourceWellKnown))
		})

		// The annotation outranks the well-known namespace, so an admin can move one
		// namespace's policy without disturbing the cluster-wide default.
		It("prefers the namespace annotation over the well-known namespace", func() {
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithObjects(
					namespace(tenantNS, map[string]string{
						constants.PolicyNamespaceAnnotationKey: "platform-policy",
					}),
					namespace(config.WellKnownPolicyNamespace, nil),
				).Build()

			cfg := selfManaged()
			Expect(ResolvePolicyNamespace(context.Background(), c, cfg)).To(Succeed())
			Expect(cfg.PolicyNamespace()).To(Equal("platform-policy"))
		})

		// A controller with no namespace read is an ordinary namespace-scoped
		// deployment, not a suspicious one. It must still start.
		It("starts when it cannot read the namespace it manages", func() {
			c := fake.NewClientBuilder().WithScheme(newScheme()).Build() // namespace absent

			cfg := selfManaged()
			Expect(ResolvePolicyNamespace(context.Background(), c, cfg)).To(Succeed())
			Expect(cfg.PolicySource()).To(Equal(config.PolicySourceLocal))
		})
	})

	Context("when the controller does not manage its own namespace", func() {
		// An admin-run controller — cluster-scoped, or namespace-scoped from
		// outside — is already beyond the tenant's reach, so it is not made to
		// prove anything.
		It("starts without policy, because the tenant does not own it", func() {
			GinkgoT().Setenv("POD_NAMESPACE", "wva-system")
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithObjects(namespace(tenantNS, nil)).Build()

			cfg := &config.Config{}
			config.SetWatchNamespaceForTest(cfg, tenantNS)

			Expect(ResolvePolicyNamespace(context.Background(), c, cfg)).To(Succeed())
			Expect(cfg.PolicySource()).To(Equal(config.PolicySourceLocal))
		})

		// Regression guard. Letting the well-known namespace win here took a
		// working, correctly-bounded cluster-scoped install and silently switched
		// its policy source the moment someone created wva-policy for an unrelated
		// tenant — and if that ConfigMap declared no limiters, the cluster went
		// from bounded to UNBOUNDED with no edit to the install that changed.
		It("does NOT let the well-known namespace hijack an admin-owned controller", func() {
			GinkgoT().Setenv("POD_NAMESPACE", "wva-system")
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithObjects(namespace(config.WellKnownPolicyNamespace, nil)).Build()

			cfg := &config.Config{}
			Expect(ResolvePolicyNamespace(context.Background(), c, cfg)).To(Succeed())

			Expect(cfg.PolicySource()).To(Equal(config.PolicySourceLocal))
			Expect(cfg.PolicyNamespaceIsSeparate()).To(BeFalse())
		})
	})
})
