package saturation

import (
	"testing"

	wvav1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// WVA scales a MODEL: the optimizer distributes its replicas across its variants
// against one set of thresholds. Variants naming different tiers therefore have no
// correct merge, so the choice must at least be deterministic — an allocation that
// flips tier as map order changes is worse than either tier.
func TestModelPolicyIsDeterministicWhenVariantsDisagree(t *testing.T) {
	vas := []wvav1alpha1.VariantAutoscaling{
		{Spec: wvav1alpha1.VariantAutoscalingSpec{ScalingPolicy: "interactive"}},
		{Spec: wvav1alpha1.VariantAutoscalingSpec{ScalingPolicy: "batch"}},
	}
	chosen, conflicting := modelPolicy(vas)
	if chosen != "batch" {
		t.Errorf("chosen = %q, want the lexicographically smallest (%q) for determinism", chosen, "batch")
	}
	if len(conflicting) != 2 {
		t.Errorf("conflicting = %v, want both tiers reported", conflicting)
	}

	// Reversing the input must not change the outcome.
	reversed, _ := modelPolicy([]wvav1alpha1.VariantAutoscaling{vas[1], vas[0]})
	if reversed != chosen {
		t.Errorf("input order changed the resolved policy: %q vs %q", reversed, chosen)
	}
}

func TestModelPolicyAgreementIsNotAConflict(t *testing.T) {
	vas := []wvav1alpha1.VariantAutoscaling{
		{Spec: wvav1alpha1.VariantAutoscalingSpec{ScalingPolicy: "interactive"}},
		{Spec: wvav1alpha1.VariantAutoscalingSpec{ScalingPolicy: "interactive"}},
		{Spec: wvav1alpha1.VariantAutoscalingSpec{}}, // names none: inherits, not a disagreement
	}
	chosen, conflicting := modelPolicy(vas)
	if chosen != "interactive" {
		t.Errorf("chosen = %q, want interactive", chosen)
	}
	if len(conflicting) > 1 {
		t.Errorf("conflicting = %v, want no conflict when the named tiers agree", conflicting)
	}
}
