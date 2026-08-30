package e2e

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
	testutils "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

// WHOSE LOAD IS THE BRIDGE CARRYING, and is it one of the variant's own
// replicas?
//
// A warm pool Pod is owned by the POOL's workload, so the ownerReference walk
// that identifies every other Pod reaches the pool and not the model. While it
// is lent it is serving one variant's traffic, and that traffic has to be
// counted against that variant -- a bridge exists precisely because a variant is
// short, so its load is the load that most needs counting.
//
// Two things have to be right for that, and they pull in opposite directions:
//
//   - the bridge's metrics must be ATTRIBUTED to the variant, under the name
//     everything downstream keys a variant by, which is the ScaledObject's;
//   - the bridge must NOT be counted among the variant's own SERVING replicas,
//     because the pool reads that count to decide whether the ordinary replicas
//     have arrived and its Pod can go home.
//
// Both were wrong, in ways that only a cluster showed. The lending was published
// under the SCALE TARGET's name, which matched no analyzer row, so the bridge
// became a phantom variant of its own. Fixing that made the serving count reach
// the pool for the first time -- carrying the bridge inside it, so the pool was
// told the replicas had arrived one replica early.
//
// The fixtures below therefore give the variant and its scale target DIFFERENT
// names on purpose. Every unit test that missed this used one string for both.
var _ = Describe("Warm pool - a bridge is measured as the variant it serves", Label("full"), Label("warmpool-attribution"), Ordered, Serial, func() {
	const (
		attrPool     = "e2e-pool-attr"
		modelSvcName = "e2e-attr-ms"
		fixturePool  = "e2e-attr-pool"

		// THE TWO NAMES. scalerName is the ScaledObject, which is what the
		// collector resolves for a Pod and what the analyzer keys its rows by;
		// scaleTarget is the Deployment underneath it. On a real deployment they
		// differ -- qwen-decode-wva scales qwen-decode -- and everything this
		// spec asserts is about telling them apart.
		scalerName  = "e2e-attr"
		scaleTarget = modelSvcName + "-decode"

		tenantDriver = "e2e-attr-tenant"

		attrFakeMetrics   = `{"kv-cache-usage":0.9,"running-requests":8,"waiting-requests":4}`
		scaleUpThreshold  = 0.30
		scaleDownBoundary = 0.20
		kvCacheThreshold  = 0.80
		queueThreshold    = 50

		settle       = 5 * time.Minute
		sinceRestart = int64(900)
	)

	var (
		ctx        context.Context
		controller fixtures.ControllerDeployment
		poolSpec   fixtures.WarmPoolSpec
		poolPod    string
		poolPodIP  string
		modelNode  string
	)

	controllerLog := func() string {
		_, logs, err := testutils.PodLogsLabelSelectorContain(ctx, k8sClient, controller.Namespace,
			"control-plane=controller-manager", "", sinceRestart)
		if err != nil {
			return ""
		}
		return logs
	}

	poolState := func() string {
		last := ""
		for _, line := range strings.Split(controllerLog(), "\n") {
			if strings.Contains(line, `"pool": "`+attrPool+`"`) && strings.Contains(line, "warm pool state") {
				last = line
			}
		}
		return last
	}

	// linesContaining returns every controller log line holding all of subs.
	linesContaining := func(subs ...string) []string {
		var out []string
		for _, line := range strings.Split(controllerLog(), "\n") {
			all := true
			for _, s := range subs {
				if !strings.Contains(line, s) {
					all = false
					break
				}
			}
			if all {
				out = append(out, line)
			}
		}
		return out
	}

	BeforeAll(func() {
		if !cfg.UseSimulator {
			Skip("--fake-metrics is simulator-only, and this spec needs demand that does not depend on timing")
		}
		ctx = context.Background()
		controller = fixtures.ControllerDeployment{
			Namespace: cfg.WVANamespace,
			Name:      "wva-controller-manager",
		}

		nodes, err := fixtures.SchedulableNodes(ctx, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		Expect(nodes).NotTo(BeEmpty())
		modelNode = nodes[0].Name

		By("Restarting the controller with the warm pool enabled")
		restoreCtl, err := fixtures.EnableWarmPool(ctx, k8sClient, controller, cfg.LLMDNamespace)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = restoreCtl(context.Background())
			_ = fixtures.WaitForControllerReady(context.Background(), k8sClient, controller)
		})

		By("Standing up the pool")
		poolSpec = fixtures.WarmPoolSpec{
			Name:       attrPool,
			Namespace:  cfg.LLMDNamespace,
			ProxyImage: cfg.WarmPoolProxyImage,
			PoolName:   attrPool,
		}
		Expect(fixtures.CreateWarmPool(ctx, k8sClient, poolSpec)).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteWarmPool(context.Background(), k8sClient, poolSpec) })

		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace,
			attrPool, attrPool, attrPool, 1, 4, cfg.MonitoringNS,
			fixtures.WithWarmPoolTrigger(attrPool, map[string]string{
				"warmPoolSleepMinSize": "0",
				"warmPoolMaxHold":      "15m",
			}),
		)).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteScaledObject(context.Background(), crClient, cfg.LLMDNamespace, attrPool)
		})

		By("SCRAPING the pool, without which none of this is observable")
		// The assertions below are all about how a bridge's metrics are
		// attributed. A pool nothing scrapes produces no metrics, the collector
		// produces no row, and every one of those assertions passes while
		// proving nothing -- which is an empty result, not a green one.
		// BOTH halves, and the second is the one that is easy to miss. The pool's
		// shipped NetworkPolicy admits the controller and the model's namespace
		// and nothing else, deliberately -- so a PodMonitor on its own produces a
		// target that is refused at the network, which looks exactly like a Pod
		// that serves no metrics. Discovered by this spec failing with the engine
		// answering /metrics perfectly well to a tenant.
		Expect(fixtures.EnsureWarmPoolPodMonitor(ctx, crClient, cfg.LLMDNamespace, attrPool)).To(Succeed())
		Expect(fixtures.EnsureWarmPoolScrapeIngress(ctx, k8sClient, cfg.LLMDNamespace, cfg.MonitoringNS)).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteWarmPoolPodMonitor(context.Background(), crClient, cfg.LLMDNamespace, attrPool)
			_ = fixtures.DeleteWarmPoolScrapeIngress(context.Background(), k8sClient, cfg.LLMDNamespace)
		})

		By("Waiting for the pool Pod")
		Eventually(func(g Gomega) {
			pods, err := k8sClient.CoreV1().Pods(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: fixtures.WarmPoolNameLabel + "=" + attrPool,
			})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods.Items).To(HaveLen(1))
			g.Expect(pods.Items[0].Status.PodIP).NotTo(BeEmpty())
			poolPod = pods.Items[0].Name
			poolPodIP = pods.Items[0].Status.PodIP
		}, settle, 5*time.Second).Should(Succeed())

		Expect(fixtures.CreateHTTPDriver(ctx, k8sClient, fixtures.DriverSpec{
			Name: tenantDriver, Namespace: cfg.LLMDNamespace, Labels: fixtures.TenantDriverLabels(),
		})).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteHTTPDriver(context.Background(), k8sClient,
				fixtures.DriverSpec{Name: tenantDriver, Namespace: cfg.LLMDNamespace})
		})

		By("Creating a model whose metrics say it is over its threshold")
		Expect(fixtures.CreateModelServiceWithExtraArgs(
			ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, fixturePool, cfg.ModelID,
			cfg.UseSimulator, cfg.MaxNumSeqs,
			[]string{"--fake-metrics", attrFakeMetrics})).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteModelService(context.Background(), k8sClient, cfg.LLMDNamespace, modelSvcName)
		})

		By("Making the model's own metrics reachable too")
		Expect(fixtures.EnsureService(
			ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, scaleTarget, 8000,
		)).To(Succeed())
		Expect(fixtures.EnsureServiceMonitor(
			ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelSvcName, scaleTarget,
		)).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteServiceMonitor(context.Background(), crClient, cfg.MonitoringNS, modelSvcName)
			_ = fixtures.DeleteService(context.Background(), k8sClient, cfg.LLMDNamespace, modelSvcName)
		})

		By("Pinning the model so its ordinary replicas cannot ARRIVE")
		// Holds the shortfall open, so the bridge stays lent for the whole spec
		// rather than being handed back between two assertions. It also creates
		// the exact condition the serving-count assertion is about: one ordinary
		// replica Ready, one bridge serving, and a pool that must not confuse
		// the two.
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, scaleTarget, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			app := dep.Spec.Template.Labels["app"]
			g.Expect(app).NotTo(BeEmpty())
			dep.Spec.Template.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": modelNode}
			dep.Spec.Template.Spec.Affinity = &corev1.Affinity{
				PodAntiAffinity: &corev1.PodAntiAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
						TopologyKey:   "kubernetes.io/hostname",
						LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": app}},
					}},
				},
			}
			_, err = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Update(ctx, dep, metav1.UpdateOptions{})
			g.Expect(err).NotTo(HaveOccurred())
		}, time.Minute, 5*time.Second).Should(Succeed())

		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, scaleTarget, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 1))
		}, settle, 5*time.Second).Should(Succeed())

		By("Registering the variant under a name that is NOT its scale target")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace,
			scalerName, scaleTarget, modelSvcName+"-variant", 1, 4, cfg.MonitoringNS,
			fixtures.WithWVATriggerMetadata(cfg.ModelID, "10.0"),
			fixtures.WithWarmPoolSelection(attrPool, 1),
		)).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteScaledObject(context.Background(), crClient, cfg.LLMDNamespace, scalerName)
		})

		By("Setting thresholds the fake metrics exceed")
		cmName := scalingPolicyConfigMapName()
		original, err := k8sClient.CoreV1().ConfigMaps(cfg.WVANamespace).Get(ctx, cmName, metav1.GetOptions{})
		existedBefore := err == nil
		if existedBefore {
			original = original.DeepCopy()
		}
		Expect(upsertSaturationConfigEntry(ctx, cfg.WVANamespace, cmName, defaultConfigKey,
			buildSaturationConfigYAMLWithThresholds("saturation",
				kvCacheThreshold, queueThreshold, scaleUpThreshold, scaleDownBoundary),
		)).To(Succeed())
		DeferCleanup(func() {
			restoreSaturationConfigMap(context.Background(), cfg.WVANamespace, cmName, original, existedBefore)
		})
	})

	It("attributes the bridge to the variant, and does not count it as one of its replicas", func() {
		By("Waiting for the pool to lend")
		Eventually(poolState, settle, 5*time.Second).ShouldNot(BeEmpty(),
			"the controller never reported this pool, so its ScaledObject was never called about")
		Eventually(poolState, settle, 5*time.Second).Should(MatchRegexp(`resident=[1-9]`),
			"the pool never warmed the model, so there was never anything to lend")
		Eventually(poolState, settle, 5*time.Second).Should(MatchRegexp(`lent=[1-9]`),
			"WVA never decided to borrow, so nothing below is being tested")

		By("Checking the bridge SERVES metrics at all, before blaming the scrape")
		// Splits the one failure that matters into two. "The bridge was never
		// attributed" has two very different causes -- the Pod is not producing
		// metrics, or it is and nothing collected them -- and they need opposite
		// fixes. Asking the Pod directly, from inside the cluster, settles which.
		//
		// Through the PROXY's serving port, because that is the address the
		// PodMonitor scrapes: a Pod whose engine serves metrics on some other
		// port is exactly as unscrapeable as one serving none.
		Eventually(func(g Gomega) {
			got, err := fixtures.DriverCall(ctx, k8sClient, cfg.LLMDNamespace, tenantDriver,
				"GET", fmt.Sprintf("http://%s:%d/metrics", poolPodIP, fixtures.WarmPoolServingPort), "")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got.Status).To(Equal(200),
				"the lent Pod does not serve /metrics through its proxy: %s", got.Body)
			g.Expect(got.Body).To(ContainSubstring("vllm:"),
				"the lent Pod answered /metrics with no vLLM series, so there is nothing to attribute: %s",
				got.Body)
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("Confirming Prometheus actually COLLECTED the bridge's series")
		// The third distinct failure hiding behind one symptom. The Pod serves
		// metrics (proved above) and something still has to carry them to the
		// collector; if the PodMonitor target never comes up, the collector is
		// working perfectly on data it was never given. Asking Prometheus
		// directly separates "not scraped" from "scraped and not attributed".
		if pc := promClientForCheck(); pc != nil {
			Eventually(func(g Gomega) {
				n, err := pc.QueryWithRetry(ctx,
					fmt.Sprintf(`count(vllm:kv_cache_usage_perc{pod=%q})`, poolPod))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(n).To(BeNumerically(">", 0),
					"Prometheus holds no vLLM series for the lent Pod: its PodMonitor target "+
						"is not up. Check the pool's NetworkPolicy admits the monitoring "+
						"namespace on the serving port, and that the port name matches")
			}, 3*time.Minute, 10*time.Second).Should(Succeed())
		}

		By("Waiting for the bridge to be SCRAPED and attributed")
		// The gate that stops every assertion below from passing vacuously. Until
		// the collector has produced a row for the pool Pod there is nothing to
		// be attributed rightly or wrongly.
		Eventually(func() []string {
			return linesContaining("attributing it to the variant it is lent to", poolPod)
		}, settle, 10*time.Second).ShouldNot(BeEmpty(),
			"the bridge was never attributed: check that the pool PodMonitor has a live target "+
				"and that the emulated engine serves /metrics")

		By("Attributing it under the VARIANT's name")
		attributions := linesContaining("attributing it to the variant it is lent to", poolPod)
		for _, line := range attributions {
			Expect(line).To(ContainSubstring(`"lentTo": "`+scalerName+`"`),
				"a bridge must be attributed to the ScaledObject the collector resolves a variant by")
			Expect(line).NotTo(ContainSubstring(`"lentTo": "`+scaleTarget+`"`),
				"the scale target is the Deployment underneath the ScaledObject; "+
					"a lending published under it matches no analyzer row")
		}

		By("Producing NO phantom variant named after the scale target")
		// The visible shape of the bug: the analyzer grew a second variant, made
		// entirely of the bridge, that nothing else in WVA had ever heard of.
		Expect(linesContaining("replica-capacity-decision", `"variant": "`+scaleTarget+`"`)).To(BeEmpty(),
			"the analyzer keyed a row by the scale target, which is a variant nothing is scaling")

		By("Counting the bridge in the variant's own capacity rows")
		// The positive half: the row for the pool Pod exists AND names the
		// variant. Without this the assertion above is satisfied by a bridge that
		// is simply dropped, which is the behaviour before any of this worked.
		Expect(linesContaining("replica-capacity-decision", `"variant": "`+scalerName+`"`, poolPod)).
			NotTo(BeEmpty(), "the bridge's capacity was never attributed to the variant it serves")

		By("Never counting the bridge among the variant's own SERVING replicas")
		// Demand yes, supply no. The pool reads this to decide whether the
		// ordinary replicas have arrived; counting the bridge answers yes one
		// replica early and hands back the Pod that was carrying the traffic.
		//
		// Asserted as a relation rather than against a fixed number, because the
		// ordinary replica count is the scale target's and this spec deliberately
		// stops a second one from arriving rather than pinning what Ready is.
		servingLine := regexp.MustCompile(`"ready": (\d+), "serving": (\d+)`)
		for _, line := range linesContaining("counting SERVING replicas", `"variant": "`+scalerName+`"`) {
			m := servingLine.FindStringSubmatch(line)
			Expect(m).To(HaveLen(3), "unparseable serving line: %s", line)
			ready, err := strconv.Atoi(m[1])
			Expect(err).NotTo(HaveOccurred())
			serving, err := strconv.Atoi(m[2])
			Expect(err).NotTo(HaveOccurred())
			Expect(serving).To(BeNumerically("<=", ready),
				fmt.Sprintf("serving exceeded the ordinary replica count, so a bridge was counted "+
					"as one of them: %s", line))
		}
	})
})
