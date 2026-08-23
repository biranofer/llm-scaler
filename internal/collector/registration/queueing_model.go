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

	// QueryAvgServiceTime is the query name for how long a request occupies a
	// replica, EXCLUDING time spent waiting in the queue (seconds).
	//
	// The exclusion is the whole point. End-to-end latency rises when a fleet is
	// behind and falls when it catches up, so anything built on it varies with
	// capacity -- which is the property that makes occupancy unusable for sizing.
	// Service time does not: it is what one request costs to serve.
	//
	// vLLM publishes it directly as request_inference_time_seconds. SGLang has no
	// single equivalent but publishes both halves, so it is reconstructed there as
	// e2e minus queue.
	QueryAvgServiceTime = "avg_service_time"
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

	// Average per-request service time (seconds), queue wait excluded.
	// vLLM publishes this directly; SGLang has no equivalent and reconstructs it
	// from two histograms (see the SGLang registration below).
	registry.MustRegister(source.QueryTemplate{
		Name:     QueryAvgServiceTime,
		Type:     source.QueryTypePromQL,
		Template: `max by (model_name, instance, pod) (rate(vllm:request_inference_time_seconds_sum{namespace="{{.namespace}}"}[1m]) / rate(vllm:request_inference_time_seconds_count{namespace="{{.namespace}}"}[1m]))`,
		Params:   []string{source.ParamNamespace},
		Description: "Average per-request service time excluding queue wait (seconds), " +
			"used to size demand from the offered load",
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

	// Service time: SGLang has no single metric for it, but publishes both
	// halves, so subtract the queue wait from end-to-end. clamp_min guards the
	// window where the two rates disagree -- they cover the same interval but
	// count different request populations, so their difference can dip below
	// zero without either being wrong.
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:     QueryAvgServiceTime,
		Type:     source.QueryTypePromQL,
		Template: `clamp_min(max by (model_name, instance, pod) (rate(sglang:e2e_request_latency_seconds_sum{namespace="{{.namespace}}"}[1m]) / rate(sglang:e2e_request_latency_seconds_count{namespace="{{.namespace}}"}[1m])) - max by (model_name, instance, pod) (rate(sglang:queue_time_seconds_sum{namespace="{{.namespace}}"}[1m]) / rate(sglang:queue_time_seconds_count{namespace="{{.namespace}}"}[1m])), 0)`,
		Params:   []string{source.ParamNamespace},
		Description: "Average per-request service time excluding queue wait (seconds) " +
			"(SGLang, reconstructed as e2e minus queue)",
	})

}
