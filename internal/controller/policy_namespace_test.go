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
		It("refuses to start when no policy applies and no waiver was granted", func() {
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithObjects(namespace(tenantNS, nil)).Build()

			err := ResolvePolicyNamespace(context.Background(), c, selfManaged())

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("refusing to start"))
			// The message has to be actionable by the person who can fix it, so it
			// names every route out.
			Expect(err.Error()).To(ContainSubstring(config.WellKnownPolicyNamespace))
			Expect(err.Error()).To(ContainSubstring(constants.PolicyNamespaceAnnotationKey))
			Expect(err.Error()).To(ContainSubstring(constants.UnboundedAllowedAnnotationKey))
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

		It("starts unbounded only when an admin annotated the namespace to allow it", func() {
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithObjects(namespace(tenantNS, map[string]string{
					constants.UnboundedAllowedAnnotationKey: constants.PolicyUnboundedAllowed,
				})).Build()

			cfg := selfManaged()
			Expect(ResolvePolicyNamespace(context.Background(), c, cfg)).To(Succeed())

			Expect(cfg.PolicyNamespaceIsSeparate()).To(BeFalse())
		})

		// The waiver is a specific word, not a truthy value. Accepting "true" here
		// would let it be set by anything that writes booleans generically, and
		// would read like a routine flag to whoever reviewed the namespace.
		It("does not accept an arbitrary truthy value as the waiver", func() {
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithObjects(namespace(tenantNS, map[string]string{
					constants.UnboundedAllowedAnnotationKey: "true",
				})).Build()

			Expect(ResolvePolicyNamespace(context.Background(), c, selfManaged())).NotTo(Succeed())
		})

		// The pointer outranks the waiver: an admin who named a policy namespace
		// AND left a stale waiver behind meant the policy.
		It("prefers an explicit policy namespace over a waiver", func() {
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithObjects(namespace(tenantNS, map[string]string{
					constants.PolicyNamespaceAnnotationKey:  "platform-policy",
					constants.UnboundedAllowedAnnotationKey: constants.PolicyUnboundedAllowed,
				})).Build()

			cfg := selfManaged()
			Expect(ResolvePolicyNamespace(context.Background(), c, cfg)).To(Succeed())
			Expect(cfg.PolicyNamespace()).To(Equal("platform-policy"))
		})

		// Not being able to READ the namespace is not permission to ignore what it
		// says. This is the case that used to proceed with a log line.
		It("refuses to start when it cannot read the namespace it manages", func() {
			c := fake.NewClientBuilder().WithScheme(newScheme()).Build() // namespace absent

			err := ResolvePolicyNamespace(context.Background(), c, selfManaged())

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("must read that Namespace object"))
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

		It("still prefers the well-known namespace when it exists", func() {
			GinkgoT().Setenv("POD_NAMESPACE", "wva-system")
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithObjects(namespace(config.WellKnownPolicyNamespace, nil)).Build()

			cfg := &config.Config{}
			Expect(ResolvePolicyNamespace(context.Background(), c, cfg)).To(Succeed())
			Expect(cfg.PolicyNamespace()).To(Equal(config.WellKnownPolicyNamespace))
		})
	})
})
