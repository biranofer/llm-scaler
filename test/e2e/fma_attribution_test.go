package e2e

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
	testutils "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

// Fast Model Actuation attribution.
//
// FMA splits a model server across two pods: a REQUESTER Deployment carrying the
// llm-d identity, which a scaler moves, and LAUNCHER pods owned by a
// LauncherConfig, which hold the GPU and run the engine. The ownerReferences gap
// is deliberate — FMA patches the provider's labels so no ReplicaSet adopts it —
// so the collector's owner walk ends at nothing and falls back to the pairing
// FMA declares on the pods themselves.
//
// Before that fallback existed, an FMA variant reported demand 0 under load: the
// only pods reporting engine metrics were attributed to no scale target, and the
// requester that WAS attributable runs no engine. A benchmark showed a variant
// flat at one replica through a 155-deep queue and read as healthy.
//
// The layout here is built without FMA installed, because the labels are the
// whole contract: a launcher pod owned by a kind the walk cannot follow, paired
// by dual-pods.llm-d.ai/dual to a requester pod that a Deployment owns. No CRD
// and no controller are required — walkOwnersUp stops at an unknown owner kind
// before it tries to fetch it, which is exactly the production path.
var _ = Describe("FMA launcher attribution", Label("full"), Label("fma"), Ordered, func() {
	const (
		layoutName = "e2e-fma"
		modelID    = "default/default"
		modelLabel = "e2e-fma-model"
		maxNumSeqs = 4
		// Same selector the other specs use to read the controller's logs.
		fmaControllerManagerLabel = "control-plane=controller-manager"
	)

	var (
		ctx    context.Context
		layout *fixtures.FMALayout
	)

	BeforeAll(func() {
		ctx = context.Background()

		DeferCleanup(func() {
			_ = fixtures.DeleteFMALauncherPodMonitor(ctx, crClient, cfg.LLMDNamespace)
			_ = fixtures.DeleteFMALayout(ctx, k8sClient, cfg.LLMDNamespace, layout)
		})

		By("creating an FMA layout: a requester Deployment, a bound launcher, and one warm spare")
		var err error
		layout, err = fixtures.CreateFMALayout(ctx, k8sClient, cfg.LLMDNamespace,
			layoutName, modelLabel, modelID, maxNumSeqs)
		Expect(err).NotTo(HaveOccurred())

		By("scraping the launchers the way the shipped PodMonitor does")
		Expect(fixtures.EnsureFMALauncherPodMonitor(ctx, crClient, cfg.LLMDNamespace)).To(Succeed())
	})

	It("attributes a bound launcher to the requester's scale target", func() {
		// The controller says so once per cycle when the hop carries anything.
		// Asserting on the log rather than on a metric keeps this independent of
		// how long Prometheus takes to make a new series visible, and the line
		// names the pair so a failure says which one.
		Eventually(func() bool {
			ok, _, err := testutils.PodLogsLabelSelectorContain(ctx, k8sClient, cfg.WVANamespace,
				fmaControllerManagerLabel, "Attributed FMA launcher pods through their dual-pods pairing", 300)
			return err == nil && ok
		}, 5*time.Minute, 15*time.Second).Should(BeTrue(),
			"the collector never reported attributing a launcher through its pairing; "+
				"check that the launcher pod is a scrape target and that its dual-pods.llm-d.ai/dual "+
				"label names a pod whose ownerReferences reach a ScaledObject")
	})

	It("does not attribute a launcher with no bound instance", func() {
		// The warm spare carries no pairing label, so it must be rejected — and
		// rejected under its OWN reason, not the generic one. A pool of spares is
		// expected and permanent; filing it as `unresolved` would bury a real
		// attribution failure in the same counter.
		Eventually(func() bool {
			ok, _, err := testutils.PodLogsLabelSelectorContain(ctx, k8sClient, cfg.WVANamespace,
				fmaControllerManagerLabel,
				fmt.Sprintf("%s\"", layout.UnboundLauncherPod), 300)
			if err != nil || !ok {
				return false
			}
			ok, _, err = testutils.PodLogsLabelSelectorContain(ctx, k8sClient, cfg.WVANamespace,
				fmaControllerManagerLabel, "unbound_launcher", 300)
			return err == nil && ok
		}, 5*time.Minute, 15*time.Second).Should(BeTrue(),
			"the unbound launcher was not skipped as a warm spare; it must be counted under "+
				"reason=unbound_launcher rather than attributed or filed as unresolved")
	})

	It("never files a bound launcher as unresolved", func() {
		// The negative that matters. `unresolved` means the walk reached nothing
		// AND no pairing rescued it — for a bound launcher that is a regression in
		// the hop, and it is silent: demand simply reads low.
		Consistently(func() bool {
			ok, _, err := testutils.PodLogsLabelSelectorContain(ctx, k8sClient, cfg.WVANamespace,
				fmaControllerManagerLabel,
				fmt.Sprintf("%s\", \"instance", layout.LauncherPod), 300)
			return err == nil && ok
		}, 60*time.Second, 20*time.Second).Should(BeFalse(),
			"the bound launcher appeared in a skip message; the pairing hop is not resolving it")
	})
})
