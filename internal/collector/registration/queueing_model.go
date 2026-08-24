// This file provides queueing model analyzer metrics collection using the source
// infrastructure with registered query templates.
package registration

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
)

// Query name constants for queueing model analyzer metrics.
const (
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
	//
	// The two histograms are NOT labelled identically upstream: e2e carries an
	// extra is_streaming label that queue_time does not (sglang's
	// python/sglang/srt/observability/metrics_collector.py builds e2e with
	// labelnames + ["is_streaming"] where queue_time uses labelnames alone). `max by (model_name,
	// instance, pod)` drops it on both sides so the subtraction still matches
	// series, but it means the e2e term is the max ACROSS streaming and
	// non-streaming rather than a pooled average, which biases W upward. That is
	// the safe direction for a floor -- it over-provisions rather than under --
	// and it is why this uses max rather than avg, consistent with the ITL query
	// above.
	//
	// Unverified against a live SGLang: whether queue_time is observed for every
	// request or only for requests that actually queued. If the latter, its mean
	// is taken over a biased subset and the subtraction systematically
	// understates the wait, which would inflate W further -- again the safe
	// direction, but worth confirming before anyone relies on the magnitude.
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:     QueryAvgServiceTime,
		Type:     source.QueryTypePromQL,
		Template: `clamp_min(max by (model_name, instance, pod) (rate(sglang:e2e_request_latency_seconds_sum{namespace="{{.namespace}}"}[1m]) / rate(sglang:e2e_request_latency_seconds_count{namespace="{{.namespace}}"}[1m])) - max by (model_name, instance, pod) (rate(sglang:queue_time_seconds_sum{namespace="{{.namespace}}"}[1m]) / rate(sglang:queue_time_seconds_count{namespace="{{.namespace}}"}[1m])), 0)`,
		Params:   []string{source.ParamNamespace},
		Description: "Average per-request service time excluding queue wait (seconds) " +
			"(SGLang, reconstructed as e2e minus queue)",
	})

}
