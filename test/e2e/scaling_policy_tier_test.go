package e2e

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// A named scaling policy is a reusable TIER — "interactive", "batch" — carrying
// thresholds but no model and no namespace. A workload joins one by naming it in
// its trigger metadata, which is the only thing binding the two: the tier does not
// know its members, and that is the whole point of it.
//
// This suite proves the name travels the entire path it has to travel to matter:
//
//	ScaledObject trigger metadata → KEDA → gRPC scalerMetadata → registry →
//	policy resolution (default entry ← tier) → optimizer → decision → KEDA → pods
//
// It is built as a matched pair of arcs at ONE operating point. The simulator
// reports a fixed kv-cache-usage of 0.3 for the whole run, so the replica count is
// a pure function of the thresholds in play — and the two threshold pairs used
// here are the ones the KEDA external-scaler suite and the V2 suite already pin to
// 2 replicas and 1 replica respectively at exactly this occupancy.
//
// The default entry alone therefore decides 1. If the deployment reaches 2, the
// tier's numbers reached the optimizer; if removing the tier returns it to 1, the
// tier's numbers — not some other pressure — were what held it there.
const tierFakeMetricsJSON = `{"kv-cache-usage":0.3,"running-requests":1,"waiting-requests":0}`

const (
	// Shared by both entries so the arcs differ ONLY by the threshold pair.
	tierKvCacheThreshold     = 0.80
	tierQueueLengthThreshold = 50

	// Default entry: the canonical ordering the V2 suite settles at 1 replica.
	tierDefaultScaleUpThreshold  = 0.95
	tierDefaultScaleDownBoundary = 0.85

	// The tier: the pair the external-scaler suite drives to 2 replicas.
	tierPolicyScaleUpThreshold  = 0.30
	tierPolicyScaleDownBoundary = 0.20

	// The tier's name, which is also its ConfigMap key. A tier carries no model
	// identity in its body — that is what tells it apart from an override.
	tierPolicyName = "interactive"

	// The override entry's key. Arbitrary by design — what binds it to the model
	// is the model_id/namespace in its body.
	tierOverrideKey = "policy-tier-model-override"
)

var _ = Describe("Named scaling policy tier", Label("full"), Ordered, func() {
	const (
		poolName              = "policy-tier-pool"
		modelSvcName          = "policy-tier-ms"
		modelDecodeDeployment = modelSvcName + "-decode"
		serviceName           = modelSvcName + "-service"
		smName                = modelSvcName + "-monitor"

		scalerBaseName = "policy-tier"
	)

	var (
		modelID         string
		cmName          string
		cmNamespace     string
		cmOriginal      *corev1.ConfigMap
		cmExistedBefore bool
		variantName     string
		scalerAddress   string
	)

	BeforeAll(func() {
		if !cfg.UseSimulator {
			Skip("This suite needs the simulator runtime: set USE_SIMULATOR=true. " +
				"It uses llm-d-inference-sim's --fake-metrics flag, which real vLLM rejects.")
		}

		modelID = cfg.ModelID
		cmName = scalingPolicyConfigMapName()
		cmNamespace = cfg.WVANamespace
		variantName = scalerBaseName + "-so"
		scalerAddress = "wva-external-scaler." + cfg.WVANamespace + ".svc.cluster.local:9090"

		By("Snapshotting the scaling policy ConfigMap for restore in AfterAll")
		cm, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if err == nil {
			cmExistedBefore = true
			cmOriginal = cm.DeepCopy()
		} else if !errors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred(), "failed reading existing scaling policy configmap")
		}

		By("Installing a default entry that holds at 1 replica, and an " + tierPolicyName + " tier that does not")
		// Written before the workload registers, so the first decision the engine
		// makes for it already has the tier to resolve against.
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, defaultConfigKey,
			buildSaturationConfigYAMLWithThresholds(
				"saturation", tierKvCacheThreshold, tierQueueLengthThreshold,
				tierDefaultScaleUpThreshold, tierDefaultScaleDownBoundary,
			))).To(Succeed())
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, tierPolicyName,
			buildSaturationConfigYAMLWithThresholds(
				"saturation", tierKvCacheThreshold, tierQueueLengthThreshold,
				tierPolicyScaleUpThreshold, tierPolicyScaleDownBoundary,
			))).To(Succeed())

		By("Creating the model service with --fake-metrics so the operating point is fixed")
		_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
		Expect(fixtures.CreateModelServiceWithExtraArgs(
			ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, poolName, modelID, variantName,
			cfg.UseSimulator, cfg.MaxNumSeqs,
			[]string{"--fake-metrics", tierFakeMetricsJSON},
		)).To(Succeed())
		Expect(fixtures.EnsureService(
			ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment, 8000,
		)).To(Succeed())
		Expect(fixtures.EnsureServiceMonitor(
			ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment,
		)).To(Succeed())

		By("Waiting for the policy-tier model deployment to be ready")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 1))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())

		By("Registering the workload with WVA, naming the " + tierPolicyName + " tier in its trigger metadata")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBaseName, modelDecodeDeployment, variantName, 1, 10, cfg.MonitoringNS,
			fixtures.WithWVATriggerMetadata(modelID, "30.0"),
			fixtures.WithScalingPolicy(tierPolicyName),
			fixtures.WithExternalScalerTrigger(scalerAddress),
			fixtures.WithScaledObjectScaleDownStabilizationWindow(30))).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBaseName) })
	})

	AfterAll(func() {
		By("Restoring the scaling policy ConfigMap")
		if cmExistedBefore && cmOriginal != nil {
			propagation := metav1.DeletePropagationBackground
			if err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Delete(ctx, cmName, metav1.DeleteOptions{
				PropagationPolicy: &propagation,
			}); err != nil && !errors.IsNotFound(err) {
				GinkgoWriter.Printf("Warning: failed to delete scaling policy configmap %s before restore: %v\n", cmName, err)
			}
			toCreate := saturationConfigMapForRecreate(cmOriginal)
			if _, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Create(ctx, toCreate, metav1.CreateOptions{}); err != nil {
				GinkgoWriter.Printf("Warning: failed to recreate scaling policy configmap %s: %v\n", cmName, err)
			}
		} else {
			_ = k8sClient.CoreV1().ConfigMaps(cmNamespace).Delete(ctx, cmName, metav1.DeleteOptions{})
		}

		By("Cleaning up policy-tier resources")
		_ = crClient.Delete(ctx, &promoperator.ServiceMonitor{
			ObjectMeta: metav1.ObjectMeta{Name: smName, Namespace: cfg.MonitoringNS},
		})
		_ = k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, modelDecodeDeployment, metav1.DeleteOptions{})
	})

	// The tier's thresholds — not the default entry's — decide the count. At this
	// occupancy the default entry alone decides 1, so reaching 2 is only possible
	// if the name in the trigger metadata survived the whole path and selected the
	// tier's numbers.
	It("scales on the thresholds of the tier the workload names", func() {
		By("Asserting KEDA actuates scale-up to >= 2 replicas on the tier's scaleUpThreshold")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 2),
				"the "+tierPolicyName+" tier sets scaleUpThreshold=0.30, which decides 2 replicas at this "+
					"occupancy; the default entry's 0.95 decides 1, so staying at 1 means the tier never "+
					"reached the optimizer")
		}, time.Duration(cfg.ScaleUpTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())
	})

	// The layering is default entry -> tier -> {modelID}#{namespace}, most specific
	// winning. The per-model override stays innermost so a fleet can adopt tiers
	// model by model rather than all at once: a model that already has an override
	// keeps it when its workload joins a tier.
	It("lets a per-model override win over the tier the workload names", func() {
		By("Confirming the tier is in force at >= 2 replicas before overriding it")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 2),
				"the override assertion is meaningless unless the tier first drove the count up")
		}, time.Duration(cfg.ScaleUpTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())

		By("Adding a per-model override carrying the no-scale threshold pair")
		// Same numbers as the default entry, so the ONLY question this asks is which
		// layer wins: the tier still says 0.30, the override says 0.95, and they
		// disagree about the replica count at this fixed occupancy.
		//
		// The entry names the model in its BODY. Its key is arbitrary — and has to
		// be: the model ID here is "e2ewva/dummy-model", and a ConfigMap data key
		// admits only [-._a-zA-Z0-9], so neither the slash nor the "#" of the old
		// {modelID}#{namespace} form could ever be written.
		overrideYAML := buildSaturationConfigYAMLWithModel(
			"saturation", tierKvCacheThreshold, tierQueueLengthThreshold,
			tierDefaultScaleUpThreshold, tierDefaultScaleDownBoundary,
			modelID, cfg.LLMDNamespace,
		)
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName,
			tierOverrideKey, overrideYAML)).To(Succeed())
		DeferCleanup(func() {
			_ = deleteSaturationConfigEntry(ctx, cmNamespace, cmName, tierOverrideKey)
		})

		By("Asserting the override's decision of 1 replica beats the tier's 2")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically("<=", 1),
				"a per-model override is the innermost layer; the workload still names the "+
					"tier, and the tier must not win over settings bound to this model")
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())
	})

	// Removing the tier a workload names must fall back to the default entry rather
	// than fail: refusing to scale a workload because its policy name no longer
	// resolves would turn a config edit into an outage. The engine reports the
	// unknown name separately, so the fallback is not silent.
	//
	// This arc also closes the first one. Returning to 1 when the tier disappears
	// proves the tier was what held the workload at 2 — nothing else about the
	// workload, the metrics or the cluster changed.
	It("falls back to the default entry when the named tier is removed", func() {
		By("Confirming the deployment is at >= 2 replicas before removing the tier")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 2),
				"the fallback assertion is meaningless unless the tier first drove the count above minReplicas")
		}, time.Duration(cfg.ScaleUpTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())

		By("Deleting the " + tierPolicyName + " entry, leaving the workload naming a tier that no longer exists")
		Expect(deleteSaturationConfigEntry(ctx, cmNamespace, cmName, tierPolicyName)).To(Succeed())

		By("Asserting the workload settles back to the default entry's decision of 1 replica")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically("<=", 1),
				"an unresolvable tier must resolve to the default entry, whose scaleUpThreshold=0.95 "+
					"decides 1 replica at this occupancy")
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())
	})
})
