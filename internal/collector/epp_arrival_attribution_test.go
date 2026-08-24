package collector

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// TestEPPArrivalRateAttribution pins the join between the EPP's dispatch rate
// and the engine's own series.
//
// The two arrive keyed differently and cannot be reconciled on port. Engine
// series are keyed by buildInstanceKey from Prometheus's `instance` label, whose
// port is the one the engine is SCRAPED on; the EPP reports the port it ROUTES
// to. Under llm-d modelservice a routing sidecar fronts the pod on 8000 while
// vLLM serves and exposes /metrics on 8200, so the ports differ by construction
// and pod name is the only shared identity.
//
// Regression: when the collector built "<pod>:<epp port>" as an instance key, no
// pod ever received an arrival rate — the EPP half carried no KV or queue and was
// dropped as a stale key, and every serving pod logged "no dispatch rate was
// collected". λ was structurally zero on every real deployment.
func TestEPPArrivalRateAttribution(t *testing.T) {
	const (
		podName    = "decode-6b879574d5-4f4xh"
		modelID    = "test-model"
		scrapePort = "8200" // vLLM's metrics port, via the `instance` label
		routedPort = "8000" // the InferencePool target, via the EPP's `port` label
	)

	tests := []struct {
		name     string
		dispatch []source.MetricValue
		want     float64
	}{
		{
			name: "ports differ – arrival rate still lands on the pod",
			dispatch: []source.MetricValue{{
				Labels: map[string]string{
					seriesTargetModelLabel: modelID,
					"pod_name":             podName,
					"port":                 routedPort,
				},
				Value: 1.5,
			}},
			want: 1.5,
		},
		{
			name: "several routed ports for one pod – summed, not overwritten",
			dispatch: []source.MetricValue{
				{Labels: map[string]string{seriesTargetModelLabel: modelID, "pod_name": podName, "port": "8000"}, Value: 1.5},
				{Labels: map[string]string{seriesTargetModelLabel: modelID, "pod_name": podName, "port": "8001"}, Value: 0.5},
			},
			want: 2.0,
		},
		{
			name: "dispatch for a pod with no engine series – dropped, not a phantom replica",
			dispatch: []source.MetricValue{{
				Labels: map[string]string{
					seriesTargetModelLabel: modelID,
					"pod_name":             "some-other-pod",
					"port":                 routedPort,
				},
				Value: 9.0,
			}},
			want: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			if err := metrics.InitMetrics(registry); err != nil {
				t.Fatalf("InitMetrics: %v", err)
			}

			scheme := runtime.NewScheme()
			if err := llmdVariantAutoscalingV1alpha1.AddToScheme(scheme); err != nil {
				t.Fatalf("AddToScheme: %v", err)
			}
			k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

			now := time.Now()
			for i := range tc.dispatch {
				tc.dispatch[i].Timestamp = now
			}

			mockSource := &mockMetricsSource{
				refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
					return map[string]*source.MetricResult{
						"kv_cache_usage": {
							Values: []source.MetricValue{{
								Labels: map[string]string{
									seriesModelLabel: modelID,
									"pod":            podName,
									"instance":       "10.0.0.1:" + scrapePort,
								},
								Value:     0.5,
								Timestamp: now,
							}},
						},
						registration.QuerySchedulerDispatchRate: {Values: tc.dispatch},
					}, nil
				},
			}

			collector := NewReplicaMetricsCollector(mockSource, k8sClient, nil, allPodsLocator("my-va"))
			results, err := collector.CollectReplicaMetrics(
				context.Background(),
				modelID,
				"test-ns",
				make(map[string]scaletarget.ScaleTargetAccessor),
				make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling),
				nil,
			)
			if err != nil {
				t.Fatalf("CollectReplicaMetrics: %v", err)
			}

			// One entry only: the engine's. The EPP series must not create a
			// second, KV-less replica.
			if len(results) != 1 {
				t.Fatalf("expected exactly 1 ReplicaMetrics (the engine pod), got %d", len(results))
			}
			if got := results[0].PodName; got != podName {
				t.Errorf("PodName: got %q, want %q", got, podName)
			}
			if got := results[0].ArrivalRate; got != tc.want {
				t.Errorf("ArrivalRate: got %v, want %v", got, tc.want)
			}
		})
	}
}
