package registry

import "testing"

// TestParseMetaAcceptsAFullTrigger — the shape a working ScaledObject carries.
func TestParseMetaAcceptsAFullTrigger(t *testing.T) {
	m, err := ParseMeta(map[string]string{
		ScalerAddressKey: "wva-external-scaler.wva-system.svc:9090",
		ModelIDKey:       "default/default",
		VariantCostKey:   "12.5",
		VariantNameKey:   "sample-deployment",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.ModelID != "default/default" {
		t.Errorf("modelID: have %q", m.ModelID)
	}
	if m.VariantCost != "12.5" {
		t.Errorf("variantCost: have %q", m.VariantCost)
	}
	if m.VariantName != "sample-deployment" {
		t.Errorf("variantName: have %q", m.VariantName)
	}
}

// TestModelIDIsRequired: it is the grouping key for every multi-variant
// decision, so a variant without one cannot be optimized against a model at all.
func TestModelIDIsRequired(t *testing.T) {
	for _, meta := range []map[string]string{
		nil,
		{},
		{ModelIDKey: ""},
		{VariantCostKey: "1.0"},
	} {
		if _, err := ParseMeta(meta); err == nil {
			t.Errorf("expected an error for metadata %v", meta)
		}
	}
}

// TestVariantCostDefaults so a trigger that does not care about cost ranks where
// an unset cost has always ranked, rather than at zero.
func TestVariantCostDefaults(t *testing.T) {
	m, err := ParseMeta(map[string]string{ModelIDKey: "m"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.VariantCost != DefaultVariantCost {
		t.Errorf("want the default %q, have %q", DefaultVariantCost, m.VariantCost)
	}
}

// TestVariantCostIsValidated at the boundary, so no consumer downstream has to
// wonder whether the string parses.
func TestVariantCostIsValidated(t *testing.T) {
	for _, cost := range []string{"free", "-1", "1.0.0", "10 "} {
		if _, err := ParseMeta(map[string]string{ModelIDKey: "m", VariantCostKey: cost}); err == nil {
			t.Errorf("expected an error for variantCost %q", cost)
		}
	}
}

// TestVariantNameIsOptional — it only skips the ScaledObject read.
func TestVariantNameIsOptional(t *testing.T) {
	m, err := ParseMeta(map[string]string{ModelIDKey: "m"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.VariantName != "" {
		t.Errorf("expected no override, have %q", m.VariantName)
	}
}
