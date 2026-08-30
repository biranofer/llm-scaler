package fixtures

import (
	"context"
	"fmt"

	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// warmPoolMonitorName is the PodMonitor these helpers own, per pool.
func warmPoolMonitorName(poolName string) string {
	return "warm-pool-metrics-" + poolName
}

// EnsureWarmPoolPodMonitor scrapes a pool's Pods, so a BRIDGE is measured like
// any other replica.
//
// Without this the pool Pod is invisible to Prometheus: the collector never
// produces a row for it, and every assertion about how a bridge is attributed
// passes while proving nothing -- which is the shape of an empty result, not a
// green one.
//
// Scraped through the PROXY's serving port rather than an engine port, and that
// is the only address that works. A pool Pod holds several engines on ports
// assigned at admission, so their numbers are not known when this is created;
// the proxy sits in front of whichever one is awake and forwards /metrics to it.
// It is also the port the real deployment scrapes, so this exercises the shipped
// shape rather than a test-only one.
func EnsureWarmPoolPodMonitor(ctx context.Context, crClient client.Client, namespace, poolName string) error {
	pm := &promoperator.PodMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      warmPoolMonitorName(poolName),
			Namespace: namespace,
			Labels:    map[string]string{"test-resource": defaultTestResourceLabelValue},
		},
		Spec: promoperator.PodMonitorSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{WarmPoolNameLabel: poolName},
			},
			PodMetricsEndpoints: []promoperator.PodMetricsEndpoint{{
				Path: "/metrics",
				// Faster than the 30s a model gets. A bridge exists for as long
				// as a scale-up takes to land, and a scrape interval longer than
				// the borrow means the spec can watch the whole lifecycle go by
				// without a single sample of it.
				Interval: "10s",
				Port:     ptrString("serving"),
			}},
		},
	}
	if err := crClient.Create(ctx, pm); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create warm pool PodMonitor: %w", err)
	}
	return nil
}

// DeleteWarmPoolPodMonitor removes it; a missing object is not an error.
//
// Deleted rather than left behind: the next run would inherit a monitor pointed
// at a pool that no longer exists, and a target that never comes up is quiet in
// exactly the way that makes a later spec's metrics hard to trust.
func DeleteWarmPoolPodMonitor(ctx context.Context, crClient client.Client, namespace, poolName string) error {
	pm := &promoperator.PodMonitor{ObjectMeta: metav1.ObjectMeta{
		Name:      warmPoolMonitorName(poolName),
		Namespace: namespace,
	}}
	if err := crClient.Delete(ctx, pm); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

func ptrString(s string) *string { return &s }
