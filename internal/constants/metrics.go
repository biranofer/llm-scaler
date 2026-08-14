// Package constants provides centralized constant definitions for the autoscaler.
// This file contains metric-related constants (VLLM input metrics, WVA output metrics, and metric label names).
package constants

// VLLM Input Metrics
// These metric names are used to query VLLM (vLLM inference engine) metrics from Prometheus.
// The metrics are emitted by VLLM servers and consumed by the collector to make scaling decisions.
const (
	// VLLMNumRequestRunning tracks the current number of running requests.
	// Used to validate metrics availability.
	VLLMNumRequestRunning = "vllm:num_requests_running"

	// VLLMRequestSuccessTotal tracks the total number of successful requests.
	// Used to calculate arrival rate.
	VLLMRequestSuccessTotal = "vllm:request_success_total"

	// VLLMRequestPromptTokensSum tracks the sum of prompt tokens across all requests.
	// Used with VLLMRequestPromptTokensCount to calculate average output tokens.
	VLLMRequestPromptTokensSum = "vllm:request_prompt_tokens_sum"

	// VLLMRequestPromptTokensCount tracks the count of requests for token generation.
	// Used with VLLMRequestPromptTokensSum to calculate average output tokens.
	VLLMRequestPromptTokensCount = "vllm:request_prompt_tokens_count"

	// VLLMRequestGenerationTokensSum tracks the sum of generated tokens across all requests.
	// Used with VLLMRequestGenerationTokensCount to calculate average output tokens.
	VLLMRequestGenerationTokensSum = "vllm:request_generation_tokens_sum"

	// VLLMRequestGenerationTokensCount tracks the count of requests for token generation.
	// Used with VLLMRequestGenerationTokensSum to calculate average output tokens.
	VLLMRequestGenerationTokensCount = "vllm:request_generation_tokens_count"

	// VLLMTimeToFirstTokenSecondsSum tracks the sum of TTFT (Time To First Token) across all requests.
	// Used with VLLMTimeToFirstTokenSecondsCount to calculate TTFT.
	VLLMTimeToFirstTokenSecondsSum = "vllm:time_to_first_token_seconds_sum"

	// VLLMTimeToFirstTokenSecondsCount tracks the count of requests for TTFT.
	// Used with VLLMTimeToFirstTokenSecondsSum to calculate TTFT.
	VLLMTimeToFirstTokenSecondsCount = "vllm:time_to_first_token_seconds_count"

	// VLLMTimePerOutputTokenSecondsSum tracks the sum of time per output token across all requests.
	// Used with VLLMTimePerOutputTokenSecondsCount to calculate ITL (Inter-Token Latency).
	VLLMTimePerOutputTokenSecondsSum = "vllm:time_per_output_token_seconds_sum"

	// VLLMTimePerOutputTokenSecondsCount tracks the count of requests for time per output token.
	// Used with VLLMTimePerOutputTokenSecondsSum to calculate ITL (Inter-Token Latency).
	VLLMTimePerOutputTokenSecondsCount = "vllm:time_per_output_token_seconds_count"

	// VLLMKvCacheUsagePerc tracks the KV cache utilization as a percentage (0.0-1.0).
	// Used by saturation analyzer to detect KV cache saturation and prevent OOM errors.
	VLLMKvCacheUsagePerc = "vllm:kv_cache_usage_perc"

	// VLLMNumRequestsWaiting tracks the number of requests waiting in the queue.
	// Used by saturation analyzer to detect request queue saturation.
	VLLMNumRequestsWaiting = "vllm:num_requests_waiting"

	// VLLMCacheConfigInfo is an info-style gauge that exposes KV cache configuration as labels.
	// Labels include num_gpu_blocks, block_size, cache_dtype, etc.
	// Value is always 1.0. Used by Saturation Analyzer V2 for token capacity computation.
	VLLMCacheConfigInfo = "vllm:cache_config_info"

	// VLLMPrefixCacheHits is a counter of prefix cache block hits.
	// Used with VLLMPrefixCacheQueries to compute prefix cache hit rate.
	VLLMPrefixCacheHits = "vllm:prefix_cache_hits"

	// VLLMPrefixCacheQueries is a counter of prefix cache block queries.
	// Used with VLLMPrefixCacheHits to compute prefix cache hit rate.
	VLLMPrefixCacheQueries = "vllm:prefix_cache_queries"
)

// SGLang Input Metrics
// These metric names are used to query SGLang inference engine metrics from Prometheus.
// Names and types were taken from SGLang's metrics collector
// (python/sglang/srt/observability/metrics_collector.py). SGLang labels its metrics
// with model_name, matching the label WVA filters on. See docs/proposals/sglang-backend.md.
const (
	// SGLangNumRunningReqs is the number of running requests (gauge).
	// SGLang equivalent of vllm:num_requests_running.
	SGLangNumRunningReqs = "sglang:num_running_reqs"

	// SGLangNumQueueReqs is the number of requests in the waiting queue (gauge).
	// SGLang equivalent of vllm:num_requests_waiting.
	SGLangNumQueueReqs = "sglang:num_queue_reqs"

	// SGLangTokenUsage is the KV-cache token-pool utilization as a fraction 0.0-1.0 (gauge).
	// SGLang equivalent of vllm:kv_cache_usage_perc.
	SGLangTokenUsage = "sglang:token_usage"

	// SGLangMaxTotalNumTokens is the total KV-cache token capacity (gauge).
	// SGLang exposes capacity directly, unlike vLLM which derives it from
	// vllm:cache_config_info (num_gpu_blocks x block_size).
	SGLangMaxTotalNumTokens = "sglang:max_total_num_tokens"

	// SGLangTimeToFirstTokenSecondsSum is the sum part of the TTFT histogram.
	// SGLang equivalent of vllm:time_to_first_token_seconds_sum.
	SGLangTimeToFirstTokenSecondsSum = "sglang:time_to_first_token_seconds_sum"

	// SGLangTimeToFirstTokenSecondsCount is the count part of the TTFT histogram.
	// SGLang equivalent of vllm:time_to_first_token_seconds_count.
	SGLangTimeToFirstTokenSecondsCount = "sglang:time_to_first_token_seconds_count"

	// SGLangInterTokenLatencySecondsSum is the sum part of the inter-token-latency histogram.
	// SGLang equivalent of vllm:inter_token_latency_seconds_sum.
	SGLangInterTokenLatencySecondsSum = "sglang:inter_token_latency_seconds_sum"

	// SGLangInterTokenLatencySecondsCount is the count part of the inter-token-latency histogram.
	// SGLang equivalent of vllm:inter_token_latency_seconds_count.
	SGLangInterTokenLatencySecondsCount = "sglang:inter_token_latency_seconds_count"

	// SGLangPromptTokensHistogramSum is the sum part of the prompt-token histogram.
	// SGLang equivalent of vllm:request_prompt_tokens_sum.
	SGLangPromptTokensHistogramSum = "sglang:prompt_tokens_histogram_sum"

	// SGLangPromptTokensHistogramCount is the count part of the prompt-token histogram.
	// SGLang equivalent of vllm:request_prompt_tokens_count.
	SGLangPromptTokensHistogramCount = "sglang:prompt_tokens_histogram_count"

	// SGLangGenerationTokensHistogramSum is the sum part of the generation-token histogram.
	// SGLang equivalent of vllm:request_generation_tokens_sum.
	SGLangGenerationTokensHistogramSum = "sglang:generation_tokens_histogram_sum"

	// SGLangGenerationTokensHistogramCount is the count part of the generation-token histogram.
	// SGLang equivalent of vllm:request_generation_tokens_count.
	SGLangGenerationTokensHistogramCount = "sglang:generation_tokens_histogram_count"

	// SGLangCachedTokensTotal is a counter of prompt tokens served from the prefix cache.
	// Used with SGLangPromptTokensTotal to compute the prefix cache hit rate, the
	// unit-safe analog of vllm:prefix_cache_hits / vllm:prefix_cache_queries.
	SGLangCachedTokensTotal = "sglang:cached_tokens_total"

	// SGLangPromptTokensTotal is a counter of total prompt tokens.
	SGLangPromptTokensTotal = "sglang:prompt_tokens_total"

	// SGLangNumRequestsTotal is a counter of received requests.
	// SGLang equivalent of vllm:request_success_total (used for scale-to-zero).
	SGLangNumRequestsTotal = "sglang:num_requests_total"
)

// llm-d Inference Scheduler Flow Control Metrics
// These metrics come from the Gateway API Inference Extension EPP (Endpoint Picker)
// flow control layer, not from vLLM pods. They are model-scoped (not per-pod).
//
// TODO(#2309): These metrics currently lack a namespace label upstream.
// If the same model and inference pool names exist in different namespaces,
// the metrics will collide. See gateway-api-inference-extension issue #2309.
const (
	// EPPFlowControlQueueSize is the number of requests queued in the inference
	// scheduler's flow control layer. This is the current name for the family
	// below: llm-d's EPP exports both and marks the inference_extension_ one
	// "[Deprecated: Use llm_d_epp_flow_control_queue_size]". Prefer this one and
	// keep the deprecated name only as a fallback.
	// Labels: fairness_id, priority, inference_pool, model_name, target_model_name
	// Note: no namespace label — see TODO(#2309) above.
	EPPFlowControlQueueSize = "llm_d_epp_flow_control_queue_size"

	// SchedulerFlowControlQueueSize is the deprecated alias of
	// EPPFlowControlQueueSize. Upstream gateway-api-inference-extension still
	// emits only this name (and without the inference_pool/model_name/
	// target_model_name labels llm-d's EPP adds), so it remains the fallback.
	// Labels: fairness_id, priority, inference_pool, model_name, target_model_name
	// Note: no namespace label — see TODO(#2309) above.
	SchedulerFlowControlQueueSize = "inference_extension_flow_control_queue_size"

	// SchedulerFlowControlQueueBytes is the total bytes of request bodies queued
	// in the inference scheduler's flow control layer.
	// Labels: fairness_id, priority, inference_pool, model_name, target_model_name
	// Note: no namespace label — see TODO(#2309) above.
	SchedulerFlowControlQueueBytes = "inference_extension_flow_control_queue_bytes"
)

// WVA Output Metrics
// These metric names are used to emit WVA (Workload Variant Autoscaler) metrics to Prometheus.
// The metrics expose scaling decisions and current state for monitoring and alerting.
const (
	// WVAReplicaScalingTotal is a counter that tracks the total number of scaling operations.
	// Labels: variant_name, namespace, direction (up/down), reason, accelerator_type
	WVAReplicaScalingTotal = "wva_replica_scaling_total"

	// WVADesiredReplicas is a gauge that tracks the desired number of replicas.
	// Labels: variant_name, namespace, accelerator_type
	WVADesiredReplicas = "wva_desired_replicas"

	// WVAUnattributedGPUs counts GPUs held by variants whose accelerator type
	// could not be resolved. Such usage cannot be charged to any accelerator
	// pool, so every pool over-states how much is free by this amount and the
	// budget check lets wakes through that should have been refused. It is
	// exported because the condition is otherwise entirely silent — nothing
	// errors, capacity simply looks larger than it is.
	WVAUnattributedGPUs = "wva_unattributed_gpus"

	// WVACurrentReplicas is a gauge that tracks the current number of replicas.
	// Labels: variant_name, namespace, accelerator_type
	WVACurrentReplicas = "wva_current_replicas"

	// WVADesiredRatio is a gauge that tracks the ratio of desired to current replicas.
	// Labels: variant_name, namespace, accelerator_type
	WVADesiredRatio = "wva_desired_ratio"

	// WVAOptimizationDurationSeconds is a histogram that tracks the duration of each optimization cycle.
	// Labels: status (success, error)
	WVAOptimizationDurationSeconds = "wva_optimization_duration_seconds"

	// WVAModelsProcessed is a gauge that tracks the number of models processed in the last optimization cycle.
	WVAModelsProcessed = "wva_models_processed"

	// WVADecisionsLimitedTotal is a counter that tracks the total number of decisions limited by the limiter.
	// Labels: variant_name, namespace, limiter_name
	WVADecisionsLimitedTotal = "wva_decisions_limited_total"

	// WVAGpuDiscoveryUp is a gauge that indicates whether GPU discovery is on or off.
	WVAGpuDiscoveryUp = "wva_gpu_discovery_up"

	// WVANodeAccessDenied is 1 while a configured physical limiter cannot read
	// nodes, and 0 when it can.
	//
	// This is the one combination that fails silently. A GPU-aware limiter
	// allocates out of per-accelerator pools, so a variant whose accelerator it
	// cannot resolve is charged to no pool, receives no budget, and never scales
	// up — with no error anywhere, because an unresolved accelerator is a normal
	// state when no limiter is configured. Node permission is optional right up
	// until someone turns the limiter on, and then it is load-bearing.
	WVANodeAccessDenied = "wva_node_access_denied"

	// WVAScaleFromZeroQueueFallbackActive is 1 while the scale-from-zero engine is
	// reading the EPP flow-control queue from Prometheus because the direct EPP
	// scrape is failing, and 0 while the direct scrape works. A sustained 1 means
	// wakes still happen but are slower and bounded by the Prometheus scrape
	// interval, and that the EPP metrics path (token, RBAC, NetworkPolicy) is
	// broken and should be fixed.
	WVAScaleFromZeroQueueFallbackActive = "wva_scale_from_zero_queue_fallback_active"

	// WVAAvailableGpus is a gauge that tracks the number of currently available GPUs. If wva_gpu_discovery_up is 1, it shows
	// the number of currently available GPUs. If wva_gpu_discovery_up is 0, it shows the number
	// of GPUs that were available at the last successful discovery.
	// Labels: accelerator_type
	WVAAvailableGpus = "wva_available_gpus"

	// WVAEnforcerModificationsTotal is a counter that tracks the total number of decision modifications made by the enforcer.
	// Labels: policy_type
	WVAEnforcerModificationsTotal = "wva_enforcer_modifications_total"

	// WVAOptimizerActive is a gauge that is 0 when an optimizer is inactive, and 1 when it's active.
	// Labels: optimizer_name
	WVAOptimizerActive = "wva_optimizer_active"
	// WVAErrorsTotal is a counter that tracks the total number of errors by component.
	// Labels: component, error_type
	WVAErrorsTotal = "wva_errors_total"
	// WVAAnalyzerDemand is a gauge exposing each analyzer's demand D (per model,
	// per role). Labels: analyzer_name, namespace, model_name, role.
	WVAAnalyzerDemand = "wva_analyzer_demand"
	// WVAAnalyzerTarget is a gauge exposing each analyzer's per-replica target P
	// (per variant). Labels: analyzer_name, namespace, model_name, variant_name.
	WVAAnalyzerTarget = "wva_analyzer_target"
	// WVAConfigInfo is an info-style gauge that exposes WVA configuration as labels.
	// Labels: analyzer_name, limiter_enabled, scale_to_zero_enabled
	WVAConfigInfo = "wva_config_info"

	// WVAConfigOptimizationIntervalSeconds is a gauge that tracks the optimization interval in seconds.
	WVAConfigOptimizationIntervalSeconds = "wva_config_optimization_interval_seconds"
	// WVAMetricsCollectionDurationSeconds is a histogram that tracks the duration of metrics collection operations.
	// Labels: query_type
	WVAMetricsCollectionDurationSeconds = "wva_metrics_collection_duration_seconds"

	// WVAMetricsCollectionErrorsTotal is a counter that tracks the total number of metrics collection errors.
	// Labels: query_type, reason
	WVAMetricsCollectionErrorsTotal = "wva_metrics_collection_errors_total"

	// WVAMetricsPodsDiscovered is a gauge that tracks the number of pods discovered for metrics collection.
	// Labels: namespace
	WVAMetricsPodsDiscovered = "wva_metrics_pods_discovered"

	// WVAMetricsFreshnessStatus is a gauge that tracks the freshness status of metrics for each variant.
	// Labels: variant_name, status
	WVAMetricsFreshnessStatus = "wva_metrics_freshness_status"

	// WVASaturationUtilization is a gauge that tracks per-variant utilization ratio (0.0-1.0).
	// Labels: variant_name, namespace, model_name, accelerator_type
	WVASaturationUtilization = "wva_saturation_utilization"

	// WVASpareCapacity is a gauge that tracks spare capacity; >0 means scale-down
	// headroom (per-role for P/D-disaggregated models, model-level otherwise).
	// The value is a token surplus, carrying unit="continuous".
	// Labels: variant_name, namespace, model_name, unit
	WVASpareCapacity = "wva_spare_capacity"

	// WVARequiredCapacity is a gauge that tracks required capacity; >0 means scale-up
	// needed (per-role for P/D-disaggregated models, model-level otherwise).
	// The value is a token demand, carrying unit="continuous".
	// Labels: variant_name, namespace, model_name, unit
	WVARequiredCapacity = "wva_required_capacity"

	// WVAKvCacheTokensUsed is a gauge that tracks total KV cache tokens currently in use per variant.
	// Labels: variant_name, namespace, model_name
	WVAKvCacheTokensUsed = "wva_kv_cache_tokens_used"

	// WVAKvCacheTokensCapacity is a gauge that tracks total KV cache token capacity per variant.
	// Labels: variant_name, namespace, model_name
	WVAKvCacheTokensCapacity = "wva_kv_cache_tokens_capacity"

	// WVASaturationMetricsUp is a per-VA freshness signal for the five
	// saturation/capacity gauges above. Set to 1.0 in cycles where the
	// optimizer produced a fresh decision for the variant (i.e. the other
	// gauges were just refreshed), and 0.0 in cycles where the analyzer was
	// aware of the variant but no fresh decision was emitted. Lets
	// dashboards distinguish "the system says utilization is X" from "the
	// system has not updated utilization in N minutes and X is the stalest
	// sample" without relying on Prometheus' 5-minute staleness marker.
	// Labels: variant_name, namespace
	WVASaturationMetricsUp = "wva_saturation_metrics_up"

	// WVAPodMappingMissTotal is a counter that tracks pods whose metrics could not be
	// attributed to a managed scaler: the walk up their ownerReferences ended
	// without reaching one. Makes the otherwise-silent skip visible.
	// Labels: namespace, reason
	WVAPodMappingMissTotal = "wva_pod_mapping_miss_total"

	// WVAUnmeasuredQueue is a gauge carrying the number of requests queued for a
	// model that WVA has NO attributed replica to act on.
	//
	// It separates two states the logs used to render identically. "No saturation
	// metrics available, skipping analysis" is the correct and quiet answer for a
	// model that is scaled to zero and idle. It is an emergency for a model that
	// is serving traffic through pods WVA cannot attribute — an FMA topology
	// whose launchers are unscraped, a PodMonitor that selects by a port the pods
	// do not declare, a workload whose ownerReferences reach no scale target.
	// Both produced the same line, so the second was invisible.
	//
	// Non-zero means: requests are queued, and the autoscaler is not going to do
	// anything about them. Sourced from the model-level scheduler flow-control
	// queue, which comes from EPP and is therefore independent of whether any
	// engine pod is scraped.
	//
	// Labels: namespace, model_name
	WVAUnmeasuredQueue = "wva_unmeasured_queue"
)

// Pod-mapping miss reasons (values for the `reason` label of WVAPodMappingMissTotal).
const (
	// PodMappingMissUnresolved indicates a scraped pod resolved to no managed scaler:
	// the locator found no owning ScaledObject above it. A pod owned by something
	// else entirely — an FMA launcher, owned by a LauncherConfig — counts here.
	PodMappingMissUnresolved = "unresolved"
)

// Metric Label Names
// Common label names used across metrics for consistency.
const (
	LabelModelName          = "model_name"
	LabelNamespace          = "namespace"
	LabelComponent          = "component"
	LabelVariantName        = "variant_name"
	LabelDirection          = "direction"
	LabelReason             = "reason"
	LabelAcceleratorVendor  = "accelerator_vendor"
	LabelAcceleratorModel   = "accelerator_model"
	LabelAcceleratorType    = "accelerator_type"
	LabelControllerInstance = "controller_instance"
	LabelRole               = "role"
	LabelStatus             = "status"
	LabelLimiterName        = "limiter_name"
	LabelPolicyType         = "policy_type"
	LabelOptimizerName      = "optimizer_name"
	LabelErrorType          = "error_type"
	LabelAnalyzerName       = "analyzer_name"
	LabelLimiterEnabled     = "limiter_enabled"
	LabelScaleToZeroEnabled = "scale_to_zero_enabled"
	LabelQueryType          = "query_type"
	// LabelUnit names the unit of a metric value. Applied to
	// wva_required_capacity and wva_spare_capacity, whose values are a
	// "continuous" token magnitude. It is retained as a stable part of those
	// series' identity so existing queries and alerts keep matching.
	LabelUnit = "unit"
)

// Metric Label Values for query_type
// These values are used as the query_type label in metrics collection metrics.
const (
	QueryTypeKVCache      = "kv_cache"
	QueryTypeQueueLength  = "queue_length"
	QueryTypeRequestCount = "request_count"
	QueryTypeCacheConfig  = "cache_config"
	QueryTypeArrivalRate  = "arrival_rate"
)

// Value for the LabelUnit Prometheus label: the metric carries an absolute
// quantity rather than a normalized ratio.
const (
	UnitContinuous = "continuous"
)
