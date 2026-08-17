package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

// Scale-from-zero has exactly one input: the EPP flow-control queue. If EPP is
// not queueing, the engine cannot wake anything and the suite is testing an
// absent precondition rather than WVA.
//
// The gate used to be `if !cfg.ScaleToZeroEnabled { Skip }` -- it asserted that a
// FLAG was set, not that the capability existed. Those are different claims, and
// the gap between them hid a real problem for weeks: with the flag off every
// spec Skip()ped, and a runtime Skip reports as SUCCESS, so the suite was green
// while more than half of it never ran. Turning the flag on then produced
// failures that read as WVA bugs but were nothing of the kind -- measured on
// kind, across 152 samples of a full spec run, llm_d_epp_flow_control_queue_size
// never registered at all while ServiceUnavailable climbed by 40. Flow control
// was not engaging; WVA never had a signal to act on.
//
// So check the capability, and when it is missing say so precisely. A skip that
// names what is absent is worth more than a failure that points at the wrong
// component.

const (
	flowControlQueueMetric = "llm_d_epp_flow_control_queue_size"
	// The GIE name for the same series, still exported as a deprecated alias.
	flowControlQueueMetricAlias = "inference_extension_flow_control_queue_size"
	// The token WVA itself mounts to scrape EPP. EPP /metrics is authenticated:
	// an unauthenticated GET returns a one-line "Unauthorized" body, which is
	// easy to mistake for "the metric is absent".
	eppMetricsTokenSecret = "wva-epp-metrics-token"
)

// eppFlowControlAvailable reports whether the flow-control queue metric exists,
// and why not when it does not.
//
// Prometheus is asked first, and is the authority. It scrapes EPP from inside
// the cluster, which the test host cannot do: pod IPs are not routable from a
// kind host, so a direct scrape reports "could not scrape" and hides the real
// answer behind a plumbing failure. Asking Prometheus checks the capability from
// somewhere that can actually observe it.
//
// The direct scrape is kept only as a fallback for environments with no
// Prometheus reachable from the test host.
func eppFlowControlAvailable(ctx context.Context, c client.Client, wvaNamespace, poolNamespace string) (bool, string) {
	if pc := promClientForCheck(); pc != nil {
		var reachable bool
		for _, name := range []string{flowControlQueueMetric, flowControlQueueMetricAlias} {
			// absent() yields 1 when the series does not exist and nothing when it
			// does. That is the distinction the whole check turns on -- a
			// value-based query cannot tell "no such metric" from "metric present,
			// value 0", and conflating those is exactly how this stayed hidden.
			v, err := pc.QueryWithRetry(ctx, fmt.Sprintf("absent(%s)", name))
			if err != nil {
				continue
			}
			reachable = true
			if v == 0 {
				return true, ""
			}
		}
		if reachable {
			return false, fmt.Sprintf(
				"Prometheus has no %s series: EPP is not queueing, so no wake signal "+
					"can exist -- an EPP/environment gap, not a WVA failure",
				flowControlQueueMetric)
		}
	}
	return eppFlowControlAvailableByScrape(ctx, c, wvaNamespace, poolNamespace)
}

// eppFlowControlAvailableByScrape is the fallback: read the metric off an EPP pod
// directly. Only usable where the test host can reach pod IPs.
func eppFlowControlAvailableByScrape(ctx context.Context, c client.Client, wvaNamespace, poolNamespace string) (bool, string) {
	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{
		Name: eppMetricsTokenSecret, Namespace: wvaNamespace,
	}, &secret); err != nil {
		return false, fmt.Sprintf("cannot read %s/%s: %v", wvaNamespace, eppMetricsTokenSecret, err)
	}
	token := strings.TrimSpace(string(secret.Data["token"]))
	if token == "" {
		return false, fmt.Sprintf("%s carries no token", eppMetricsTokenSecret)
	}

	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(poolNamespace)); err != nil {
		return false, fmt.Sprintf("listing pods in %s: %v", poolNamespace, err)
	}

	var tried int
	for i := range pods.Items {
		p := &pods.Items[i]
		if !strings.Contains(p.Name, "epp") || p.Status.PodIP == "" {
			continue
		}
		tried++
		body, err := scrapeEPPMetrics(ctx, p.Status.PodIP, token)
		if err != nil {
			continue
		}
		if strings.Contains(body, flowControlQueueMetric) ||
			strings.Contains(body, flowControlQueueMetricAlias) {
			return true, ""
		}
		// Reached and readable, but the family is not there. Report what the
		// endpoint DID contain, so the reader can tell an unauthenticated
		// response apart from a genuinely absent metric.
		return false, fmt.Sprintf(
			"EPP %s exports no flow-control queue metric (%d bytes scraped). "+
				"Flow control is not engaging, so no wake signal can exist -- "+
				"this is an EPP/environment gap, not a WVA failure",
			p.Name, len(body))
	}
	if tried == 0 {
		return false, fmt.Sprintf("no EPP pod with an IP found in %s", poolNamespace)
	}
	return false, fmt.Sprintf("could not scrape any of %d EPP pod(s) in %s", tried, poolNamespace)
}

// promClientForCheck builds a Prometheus client the same way the other e2e
// helpers do. Returns nil when none can be built, so the caller falls back to a
// direct scrape rather than failing the suite over a missing convenience.
func promClientForCheck() *utils.PrometheusClient {
	url := os.Getenv("PROMETHEUS_URL")
	if url == "" {
		url = utils.DefaultPrometheusURL
	}
	insecure := true
	if v := os.Getenv("PROMETHEUS_SKIP_TLS_VERIFY"); v != "" {
		insecure = strings.EqualFold(v, "true")
	}
	pc, err := utils.NewPrometheusClient(url, insecure)
	if err != nil {
		return nil
	}
	return pc
}

func scrapeEPPMetrics(ctx context.Context, podIP, token string) (string, error) {
	url := fmt.Sprintf("http://%s:9090/metrics", podIP)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}
