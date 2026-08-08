package e2e

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

// Scale-from-zero placement: the wake must be refused when the variant's
// accelerator has no room, and allowed when it does.
//
// This is the only suite that exercises the DENY branch of the capacity check.
// The others cannot: their fixtures set no nodeSelector, so WVA cannot resolve an
// accelerator, the candidate contributes no demand, and FitsGPUBudget returns
// true without evaluating anything. Three conditions all have to hold for a
// denial to be reachable, and each was broken at some point:
//
//  1. the variant's accelerator resolves (nodeSelector on a GPU product label);
//  2. a GPU-usage snapshot exists — which requires at least one ACTIVE variant,
//     since the saturation engine publishes only measured cycles; and
//  3. the usage is attributed to the same pool key the limits use.
//
// So the suite runs a live "occupier" workload that consumes its accelerator
// entirely, then asserts that a parked variant on that accelerator is refused.
//
// PRECONDITION — a SECOND InferencePool. The occupier and the variant under test
// cannot share one, and this is structural rather than a fixture detail:
// scale-from-zero triggers on EPP flow-control queueing, which only happens when
// the pool has NO ready endpoints, while the capacity check needs a usage
// snapshot, which only exists when some WVA-managed variant is running and
// measured. In one pool those requirements are mutually exclusive — the occupier
// gives the pool ready endpoints, so requests are dispatched to it instead of
// queueing, and no wake is ever considered. Every fixture currently stamps
// llm-d.ai/guide: optimized-baseline, so they all land in the same pool.
//
// The suite therefore skips until the e2e infra provides a second pool for the
// occupier. It is kept, rather than deleted, because everything else in it is
// correct and the missing piece is one label plus an InferencePool/EPP.
var _ = Describe("Scale-From-Zero placement against GPU capacity", Serial, Label("full"), Ordered, func() {
	const (
		// Product label as the kind emulator sets it on nodes; pools are keyed by
		// its normalized short name (A100).
		fullAccelerator = "NVIDIA-A100-PCIE-80GB"

		occupierSvc  = "sfz-cap-occupier"
		occupierVar  = "sfz-cap-occupier-va"
		occupierScal = "sfz-cap-occupier"

		deniedSvc  = "sfz-cap-denied"
		deniedVar  = "sfz-cap-denied-va"
		deniedScal = "sfz-cap-denied"

		poolName = "sfz-cap-pool"
	)
	occupierDeploy := occupierSvc + "-decode"
	deniedDeploy := deniedSvc + "-decode"

	var triggerJobName string

	// gpusPerNode is what the kind emulator allocates per labelled node. The
	// occupier claims all of them so the pool has nothing left.
	const gpusPerNode = 4

	BeforeAll(func() {
		if !cfg.ScaleToZeroEnabled {
			Skip("This suite requires EPP flow-control queuing: set SCALE_TO_ZERO_ENABLED=true")
		}

		skipUnlessSecondInferencePool()

		By("Cleaning up any existing scale-from-zero test resources")
		cleanupScaleFromZeroResources()

		By("Waiting for EPP pods to be ready")
		Eventually(func(g Gomega) {
			pods, err := utils.FindExistingEPPPods(ctx, k8sClient, cfg.LLMDNamespace, cfg.EPPServiceName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods).ToNot(BeEmpty())
		}).Should(Succeed())

		By("Applying InferenceObjective for GIE queuing")
		poolRefName := cfg.PoolName
		if poolRefName == "" {
			poolRefName = strings.TrimSuffix(cfg.EPPServiceName, "-epp")
		}
		ioApplied, errIO := fixtures.EnsureInferenceObjective(ctx, crClient, cfg.LLMDNamespace, poolRefName)
		Expect(errIO).NotTo(HaveOccurred())
		if !ioApplied {
			Skip("InferenceObjective API not available on cluster")
		}

		By("Running an occupier that consumes the whole " + fullAccelerator + " pool")
		// Also serves condition (2): an active variant is what makes the
		// saturation engine publish a usage snapshot at all.
		occupierModel := cfg.ModelID + "-occupier"
		Expect(fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace, occupierSvc, poolName,
			occupierModel, occupierVar, cfg.UseSimulator, cfg.MaxNumSeqs,
			fixtures.WithAcceleratorNodeSelector(fullAccelerator),
			fixtures.WithGPURequest(gpusPerNode),
		)).To(Succeed())
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, occupierScal, occupierDeploy,
			occupierVar, 1, 10, cfg.MonitoringNS,
			fixtures.WithScaledObjectWVAAnnotations(occupierModel, "10.0"),
			fixtures.WithExternalScalerPushTrigger(externalScalerAddress()),
		)).To(Succeed())

		By("Creating a parked variant on the exhausted accelerator")
		createParkedVariant(deniedSvc, deniedDeploy, deniedVar, deniedScal, poolName, fullAccelerator)

		DeferCleanup(func() {
			for _, s := range []string{occupierScal, deniedScal} {
				_ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, s)
			}
			for _, d := range []string{occupierDeploy, deniedDeploy} {
				_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, d, metav1.DeleteOptions{})
			}
			if triggerJobName != "" {
				propagation := metav1.DeletePropagationBackground
				_ = k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Delete(ctx, triggerJobName, metav1.DeleteOptions{
					PropagationPolicy: &propagation,
				})
			}
		})

		By("Waiting for the occupier to be running so its GPUs are accounted for")
		Eventually(func(g Gomega) {
			deploy, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, occupierDeploy, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(deploy.Status.ReadyReplicas).To(BeNumerically(">", 0),
				"the occupier must actually run, or its GPUs are not counted as used")
		}, time.Duration(cfg.ScaleUpTimeout)*time.Second, time.Duration(cfg.PollIntervalSlowSec)*time.Second).
			Should(Succeed())
	})

	It("refuses to wake a variant whose accelerator is full", func() {
		By("Sending requests so both parked variants have demand")
		triggerStart := time.Now()
		triggerJobName = fmt.Sprintf("sfz-cap-trigger-%d", time.Now().Unix())
		job := createScaleFromZeroTriggerJob(triggerJobName, cfg.LLMDNamespace, cfg.EPPServiceName, cfg.ModelID)
		_, err := k8sClient.BatchV1().Jobs(cfg.LLMDNamespace).Create(ctx, job, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			podList, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "job-name=" + triggerJobName,
			})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(podList.Items).ToNot(BeEmpty())
			g.Expect(podList.Items[0].Status.Phase).To(Or(Equal(corev1.PodRunning), Equal(corev1.PodSucceeded)))
		}).Should(Succeed())

		By("Waiting for the engine to report the refusal, naming the capacity reason")
		// The engine's own verdict, not the absence of a scale-up: a target that
		// merely has not been woken YET looks identical from the outside.
		expectScaleFromZeroRefusal(deniedVar+"-so", triggerStart, "no-capacity")

		By("Verifying the denied variant stayed at zero")
		Consistently(func(g Gomega) {
			deploy, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deniedDeploy, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			var replicas int32
			if deploy.Spec.Replicas != nil {
				replicas = *deploy.Spec.Replicas
			}
			g.Expect(replicas).To(BeZero(), "a variant that cannot be placed must not be woken")
		}, 20*time.Second, 5*time.Second).Should(Succeed())
	})

})

// createParkedVariant builds a model service pinned to an accelerator, parks it
// at zero, and puts an annotated ScaledObject in front of it.
func createParkedVariant(svcName, deployName, variantName, scalerName, poolName, accelerator string) {
	GinkgoHelper()
	Expect(fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace, svcName, poolName,
		cfg.ModelID, variantName, cfg.UseSimulator, cfg.MaxNumSeqs,
		fixtures.WithAcceleratorNodeSelector(accelerator),
		fixtures.WithGPURequest(1),
	)).To(Succeed())

	Eventually(func(g Gomega) {
		deploy, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, deployName, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		deploy.Spec.Replicas = ptr.To(int32(0))
		_, err = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Update(ctx, deploy, metav1.UpdateOptions{})
		g.Expect(err).NotTo(HaveOccurred())
	}, time.Duration(cfg.EventuallyShortSec)*time.Second, time.Duration(cfg.PollIntervalQuickSec)*time.Second).
		Should(Succeed())

	Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerName, deployName, variantName,
		0, 10, cfg.MonitoringNS,
		fixtures.WithScaledObjectWVAAnnotations(cfg.ModelID, "30.0"),
		fixtures.WithExternalScalerPushTrigger(externalScalerAddress()),
	)).To(Succeed())
}

// expectScaleFromZeroRefusal waits until the engine reports that it declined to
// wake variantName, for the given reason.
//
// The refusal is logged once per verdict change rather than once per poll, so the
// window has to start before the trigger.
func expectScaleFromZeroRefusal(variantName string, since time.Time, reason string) {
	GinkgoHelper()
	const controllerManagerLabel = "control-plane=controller-manager"
	const pattern = "no variant woken for a model with pending requests"
	Eventually(func(g Gomega) {
		sinceSeconds := int64(time.Since(since).Seconds()) + 1
		ok, logs, logErr := utils.PodLogsLabelSelectorContain(ctx, k8sClient, cfg.WVANamespace, controllerManagerLabel, pattern, sinceSeconds)
		g.Expect(logErr).NotTo(HaveOccurred())
		g.Expect(ok).To(BeTrue(), "the engine never reported declining to wake anything")
		matched := false
		for _, line := range strings.Split(logs, "\n") {
			if strings.Contains(line, pattern) && strings.Contains(line, reason) {
				matched = true
				break
			}
		}
		g.Expect(matched).To(BeTrue(), "no refusal line carried reason %q", reason)
	}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
}

// skipUnlessSecondInferencePool skips when the cluster has only the one
// InferencePool every fixture joins. See the note on the suite: the occupier has
// to live in a different pool, or it supplies the ready endpoints that stop
// requests queueing and the scenario cannot arise at all.
func skipUnlessSecondInferencePool() {
	GinkgoHelper()
	pools := &unstructured.UnstructuredList{}
	pools.SetAPIVersion("inference.networking.k8s.io/v1")
	pools.SetKind("InferencePoolList")
	if err := crClient.List(ctx, pools, client.InNamespace(cfg.LLMDNamespace)); err != nil {
		Skip("could not list InferencePools: " + err.Error())
	}
	if len(pools.Items) < 2 {
		Skip("this suite needs a second InferencePool for the occupier, so it does not supply " +
			"ready endpoints to the pool under test; only " + fmt.Sprint(len(pools.Items)) + " present")
	}
}
