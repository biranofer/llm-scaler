package e2e

import (
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// Parking a SERVING model, which nothing else covered.
//
// Every scale-from-zero spec CREATES its workload at zero and wakes it — read
// their names: "should verify deployment starts at zero replicas". None drives a
// model that is serving down to zero, so the whole scale-TO-zero path had unit
// tests and no end-to-end coverage at all. That is how WVA_SCALE_TO_ZERO came to
// be inert on every shipped install without a single test noticing: the flag is
// only consulted when a model is about to park, and no test ever asked one to.
//
// Two preconditions here are not obvious, and both would make this fail for the
// wrong reason if missed:
//
//  1. THE MODEL MUST SERVE TRAFFIC FIRST. The idle check is
//     sum(increase(vllm:request_success_total{...}[retention])), and over a model
//     Prometheus has never seen that returns no series — reported as an error, not
//     as zero, so the enforcer deliberately keeps its decisions. A model that has
//     never served cannot be parked, by design (see the cold-start guarantee in
//     internal/collector/registration/scale_to_zero_test.go). So load runs, then
//     stops, and only then can the counter read a real zero.
//
//  2. RETENTION MUST BE SHORT. The default is 10m, which is longer than this
//     suite should take, so a per-model override sets it to 1m. Identity lives in
//     the entry body, not the ConfigMap key.
//
// Unlike scale-from-zero, this needs no EPP flow control: parking is decided from
// the request counter alone, so the suite does not depend on the wake path.
var _ = Describe("Scale-To-Zero Feature (parking a serving model)", Serial, Label("full"), Ordered, func() {
	const (
		modelSvcName  = "scale-to-zero-ms"
		decodeDeploy  = "scale-to-zero-ms-decode"
		poolName      = "scale-to-zero-pool"
		scalerBase    = "scale-to-zero"
		variantName   = "scale-to-zero-so"
		retention     = "1m"
		loadRequests  = 40
		parkingBudget = 5 * time.Minute
	)

	var (
		modelID         string
		cmName          string
		cmNamespace     string
		cmOriginal      *corev1.ConfigMap
		cmExistedBefore bool
	)

	BeforeAll(func() {
		if !cfg.ScaleToZeroEnabled {
			Skip("This suite parks a model, which requires scale-to-zero: set SCALE_TO_ZERO_ENABLED=true")
		}

		modelID = sfzModelID("stz")
		cmName = scalingPolicyConfigMapName()
		cmNamespace = cfg.WVANamespace

		By("Snapshotting the scaling policy ConfigMap for restore in AfterAll")
		cm, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if err == nil {
			cmExistedBefore = true
			cmOriginal = cm.DeepCopy()
		} else if !errors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred(), "failed reading the scaling policy ConfigMap")
		}

		By("Installing a default entry, and a per-model override enabling scale-to-zero with 1m retention")
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, defaultConfigKey,
			buildSaturationConfigYAML("saturation"))).To(Succeed())

		// The override is what makes this model parkable on a test's timescale. It
		// also makes the suite independent of how the deployment flag happens to be
		// set, which matters because the entry always wins over the flag.
		modelEntry := buildSaturationConfigYAMLWithModel(
			"saturation", 0.80, 1, 0.85, 0.70, modelID, cfg.LLMDNamespace,
		) + fmt.Sprintf("scaleToZero:\n  enabled: true\n  retentionPeriod: %s\n", retention)
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, "scale-to-zero-model", modelEntry)).To(Succeed())

		By("Creating the model service, SERVING (not parked)")
		_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
		Expect(fixtures.EnsureModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName,
			poolName, modelID, cfg.UseSimulator, cfg.MaxNumSeqs)).To(Succeed())
		Expect(fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, decodeDeploy, 8000)).To(Succeed())

		// Without the ServiceMonitor the request counter never reaches Prometheus,
		// the idle query stays absent, and the model can never park — the test would
		// fail on a missing precondition rather than on WVA.
		Expect(fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS,
			cfg.LLMDNamespace, modelSvcName, decodeDeploy)).To(Succeed())

		By("Waiting for the model to be ready to serve")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, decodeDeploy, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 1), "model must serve before it can be parked")
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Registering the workload with minReplicaCount=0 so it is ALLOWED to reach zero")
		// A fresh ScaledObject, so KEDA builds its HPA with minReplicas: 0. An HPA
		// created while minReplicaCount was 1 keeps minReplicas: 1 for life, and the
		// workload then cannot reach zero however the ScaledObject reads afterwards.
		_ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBase)
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBase,
			decodeDeploy, variantName, 0, 10, cfg.MonitoringNS,
			fixtures.WithWVATriggerMetadata(modelID, "30.0"),
			fixtures.WithExternalScalerPushTrigger(externalScalerAddress()))).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBase)
		})
	})

	AfterAll(func() {
		By("Restoring the scaling policy ConfigMap")
		if cmExistedBefore && cmOriginal != nil {
			restored := cmOriginal.DeepCopy()
			restored.ResourceVersion = ""
			_ = k8sClient.CoreV1().ConfigMaps(cmNamespace).Delete(ctx, cmName, metav1.DeleteOptions{})
			if _, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Create(ctx, restored, metav1.CreateOptions{}); err != nil {
				GinkgoWriter.Printf("Warning: could not restore ConfigMap %s: %v\n", cmName, err)
			}
		}
		_ = fixtures.DeleteBurstLoadJob(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
		_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
	})

	It("serves traffic, so the idle signal exists at all", func() {
		By("Driving requests through the model service")
		target := fmt.Sprintf("http://%s.%s.svc.cluster.local:8000", modelSvcName, cfg.LLMDNamespace)
		Expect(fixtures.EnsureBurstLoadJob(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, target,
			fixtures.LoadConfig{
				Strategy:     "synthetic",
				NumPrompts:   loadRequests,
				InputTokens:  32,
				OutputTokens: 32,
				ModelID:      modelID,
			})).To(Succeed())

		// The counter existing is the precondition for everything below, so assert it
		// rather than assuming the load landed. Absent here means the rest of the
		// suite would be testing the cold-start guard, not parking.
		pc := promClientForCheck()
		if pc == nil {
			Skip("no Prometheus reachable from the test host; the idle signal cannot be confirmed")
		}
		query := fmt.Sprintf(`count(vllm:request_success_total{namespace=%q,model_name=%q})`,
			cfg.LLMDNamespace, modelID)
		Eventually(func(g Gomega) {
			v, err := pc.QueryWithRetry(ctx, query)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(v).To(BeNumerically(">", 0),
				"vllm:request_success_total must exist for this model, or nothing can park")
		}, 3*time.Minute, 10*time.Second).Should(Succeed())
	})

	It("parks the model once it goes idle", func() {
		By("Stopping the load")
		Expect(fixtures.DeleteBurstLoadJob(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)).To(Succeed())

		// After the last request, increase() over the retention window takes that
		// window to fall to zero; then the enforcer parks on its next cycle and KEDA
		// acts. This is THE assertion the suite exists for: before the flag fix, a
		// model in exactly this state stayed at one replica forever.
		By("Waiting for the deployment to reach zero replicas")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, decodeDeploy, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(*dep.Spec.Replicas).To(Equal(int32(0)),
				"an idle model whose policy enables scale-to-zero and whose variants all permit "+
					"zero must be parked; if it is not, resolve scaleToZero for this model and "+
					"check wva_model_scaling_blocked for the reason")
		}, parkingBudget, 10*time.Second).Should(Succeed())
	})

	It("reports the parked model as serving nothing", func() {
		pc := promClientForCheck()
		if pc == nil {
			Skip("no Prometheus reachable from the test host")
		}

		// wva_model_replicas is what the symptom alert reads, and 0 is the value two
		// earlier implementations made unreachable. exported_namespace, not namespace:
		// the scrape job attaches its own namespace (the controller's) and Prometheus
		// renames the scraped one.
		query := fmt.Sprintf(`sum(wva_model_replicas{exported_namespace=%q,model_name=%q})`,
			cfg.LLMDNamespace, modelID)
		Eventually(func(g Gomega) {
			v, err := pc.QueryWithRetry(ctx, query)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(v).To(BeNumerically("==", 0), "a parked model must report zero replicas")
		}, 2*time.Minute, 10*time.Second).Should(Succeed())
	})

	It("does not report a configuration contradiction for this model", func() {
		pc := promClientForCheck()
		if pc == nil {
			Skip("no Prometheus reachable from the test host")
		}

		// Both halves agree here — the policy enables zero and every variant permits
		// it — so wva_model_scaling_blocked must be silent. A variant-floor or
		// policy-forbids-zero series would mean the fixture contradicts itself and
		// the parking above happened for some other reason.
		query := fmt.Sprintf(
			`count(wva_model_scaling_blocked{exported_namespace=%q,model_name=%q,`+
				`reason=~"variant-floor|policy-forbids-zero"}) or vector(0)`,
			cfg.LLMDNamespace, modelID)
		v, err := pc.QueryWithRetry(ctx, query)
		Expect(err).NotTo(HaveOccurred())
		Expect(v).To(BeNumerically("==", 0),
			"the fixture enables scale-to-zero AND permits zero on every variant, so neither "+
				"contradiction reason should be reported")
	})
})
