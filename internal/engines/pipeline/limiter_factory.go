package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/discovery"
)

// NewLimiterFromConfig constructs the GPU limiter selected via
// Config.EffectiveLimiterMode — the inline limiters: list on the saturation
// "default" config.
//
//   - LimiterTypeNone: no limiters declared, so nothing limits. Returns a
//     NoOpLimiter, which provides no constraints, so the optimizer runs
//     unconstrained and a scale-from-zero wake is published without a capacity
//     check. An operator who declares no limiter gets no limiter — there is no
//     implicit default one, and no enable flag that could leave a declared
//     limiter inert.
//   - LimiterTypeInventory: a TypeInventoryWithUsage wrapped in a DefaultLimiter.
//     Discovers physical GPUs via the GPU operator.
//   - LimiterTypeQuota: builds one DefaultLimiter per Config.EffectiveQuotaEntries
//     entry, each wrapping a QuotaInventory. Multiple entries are grouped in a
//     CompositeLimiter, and the engine computes constraints from each. Pure
//     operator-declared caps — physical capacity is NOT consulted.
//
// The kubeClient is only used by the inventory path (for GPU operator
// discovery); the quota and none paths ignore it. Inline limiter entries are
// validated at ConfigMap parse time (SaturationScalingConfig.validateLimiters),
// so unknown limiter types reaching the default branch represent a programming
// error.
func NewLimiterFromConfig(cfg *config.Config, kubeClient client.Client) (Limiter, error) {
	switch t := cfg.EffectiveLimiterMode(); t {
	case config.LimiterTypeNone:
		return NewNoOpLimiter("no-limiter"), nil
	case config.LimiterTypeInventory:
		return newInventoryLimiter(kubeClient), nil
	case config.LimiterTypeQuota:
		return newQuotaLimiter(cfg)
	default:
		return nil, fmt.Errorf("limiter factory: unknown limiter type %q (valid: %q, %q, %q)",
			t, config.LimiterTypeNone, config.LimiterTypeInventory, config.LimiterTypeQuota)
	}
}

// LimiterSignature is a deterministic fingerprint of the config inputs that
// determine the GPU limiter, so an engine can detect when a rebuild is needed
// without rebuilding every cycle. Quota entry maps are marshaled with sorted keys
// by encoding/json, so equal configs always produce equal signatures.
//
// Shared by every engine that holds a limiter. They must agree on what "the
// config changed" means, or one of them keeps serving a stale limiter after an
// edit — which is exactly what happened while the scale-from-zero engine built
// its limiter once at startup: the ConfigMap documented limiters as applied live,
// and half the system ignored the edit.
func LimiterSignature(cfg *config.Config) string {
	entries, _ := json.Marshal(cfg.EffectiveQuotaEntries())
	return string(cfg.EffectiveLimiterMode()) + "|" + string(entries)
}

// newInventoryLimiter builds the physical-capacity GPU limiter: a
// TypeInventoryWithUsage (GPUs discovered via the GPU operator) wrapped in a
// DefaultLimiter.
func newInventoryLimiter(kubeClient client.Client) Limiter {
	gpuDiscovery := discovery.NewK8sWithGpuOperator(kubeClient)
	gpuInventory := NewTypeInventoryWithUsage("cluster-gpu-inventory", gpuDiscovery)
	return NewDefaultLimiter("gpu-limiter", gpuInventory)
}

// newQuotaLimiter builds one DefaultLimiter per QuotaLimiterConfig entry, each
// wrapping a QuotaInventory. When more than one entry is configured, the result
// is grouped in a CompositeLimiter so every entry's constraints are consulted.
func newQuotaLimiter(cfg *config.Config) (Limiter, error) {
	entries := cfg.EffectiveQuotaEntries()
	if len(entries) == 0 {
		return nil, errors.New("limiter factory: quota mode requires at least one inline " +
			"limiters: quota entry on the saturation \"default\" config")
	}
	constituents := make([]Limiter, 0, len(entries))
	for _, entry := range entries {
		constituents = append(constituents, NewDefaultLimiter(entry.Name, NewQuotaInventory(entry)))
	}
	if len(constituents) == 1 {
		return constituents[0], nil
	}
	return NewCompositeLimiter("quota-limiter", constituents), nil
}
