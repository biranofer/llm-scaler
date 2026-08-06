package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
	testutils "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

// NOTE: V2 is the sole analysis path — the V1 analyzer was removed, so
// analyzerName no longer selects between engines. The arcs that leave
// analyzerName unset now assert that an unset value keeps processing on V2
// rather than stalling the model. This suite remains self-guarded: it snapshots
// the base ConfigMap and writes its own explicit config for each arc, so a
// change to the shipped default does not affect it.

// Saturation calibration via the simulator's --fake-metrics flag.
//
// kv-cache-usage=0.3 and waiting-requests=2 are chosen so both threshold arcs
// are deterministically exercisable by config alone — no load required:
//
// V2 does not compare the usage against the threshold as a boolean the way V1
// did; kvCacheThreshold is a utilization *target* that sizes per-replica
// capacity, and the engine scales on demand vs that capacity:
//
//   - Scale-up path (kvCache=0.80, queue=1): the tighter target sizes capacity
//     so demand ≈ supply → utilization ≈ 1.0 > scaleUpThreshold 0.85 → scale-up.
//   - No-scale path (kvCache=1.00, queue=100): the looser target sizes ~25% more
//     capacity per replica → utilization drops below 0.85 → no scale-up.
//
// The target must stay above the faked 0.30 usage in both arcs: below it there
// is no headroom, per-replica capacity collapses to zero, and the analyzer
// reports a shortfall it cannot size — so nothing scales at all.
//
// --fake-metrics replaces simulator runtime emission entirely; service traffic
// has no effect on the values the analyzer reads.
const fakeMetricsJSON = `{"kv-cache-usage":0.3,"waiting-requests":2,"running-requests":1}`

const (
	saturationConfigTemplate = `
model_id: ""
namespace: ""
kvCacheThreshold: %.2f
queueLengthThreshold: %d
kvSpareTrigger: %.2f
queueSpareTrigger: %d
scaleUpThreshold: %.2f
scaleDownBoundary: %.2f
analyzerName: %q
`

	// Scale-up arc. kvCacheThreshold is V2's KV utilization *target*: it divides
	// the measured usage to size per-replica capacity, so a tighter target means
	// less capacity per replica, higher utilization, and a scale-up. It must stay
	// ABOVE the faked 0.30 usage — a target below observed usage leaves no
	// headroom and collapses per-replica capacity to zero, at which point the
	// analyzer reports a shortfall it cannot size (supply 0, prc 0) and no
	// scale-up is possible. 0.80 against kv=0.30 yields demand ≈ supply, i.e.
	// utilization ≈ 1.0, comfortably past scaleUpThreshold.
	saturationKVCacheThreshold     = 0.80
	saturationQueueLengthThreshold = 1
	saturationKVSpareTrigger       = 0.01
	saturationQueueSpareTrigger    = 1
	saturationScaleUpThreshold     = 0.85
	saturationScaleDownBoundary    = 0.70

	// No-scale arc. The looser 1.00 target sizes ~25% more capacity per replica
	// than the scale-up arc, dropping utilization below scaleUpThreshold so the
	// engine sees no shortfall.
	saturationNoScaleKVCacheThreshold     = 1.00
	saturationNoScaleQueueLengthThreshold = 100
	saturationNoScaleKVSpareTrigger       = 0.00
	saturationNoScaleQueueSpareTrigger    = 0
)

// buildSaturationConfigYAML builds a valid saturation config entry for the requested analyzer mode.
func buildSaturationConfigYAML(analyzerName string) string {
	return fmt.Sprintf(saturationConfigTemplate, 0.80, 1, 0.20, 1, 0.85, 0.70, analyzerName)
}

// buildSaturationConfigYAMLWithThresholds builds a valid saturation config entry with explicit thresholds.
func buildSaturationConfigYAMLWithThresholds(analyzerName string, kvCacheThreshold float64, queueLengthThreshold int, kvSpareTrigger float64, queueSpareTrigger int, scaleUpThreshold float64, scaleDownBoundary float64) string {
	return fmt.Sprintf(
		saturationConfigTemplate,
		kvCacheThreshold,
		queueLengthThreshold,
		kvSpareTrigger,
		queueSpareTrigger,
		scaleUpThreshold,
		scaleDownBoundary,
		analyzerName,
	)
}

// saturationConfigMapName resolves the active saturation ConfigMap name from controller runtime env.
func saturationConfigMapName() string {
	// Match the controller's runtime config map name; discover by label first
	// since the deployment name can vary across overlays.
	deps, err := k8sClient.AppsV1().Deployments(cfg.WVANamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "control-plane=controller-manager",
	})
	if err != nil || len(deps.Items) == 0 {
		return config.SaturationConfigMapName()
	}
	return saturationConfigMapNameFromDeployment(&deps.Items[0])
}

// saturationConfigMapNameFromDeployment extracts SATURATION_CONFIG_MAP_NAME from manager container env.
func saturationConfigMapNameFromDeployment(dep *appsv1.Deployment) string {
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name != "manager" {
			continue
		}
		for _, e := range c.Env {
			if e.Name == "SATURATION_CONFIG_MAP_NAME" && e.Value != "" {
				return e.Value
			}
		}
	}
	return config.SaturationConfigMapName()
}

// expectAnalyzerPathLog is a Ginkgo helper: it Eventually-waits until WVA
// controller-manager logs show the model being processed on the V2 path. V2 is
// the sole analysis path since the V1 analyzer was removed, so there is no mode
// to select — the helper asserts the model is being processed at all.
// It uses testutils.PodLogsLabelSelectorContain for log collection.
func expectAnalyzerPathLog(modelID string) {
	GinkgoHelper()
	const controllerManagerLabel = "control-plane=controller-manager"
	const pattern = "Processing model (V2)"
	Eventually(func(g Gomega) {
		ok, logs, logErr := testutils.PodLogsLabelSelectorContain(ctx, k8sClient, cfg.WVANamespace, controllerManagerLabel, pattern, 120)
		g.Expect(logErr).NotTo(HaveOccurred())
		g.Expect(ok && strings.Contains(logs, modelID)).To(BeTrue())
	}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
}

var _ = Describe("Saturation analyzer path and status propagation", Label("full"), Ordered, func() {
	const (
		poolName     = "saturation-path-pool"
		modelSvcName = "saturation-path-ms"
		// modelDecodeDeployment is the Deployment name fixtures.CreateModelService creates
		// (name + "-decode"), matching llm-d model-service decode pods / labels.
		modelDecodeDeployment = modelSvcName + "-decode"
		serviceName           = modelSvcName + "-service"
		smName                = modelSvcName + "-monitor"
		// scalerBaseName is the annotated scaler's logical base. WVA discovers the
		// scaler and uses its OBJECT name as the variant_name label on
		// wva_desired_replicas — that is base+"-so" for a KEDA ScaledObject and
		// base+"-hpa" for an HPA. The decode pods must carry
		// llm-d.ai/variant=<scaler object name> for metric attribution, so variantName is
		// derived from the backend below.
		scalerBaseName = "saturation-path"
		hpaObjectName  = scalerBaseName + "-hpa"
		soObjectName   = scalerBaseName + "-so"
	)

	var (
		modelID         string
		cmName          string
		cmOriginal      *corev1.ConfigMap
		cmExistedBefore bool
		cmKey           string
		cmNamespace     string
		// variantName is the variant_name — the scaler's object name — stamped as the
		// decode pods' llm-d.ai/variant label so the collector attributes their
		// metrics to the variant. Set from the backend in BeforeAll.
		variantName string
	)

	BeforeAll(func() {
		if !cfg.UseSimulator {
			Skip("This suite needs the simulator runtime: set USE_SIMULATOR=true. " +
				"The suite uses llm-d-inference-sim's --fake-metrics flag, which real vLLM rejects.")
		}

		variantName = soObjectName

		modelID = cfg.ModelID
		cmName = saturationConfigMapName()
		// Use global saturation config for deterministic engine-path selection.
		// Namespace-local ConfigMap watch is opt-in/tracked and can race in e2e.
		cmNamespace = cfg.WVANamespace
		cmKey = "default"

		// Snapshot existing saturation config so the test can restore it.
		cm, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if err == nil {
			cmExistedBefore = true
			cmOriginal = cm.DeepCopy()
		} else if !errors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred(), "failed reading existing saturation configmap")
		}

		By("Creating model service + service + ServiceMonitor for saturation path test")
		_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
		err = fixtures.CreateModelServiceWithExtraArgs(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, poolName, modelID, variantName,
			cfg.UseSimulator, cfg.MaxNumSeqs, []string{"--fake-metrics", fakeMetricsJSON})
		Expect(err).NotTo(HaveOccurred())
		err = fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment, 8000)
		Expect(err).NotTo(HaveOccurred())
		err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for model service to be ready")
		Eventually(func(g Gomega) {
			dep, depErr := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(depErr).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 1))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Registering the saturation-path deployment with WVA via an annotated ScaledObject")
		// The ScaledObject's variantName matches the model service's variantName so the
		// decode pods' llm-d.ai/variant label and wva_desired_replicas variant_name align.
		// The 30 s scale-down stabilization window overrides the HPA default (300 s) so
		// the "does not scale up" It can wait for minReplicas within EventuallyLongSec.
		err = fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBaseName, modelDecodeDeployment, variantName, 1, 10, cfg.MonitoringNS,
			fixtures.WithScaledObjectWVAAnnotations(modelID, "30.0"),
			fixtures.WithScaledObjectScaleDownStabilizationWindow(30))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBaseName) })
	})

	AfterAll(func() {
		By("Restoring saturation ConfigMap state")
		if cmExistedBefore && cmOriginal != nil {
			// Replace the object in two steps (delete + create) instead of updating in place.
			// That avoids resourceVersion conflict retries; a brief gap without the ConfigMap
			// during suite teardown is acceptable for e2e.
			propagation := metav1.DeletePropagationBackground
			if err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Delete(ctx, cmName, metav1.DeleteOptions{
				PropagationPolicy: &propagation,
			}); err != nil && !errors.IsNotFound(err) {
				GinkgoWriter.Printf("Warning: failed to delete saturation configmap %s before restore: %v\n", cmName, err)
			}
			toCreate := saturationConfigMapForRecreate(cmOriginal)
			if _, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Create(ctx, toCreate, metav1.CreateOptions{}); err != nil {
				GinkgoWriter.Printf("Warning: failed to recreate saturation configmap %s: %v\n", cmName, err)
			}
		} else {
			_ = k8sClient.CoreV1().ConfigMaps(cmNamespace).Delete(ctx, cmName, metav1.DeleteOptions{})
		}

		By("Cleaning up saturation analyzer path resources")
		_ = crClient.Delete(ctx, &promoperator.ServiceMonitor{
			ObjectMeta: metav1.ObjectMeta{Name: smName, Namespace: cfg.MonitoringNS},
		})
		_ = k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, modelDecodeDeployment, metav1.DeleteOptions{})
	})

	It("uses V2 path when analyzerName is saturation", func() {
		By("Writing model-specific saturation config with analyzerName=saturation")
		err := upsertSaturationConfigEntry(ctx, cmNamespace, cmName, cmKey, buildSaturationConfigYAML("saturation"))
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for controller logs to show V2 processing for this model")
		expectAnalyzerPathLog(modelID)
	})

	It("still uses the V2 path when analyzerName is unset", func() {
		// V2 is the sole analysis path since the V1 analyzer was removed, so an
		// unset analyzerName no longer selects a different engine — it must simply
		// keep processing on V2 rather than stalling the model.
		By("Updating model-specific saturation config with analyzerName unset")
		err := upsertSaturationConfigEntry(ctx, cmNamespace, cmName, cmKey, buildSaturationConfigYAML(""))
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for controller logs to show V2 processing for this model")
		expectAnalyzerPathLog(modelID)
	})

	It("propagates saturation results into wva_desired_replicas for the variant", func() {
		// WVA no longer writes VA .status; its sole output is wva_desired_replicas.
		// expectWVADesiredReplicasConsumed observes that through the KEDA-managed
		// HPA's CurrentMetrics, which KEDA populates only after reading the metric
		// from Prometheus.
		By("Verifying wva_desired_replicas was emitted and consumed for the saturation-path variant")
		Eventually(func(g Gomega) {
			expectWVADesiredReplicasConsumed(g, cfg.LLMDNamespace, modelDecodeDeployment)
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

	It("does not scale the target deployment up for bounded below-threshold traffic", func() {
		By("Configuring conservative saturation thresholds to avoid scale-up")
		// Set thresholds before capturing the baseline so WVA has time to reconcile
		// while we wait for KEDA to settle. With the KEDA Prometheus query now using
		// exported_namespace (fixed), KEDA can act on wva_desired_replicas from the
		// prior It (which may have left a scale-up recommendation in Prometheus). We
		// must wait for WVA to re-evaluate and KEDA to read the new value before the
		// Consistently window starts — otherwise the HPA would fire a scale-up.
		err := upsertSaturationConfigEntry(
			ctx,
			cmNamespace,
			cmName,
			cmKey,
			buildSaturationConfigYAMLWithThresholds(
				"",
				saturationNoScaleKVCacheThreshold,
				saturationNoScaleQueueLengthThreshold,
				saturationNoScaleKVSpareTrigger,
				saturationNoScaleQueueSpareTrigger,
				saturationScaleUpThreshold,
				saturationScaleDownBoundary,
			),
		)
		Expect(err).NotTo(HaveOccurred())

		By("Verifying controller is processing this model on the V2 path")
		expectAnalyzerPathLog(modelID)

		By("Waiting for the pipeline to converge to a sustained minReplicas (drain any in-flight scale-up)")
		// A single Spec.Replicas <= 1 reading is NOT proof of convergence: the deployment
		// starts at minReplicas, so that check passes on the pre-existing state while a
		// scale-up recommendation left in flight by the prior It (default config:
		// queueLengthThreshold=1 vs faked queue=2 → scale-up) is still working through
		// WVA (≤15 s reconcile) → Prometheus → KEDA (5 s poll) → HPA. That stale
		// recommendation actuates a scale-up mid-assertion unless it has fully drained.
		//
		// Require the deployment to HOLD at <=1 continuously for stableWindow before
		// trusting it: any bump resets the stability clock, and stableWindow outlasts the
		// full pipeline latency (reconcile + poll + the 30 s scale-down stabilization set
		// on this ScaledObject) so the old recommendation is guaranteed drained.
		const stableWindow = 45 * time.Second
		var stableSince *time.Time
		Eventually(func(g Gomega) {
			dep, getErr := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(getErr).NotTo(HaveOccurred())
			g.Expect(dep.Spec.Replicas).NotTo(BeNil())
			now := time.Now()
			if *dep.Spec.Replicas > 1 {
				stableSince = nil // reset the stability clock on any in-flight scale-up
				GinkgoWriter.Printf("  Convergence (%s): replicas=%d (>1) — resetting stability clock\n", modelDecodeDeployment, *dep.Spec.Replicas)
				g.Expect(*dep.Spec.Replicas).To(BeNumerically("<=", int32(1)),
					"waiting for the in-flight scale-up recommendation to drain")
				return
			}
			if stableSince == nil {
				stableSince = &now
			}
			held := now.Sub(*stableSince)
			GinkgoWriter.Printf("  Convergence (%s): replicas<=1 held for %s (need %s)\n", modelDecodeDeployment, held.Round(time.Second), stableWindow)
			g.Expect(held).To(BeNumerically(">=", stableWindow),
				"waiting for minReplicas to hold long enough to confirm the pipeline converged")
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Capturing baseline target deployment replicas before steady-state assertion")
		var baseline int32
		dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(dep.Spec.Replicas).NotTo(BeNil())
		baseline = *dep.Spec.Replicas
		GinkgoWriter.Printf("  Negative-path baseline (%s): replicas=%d\n", modelDecodeDeployment, baseline)

		By("Verifying the target deployment does not scale above baseline")
		Consistently(func(g Gomega) {
			dep, getErr := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(getErr).NotTo(HaveOccurred())
			current := int32(0)
			if dep.Spec.Replicas != nil {
				current = *dep.Spec.Replicas
			}
			GinkgoWriter.Printf("  Negative-path progress (%s): replicas=%d baseline=%d\n", modelDecodeDeployment, current, baseline)
			g.Expect(current).To(BeNumerically("<=", baseline),
				"bounded below-threshold traffic should not scale the target deployment above baseline")
		}, time.Duration(cfg.EventuallyMediumSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

	It("crosses the saturation threshold with bounded requests and raises wva_desired_replicas", func() {
		var baseline int32

		By("Capturing baseline target deployment replicas before scale-up trigger")
		Eventually(func(g Gomega) {
			dep, getErr := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(getErr).NotTo(HaveOccurred())
			g.Expect(dep.Spec.Replicas).NotTo(BeNil())
			baseline = *dep.Spec.Replicas
			GinkgoWriter.Printf("  Scale-up baseline (%s): replicas=%d\n", modelDecodeDeployment, baseline)
		}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Configuring aggressive saturation thresholds and unsetting analyzerName")
		err := upsertSaturationConfigEntry(
			ctx,
			cmNamespace,
			cmName,
			cmKey,
			buildSaturationConfigYAMLWithThresholds(
				"",
				saturationKVCacheThreshold,
				saturationQueueLengthThreshold,
				saturationKVSpareTrigger,
				saturationQueueSpareTrigger,
				saturationScaleUpThreshold,
				saturationScaleDownBoundary,
			),
		)
		Expect(err).NotTo(HaveOccurred())

		By("Verifying controller is processing this model on the V2 path")
		expectAnalyzerPathLog(modelID)

		By("Asserting KEDA actuates scale-up above baseline")
		// A 0.80 KV target against faked kv=0.30 sizes per-replica capacity so that
		// demand ≈ supply — utilization ≈ 1.0, past scaleUpThreshold=0.85 — which
		// deterministically drives a scale-up; KEDA consumes
		// wva_desired_replicas and drives the Deployment above its baseline. Assert the
		// observable Deployment replica count — the ground truth — rather than the KEDA
		// HPA CurrentMetrics surface, which only proves the metric was consumed.
		Eventually(func(g Gomega) {
			dep, getErr := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(getErr).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">", baseline),
				"above-threshold traffic should scale the target Deployment above baseline")
		}, time.Duration(cfg.ScaleUpTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

})

// upsertSaturationConfigEntry creates or updates a saturation ConfigMap data entry.
func upsertSaturationConfigEntry(ctx context.Context, cmNamespace, cmName, key, value string) error {
	cmClient := k8sClient.CoreV1().ConfigMaps(cmNamespace)
	cm, err := cmClient.Get(ctx, cmName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			newCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      cmName,
					Namespace: cmNamespace,
				},
				Data: map[string]string{key: value},
			}
			_, createErr := cmClient.Create(ctx, newCM, metav1.CreateOptions{})
			return createErr
		}
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[key] = value
	_, err = cmClient.Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// saturationConfigMapForRecreate returns a copy of orig suitable for Create after Delete,
// with apiserver-owned fields cleared so admission succeeds.
func saturationConfigMapForRecreate(orig *corev1.ConfigMap) *corev1.ConfigMap {
	cm := orig.DeepCopy()
	cm.ResourceVersion = ""
	cm.UID = ""
	cm.Generation = 0
	cm.CreationTimestamp = metav1.Time{}
	cm.DeletionTimestamp = nil
	cm.DeletionGracePeriodSeconds = nil
	cm.ManagedFields = nil
	cm.Finalizers = nil
	return cm
}
