// This file provides queueing model analyzer metrics collection using the source
// infrastructure with registered query templates.
package registration

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
)

// Query name constants for queueing model analyzer metrics.
const (
	// QuerySchedulerDispatchRate is the query name for per-endpoint request dispatch rate from scheduler.
	// This represents the arrival rate (requests/sec) being dispatched to each replica by the scheduler.
	// Source: inference_extension_scheduler_attempts_total (gateway-api-inference-extension)
	QuerySchedulerDispatchRate = "scheduler_dispatch_rate"

	// QueryAvgITL is the query name for average inter-token latency per pod (in seconds).
	// Source: vllm:inter_token_latency_seconds histogram
	QueryAvgITL = "avg_itl"
)

// RegisterQueueingModelQueries registers queries used by the queueing model analyzer.
func RegisterQueueingModelQueries(sourceRegistry *source.SourceRegistry) {
	registry := sourceRegistry.Get("prometheus").QueryList()

	// Scheduler dispatch rate per endpoint (per-instance arrival rate)
	// Records successful scheduling attempts with endpoint and model information.
	// Metric labels: status, pod_name, namespace, port, model_name, target_model_name
	// We filter by status="success"; model identity comes from target_model_name
	// (resolved model after routing, e.g. specific LoRA adapter) with fallback to
	// model_name (original request model) when target_model_name is not set.
	// Both labels stay in the grouping key and the collector applies that rule per
	// series (see eppSeriesModel), which is what lets one namespace-scoped
	// execution serve every model — and replaces the "or" clause the model-scoped
	// version needed to express the same fallback in PromQL.
	// Uses sum (not max) because dispatch rate is an additive counter — multiple
	// series per instance should be summed. Uses rate() over 1m window for requests/sec.
	// Groups by pod_name and port to uniquely identify each engine instance.
	registry.MustRegister(source.QueryTemplate{
		Name:     QuerySchedulerDispatchRate,
		Type:     source.QueryTypePromQL,
		Template: `sum by (namespace, model_name, target_model_name, pod_name, port) (rate(inference_extension_scheduler_attempts_total{status="success",namespace="{{.namespace}}"}[1m]))`,
		Params:   []string{source.ParamNamespace},
		Description: "Request dispatch rate per endpoint (requests/sec) from scheduler, " +
			"representing the arrival rate to each replica, grouped by model, pod_name and port",
	})

	// Average inter-token latency per instance (seconds).
	// Uses histogram rate(sum[1m]) / rate(count[1m]) over a 1m sliding window.
	// Used by queueing model tuner as the observed ITL for Kalman filter updates.
	// Grouping key and namespace scoping: see the note above RegisterSaturationQueries
	registry.MustRegister(source.QueryTemplate{
		Name:     QueryAvgITL,
		Type:     source.QueryTypePromQL,
		Template: `max by (model_name, instance, pod) (rate(vllm:inter_token_latency_seconds_sum{namespace="{{.namespace}}"}[1m]) / rate(vllm:inter_token_latency_seconds_count{namespace="{{.namespace}}"}[1m]))`,
		Params:   []string{source.ParamNamespace},
		Description: "Average inter-token latency per instance (seconds), " +
			"used by queueing model tuner for parameter learning",
	})

	registerSGLangQueueingModelQueries(registry)
}

// registerSGLangQueueingModelQueries registers the SGLang variants of the
// engine-specific queueing-model queries. The scheduler dispatch-rate query above
// is engine-agnostic (sourced from EPP) and is not duplicated here.
func registerSGLangQueueingModelQueries(registry *source.QueryList) {
	// Average inter-token latency per instance (seconds), 1m sliding window.
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryAvgITL,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (rate(sglang:inter_token_latency_seconds_sum{namespace="{{.namespace}}"}[1m]) / rate(sglang:inter_token_latency_seconds_count{namespace="{{.namespace}}"}[1m]))`,
		Params:      []string{source.ParamNamespace},
		Description: "Average inter-token latency per instance (seconds) (SGLang)",
	})
}
