package e2e

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// fleetObservation records what the Deployment reported across BOTH windows
// below, so a failure can say WHEN the fleet left its target instead of only
// that it is not there now.
//
// The two windows check different fields for different reasons: the guard
// watches Status.Replicas for adoption and is deliberately one-sided, and the
// assertion watches Spec.Replicas for the regression. A fleet that falls below
// target DURING the guard therefore trips neither -- the guard ignores a low
// count by design, and by the time the assertion opens the fall has already
// happened, so it fails on its first poll with "1 is not >= 2" and no history.
//
// That is what made this spec unreadable: measured on a fresh cluster it passed
// twice at ~370s and failed once at 60s, and the failing run's Consistently
// gave up after 0.003s. "Scaled down thirty seconds ago" and "was never staged"
// produce the identical message, and they need opposite fixes.
type fleetObservation struct {
	start   time.Time
	last    string
	samples []string
	fellAt  time.Duration
	fell    bool
}

// record samples the Deployment. Transitions only: a poll that reports what the
// last one did adds nothing, and at a 1s interval over a 5-minute window the
// unfiltered list would be the thing nobody reads.
func (f *fleetObservation) record(dep *appsv1.Deployment, target int32) {
	spec := int32(0)
	if dep.Spec.Replicas != nil {
		spec = *dep.Spec.Replicas
	}
	line := fmt.Sprintf("spec=%d status=%d ready=%d", spec, dep.Status.Replicas, dep.Status.ReadyReplicas)
	if line != f.last {
		f.samples = append(f.samples,
			fmt.Sprintf("t+%s %s", time.Since(f.start).Round(time.Second), line))
		f.last = line
	}
	if !f.fell && spec < target {
		f.fell = true
		f.fellAt = time.Since(f.start).Round(time.Second)
	}
}

// report is the failure annotation: when the fleet first fell, and everything
// that changed on the way there.
func (f *fleetObservation) report(guard time.Duration) string {
	timeline := strings.Join(f.samples, " | ")
	if !f.fell {
		return "the fleet never fell below its target while observed; timeline: " + timeline
	}
	when := fmt.Sprintf("spec.replicas first fell below the target at t+%s", f.fellAt)
	if f.fellAt <= guard {
		return fmt.Sprintf("%s -- INSIDE the %s adoption guard, so it had already happened before this "+
			"assertion opened, and the fast failure is the tail of it rather than a fresh scale-down; "+
			"timeline: %s", when, guard, timeline)
	}
	return fmt.Sprintf("%s, after the %s adoption guard; timeline: %s", when, guard, timeline)
}

// Supply must never describe a fleet larger than the scale target is committed
// to. When it does, the optimizer is credited with spare capacity that an
// in-flight scale-down has already claimed and removes the same replicas twice,
// which is how a variant under sustained load lands on one replica. See
// steadystate.clampReplicaCountToScaleTarget.
//
// Reproducing that from a real scale-down would mean racing a termination
// window: a Deployment's status.replicas drops the moment a pod is marked for
// deletion, while the pod keeps serving and keeps being scraped until its grace
// period expires. The race is the bug's natural habitat and a bad test.
//
// This stages the same INPUT without the timing: an extra serving replica that
// no ReplicaSet owns. The collector still attributes it to the variant — it
// resolves a pod's variant by walking ownerReferences up to a Deployment, and
// this pod carries one — so it reports real capacity. But Status.Replicas is
// summed from the ReplicaSets' pod counts, and none of them owns it, so the
// scale target does not count it. Measured supply therefore exceeds the
// committed count, which is exactly the state a condemned-but-still-scraping
// replica produces, held still. See createUnownedReplica for why the owner has
// to be the Deployment and nothing else.
const concededFakeMetricsJSON = `{"kv-cache-usage":0.3,"running-requests":1,"waiting-requests":0}`

// Production-shaped thresholds, because the arithmetic this guards is only
// wrong in a particular band and the run that exposed it used these.
//
// With kv-cache-usage 0.3 and a two-replica target, a third reporting replica
// takes spare capacity from 2×P − 3×0.3×P/0.7 = 0.71×P (no replica to give up)
// to 3×P − 3×0.3×P/0.7 = 1.71×P — one whole replica of slack, on top of the one
// the target has already conceded. Unclamped that recommends 1; clamped it holds
// at 2.
const (
	concededKvCacheThreshold     = 0.80
	concededQueueLengthThreshold = 50
	concededScaleUpThreshold     = 0.85
	concededScaleDownBoundary    = 0.70
)

var _ = Describe("Scale-down with supply beyond the scale target", Label("full"), Ordered, func() {
	const (
		poolName              = "conceded-pool"
		modelSvcName          = "conceded-ms"
		modelDecodeDeployment = modelSvcName + "-decode"
		scalerBaseName        = "conceded"
		extraPodName          = modelSvcName + "-unowned-replica"
		targetReplicas        = 2
	)

	var (
		modelID         string
		cmName          string
		cmNamespace     string
		cmKey           string
		cmOriginal      *corev1.ConfigMap
		cmExistedBefore bool
		variantName     string
	)

	BeforeAll(func() {
		if !cfg.UseSimulator {
			Skip("This suite needs the simulator runtime: set USE_SIMULATOR=true. " +
				"It uses llm-d-inference-sim's --fake-metrics flag, which real vLLM rejects.")
		}

		modelID = cfg.ModelID
		cmName = scalingPolicyConfigMapName()
		cmNamespace = cfg.WVANamespace
		cmKey = defaultConfigKey
		variantName = scalerBaseName + "-so"

		By("Snapshotting existing saturation ConfigMap for restore in AfterAll")
		cm, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if err == nil {
			cmExistedBefore = true
			cmOriginal = cm.DeepCopy()
		} else if !errors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred(), "failed reading existing saturation configmap")
		}

		By("Creating model service + service + ServiceMonitor")
		_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
		Expect(fixtures.CreateModelServiceWithExtraArgs(
			ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, poolName, modelID,
			cfg.UseSimulator, cfg.MaxNumSeqs,
			[]string{"--fake-metrics", concededFakeMetricsJSON})).To(Succeed())
		Expect(fixtures.EnsureService(
			ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment, 8000,
		)).To(Succeed())
		Expect(fixtures.EnsureServiceMonitor(
			ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment,
		)).To(Succeed())

		By("Installing saturation config with production-shaped thresholds")
		cfgYAML := buildSaturationConfigYAMLWithThresholds(
			"saturation",
			concededKvCacheThreshold, concededQueueLengthThreshold,
			concededScaleUpThreshold, concededScaleDownBoundary,
		)
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, cmKey, cfgYAML)).To(Succeed())

		By("Registering the deployment with WVA via an annotated ScaledObject")
		// minReplicaCount is 1 on purpose: the assertion is that the fleet does
		// NOT fall to it. A floor of 2 would pass whether or not the bug is fixed.
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBaseName, modelDecodeDeployment, variantName, 1, 10, cfg.MonitoringNS,
			fixtures.WithWVATriggerMetadata(modelID, "30.0"),
			// LONG on purpose, and it is what stops this spec being a race.
			//
			// The fleet is staged by hand at two replicas while WVA's standing
			// recommendation is still 1, so a short window starts a timer against
			// that stale value; if the new recommendation has not propagated when
			// it expires, KEDA takes the fleet back down and the third replica
			// stops reporting -- destroying the premise mid-measurement. Measured
			// at 30s: the fleet fell at t+25s with WVA saying curr:2 tgt:2.
			//
			// Long enough to outlast the assertion below, so the fleet holds
			// still for the whole measurement. The assertion is on what WVA
			// PUBLISHES, not on the fleet, so holding the fleet still costs no
			// coverage -- see the Consistently at the end of this spec.
			fixtures.WithScaledObjectScaleDownStabilizationWindow(600))).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBaseName) })
	})

	AfterAll(func() {
		By("Restoring saturation ConfigMap state")
		if cmExistedBefore && cmOriginal != nil {
			propagation := metav1.DeletePropagationBackground
			if err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Delete(ctx, cmName, metav1.DeleteOptions{
				PropagationPolicy: &propagation,
			}); err != nil && !errors.IsNotFound(err) {
				GinkgoWriter.Printf("Warning: failed to delete saturation configmap %s before restore: %v\n", cmName, err)
			}
			toCreate := saturationConfigMapForRecreate(cmOriginal)
			_, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Create(ctx, toCreate, metav1.CreateOptions{})
			if err != nil && !errors.IsAlreadyExists(err) {
				GinkgoWriter.Printf("Warning: failed to restore saturation configmap %s: %v\n", cmName, err)
			}
		}
		_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
	})

	It("holds the fleet at its target when an unowned replica also reports capacity", func() {
		By("Bringing the deployment to its target replica count")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 1))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())

		scaleDeployment(cfg.LLMDNamespace, modelDecodeDeployment, targetReplicas)
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically("==", targetReplicas))
			// TOTAL replicas as well, not just the ready ones. Status.Replicas
			// counts Pods that are still TERMINATING, and the guard below reads
			// that same field: scaling down from a larger fleet reaches
			// ReadyReplicas==2 while the third Pod is still going away, so the
			// guard sees 3 and reports adoption -- a staging failure for a
			// condition that is merely mid-scale-down.
			//
			// Invisible when this spec runs alone, because the Deployment starts
			// at one replica and there is nothing to terminate. It only appears
			// after a spec that left the fleet larger, which is why it failed in
			// the full suite and passed on its own.
			g.Expect(dep.Status.Replicas).To(BeNumerically("==", targetReplicas),
				"a Pod from an earlier scale-down is still terminating, and it counts here")
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())

		By("Proving WVA is actually driving this ScaledObject before relying on a negative")
		// Everything below is a Consistently on a value NOT changing. That holds
		// just as well when nothing is running: a miswired trigger, an unreachable
		// controller or a wrong modelID would leave spec.replicas parked at the 2
		// set above and the spec would pass while testing nothing. KEDA only
		// populates the HPA's external CurrentMetrics after it has read the metric
		// from WVA, so this pins the pipeline as live first.
		Eventually(func(g Gomega) {
			expectWVADesiredReplicasConsumed(g, cfg.LLMDNamespace, modelDecodeDeployment)
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())

		// Prometheus is needed from here on -- by the premise guard below as well
		// as by the assertion -- so a missing client is settled before anything
		// is staged rather than after.
		pc := promClientForCheck()
		if pc == nil {
			Skip("no Prometheus client could be built, so WVA's recommendation cannot be read")
		}
		demandQuery := fmt.Sprintf(
			"max(wva_analyzer_demand{analyzer_name=\"saturation\",model_name=%q,exported_namespace=%q})",
			modelID, cfg.LLMDNamespace)

		By("Sampling the demand the owned replicas alone produce")
		// The baseline the premise guard is measured against. Taken here, while
		// only the ReplicaSet's Pods report, so any rise it later sees belongs to
		// the unowned Pod and to nothing else.
		var baselineDemand float64
		Eventually(func(g Gomega) {
			v, err := pc.QueryWithRetry(ctx, demandQuery)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(v).To(BeNumerically(">", 0),
				"the owned replicas are not reporting demand yet, so there is no baseline to "+
					"measure the third replica against")
			baselineDemand = v
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())

		By("Adding a serving replica the Deployment does not own")
		createUnownedReplica(cfg.LLMDNamespace, modelDecodeDeployment, extraPodName)
		DeferCleanup(func() {
			propagation := metav1.DeletePropagationBackground
			_ = k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Delete(ctx, extraPodName, metav1.DeleteOptions{
				PropagationPolicy: &propagation,
			})
		})
		Eventually(func(g Gomega) {
			pod, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).Get(ctx, extraPodName, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())

		By("Confirming the Deployment still reports only the replicas it owns")
		// If the ReplicaSet adopted the extra pod, the premise is gone and the
		// assertion below would pass for the wrong reason.
		//
		// Bounded on ONE side only. Adoption is the failure this guard exists for
		// and it shows up as a count ABOVE the target. A count BELOW it is the
		// regression itself, and that belongs to the assertion after this one:
		// an equality check here would fire first and report a staging problem
		// for what is actually the bug under test.
		// Observed across both windows so the assertion below can say when the
		// fleet left its target. Started here rather than at the assertion,
		// because the interesting departures happen during this guard.
		const guardWindow = 30 * time.Second
		fleet := &fleetObservation{start: time.Now()}

		Consistently(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			fleet.record(dep, targetReplicas)
			g.Expect(dep.Status.Replicas).To(BeNumerically("<=", targetReplicas),
				"the ReplicaSet adopted the extra pod, so this test is not staging the condition it claims")
		}, guardWindow, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Confirming WVA actually counts the unowned replica")
		// Running is not reporting. The Pod has to be scraped, and the collector
		// has to attribute it to this variant, before any of its capacity reaches
		// the optimizer -- and nothing above checks that. The adoption guard is
		// one-sided and reads Status.Replicas, which does not count an unowned
		// Pod in either direction, so it cannot catch this either.
		//
		// Measured on the CI run that produced this guard: the analyzer logged
		// replica-capacity-decision for the two OWNED Pods only, demand came in
		// at 4 rather than ~6, spare capacity was 12 - 4/0.7 = 6.29 -- just over
		// one replica -- and WVA correctly recommended 1. The assertion below
		// then reported a clamp regression for a premise that was never staged.
		//
		// Relative rather than a constant: demand is replicas x kv-cache-usage x
		// P, so a third reporting replica raises it by half. A 1.25x floor sits
		// clear of both the two-replica level and of sampling noise, and does not
		// hard-code P, which the simulator derives.
		Eventually(func(g Gomega) {
			v, err := pc.QueryWithRetry(ctx, demandQuery)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(v).To(BeNumerically(">=", baselineDemand*1.25),
				"the unowned replica is Running but WVA is not counting it: demand is %v against a "+
					"baseline of %v from the owned replicas alone. The premise of this spec is not "+
					"staged, and the assertion below would blame the clamp for it", v, baselineDemand)
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())

		By("Asserting the extra replica's capacity is never treated as removable")
		// The regression: supply over three reporting replicas yields a full
		// replica of spare on top of the one the target already conceded, and the
		// recommendation drops to minReplicaCount.
		//
		// ASSERTED ON WHAT WVA PUBLISHES, not on spec.replicas.
		//
		// spec.replicas is KEDA's. It follows WVA, but on KEDA's schedule and
		// through the HPA's stabilisation window, so reading it measures a
		// pipeline several seconds wide and calls its lag a regression. That is
		// what made this spec flaky: with the window at 30s, a scale-up staged by
		// hand starts a timer against WVA's PREVIOUS recommendation of 1, and if
		// the new one has not propagated when it expires the fleet drops -- while
		// WVA is saying curr:2 tgt:2 no-change. Measured exactly that, at t+25s.
		//
		// The window is long now (see the ScaledObject above), which keeps the
		// fleet still so the third replica keeps reporting. That deliberately
		// makes spec.replicas useless as the assertion -- it could no longer fall
		// during this window even if the clamp were gone -- so the assertion
		// moves to the number the regression is actually about.
		//
		// Prometheus rather than the HPA's CurrentMetrics: KEDA reports the
		// external metric as an AverageValue, total over current replicas, so a
		// correct recommendation of 2 across two replicas reads as 1 there. An
		// earlier attempt waited on that and timed out for five minutes against a
		// perfectly healthy controller.
		desired := fmt.Sprintf("wva_desired_replicas{variant_name=%q,exported_namespace=%q}",
			variantName, cfg.LLMDNamespace)

		// The series has to EXIST before it can be asserted on. QueryWithRetry
		// gives up after five attempts, about a second and a half, which is far
		// shorter than the gap between the fleet being staged and the first
		// scrape carrying a recommendation for it. Without this the Consistently
		// fails on its first poll with "timed out waiting for the condition" --
		// a missing series reported as a scale-down. Two runs in three died that
		// way, both at ~42s, while the run that got a series passed the whole
		// window.
		By("Waiting for WVA's recommendation to be queryable at all")
		Eventually(func(g Gomega) {
			_, err := pc.QueryWithRetry(ctx, desired)
			g.Expect(err).NotTo(HaveOccurred(),
				"wva_desired_replicas is not queryable for this variant yet; without it the "+
					"assertion below cannot tell a low recommendation from a missing one")
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())

		Consistently(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			fleet.record(dep, targetReplicas)

			v, err := pc.QueryWithRetry(ctx, desired)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(v).To(BeNumerically(">=", float64(targetReplicas)),
				"WVA recommended fewer replicas than the target while an unowned replica was "+
					"reporting: its capacity was counted as spare. %s", fleet.report(guardWindow))
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())
	})
})

// scaleDeployment sets spec.replicas directly.
func scaleDeployment(namespace, name string, replicas int32) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		dep, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		dep.Spec.Replicas = &replicas
		_, err = k8sClient.AppsV1().Deployments(namespace).Update(ctx, dep, metav1.UpdateOptions{})
		g.Expect(err).NotTo(HaveOccurred())
	}, 60*time.Second, 2*time.Second).Should(Succeed())
}

// createUnownedReplica clones the Deployment's pod template into a standalone
// Pod that serves and is scraped like any other replica, but that the Deployment
// does not count.
//
// The owner reference has to satisfy three constraints at once, and getting it
// wrong makes this spec pass while proving nothing:
//
//   - The COLLECTOR must attribute the pod to the variant. It walks
//     ownerReferences for a supported ancestor and accepts only Deployment or
//     LeaderWorkerSet (collector/locator/walk.go); anything else resolves to no
//     scale target and the pod is dropped as unattributed. A reference to the
//     Service reads naturally and is exactly wrong: the pod then contributes no
//     capacity, the measured count never exceeds the target, the clamp never
//     fires, and the assertion below holds whether or not the fix is present.
//   - The REPLICASET must not adopt it. Adoption applies to matching pods with no
//     CONTROLLER owner; an adopted pod would be counted and then deleted to
//     satisfy the replica count, destroying the premise. Any controller owner
//     blocks it, including this one.
//   - The DEPLOYMENT must still not count it. Status.Replicas is summed from the
//     ReplicaSets' own pod counts, and no ReplicaSet owns this pod, so pointing
//     the reference at the Deployment does not inflate the count it is being
//     compared against.
//
// Owning it by the Deployment satisfies all three, and garbage-collects the pod
// with the fixture so a failed run cannot strand a simulator pod holding a GPU.
func createUnownedReplica(namespace, deploymentName, podName string) {
	GinkgoHelper()
	dep, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())

	controller := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        podName,
			Namespace:   namespace,
			Labels:      dep.Spec.Template.Labels,
			Annotations: dep.Spec.Template.Annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       dep.Name,
				UID:        dep.UID,
				Controller: &controller,
			}},
		},
		Spec: *dep.Spec.Template.Spec.DeepCopy(),
	}
	_, err = k8sClient.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return
	}
	Expect(err).NotTo(HaveOccurred())
}
