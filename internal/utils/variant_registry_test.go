package utils

import (
	"context"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/registry"
)

// Discovery from the registry: a workload exists because KEDA called WVA about
// it, and its identity comes from the trigger metadata that call carried. No
// annotation, and nothing listed.
//
// See docs/plans/engine/keda-driven-discovery.md.

// registerEnriched puts an entry in reg as if KEDA had called about it and the
// enricher had since resolved its ScaledObject.
func registerEnriched(reg *registry.Registry, namespace, name, target, modelID string) {
	reg.Observe(namespace, name, map[string]string{
		registry.ModelIDKey:     modelID,
		registry.VariantCostKey: "7.5",
	})
	maxR := int32(6)
	reg.SetTarget(namespace, name, registry.Target{
		APIVersion:  "apps/v1",
		Kind:        "Deployment",
		Name:        target,
		MaxReplicas: &maxR,
	})
}

func TestRegisteredWorkloadBecomesAVariant(t *testing.T) {
	ctx := context.Background()
	cl := fake.NewClientBuilder().WithScheme(variantTestScheme(t)).Build()

	reg := registry.New(time.Minute)
	registerEnriched(reg, "ns1", "chat-so", "chat-deploy", "meta/llama-3-8b")

	result := readyVariantAutoscalings(ctx, cl, reg)
	if len(result) != 1 {
		t.Fatalf("want 1 registry-sourced variant, got %d", len(result))
	}
	va := result[0]

	// The ScaledObject names the variant — there is one per scale target.
	if va.Name != "chat-so" || va.Namespace != "ns1" {
		t.Errorf("identity should come from the ScaledObject: %s/%s", va.Namespace, va.Name)
	}
	if va.Spec.ModelID != "meta/llama-3-8b" {
		t.Errorf("modelID should come from trigger metadata, got %q", va.Spec.ModelID)
	}
	if va.Spec.VariantCost != "7.5" {
		t.Errorf("variantCost should come from trigger metadata, got %q", va.Spec.VariantCost)
	}
	if va.Spec.ScaleTargetRef.Name != "chat-deploy" {
		t.Errorf("scale target should come from enrichment, got %q", va.Spec.ScaleTargetRef.Name)
	}
	if va.Spec.MaxReplicas != 6 {
		t.Errorf("the envelope should come from the ScaledObject, got max=%d", va.Spec.MaxReplicas)
	}
	if !annotations.IsSynthetic(&va) {
		t.Error("a registry-sourced variant is in-memory only and must be marked synthetic")
	}
}

// TestUnenrichedEntryIsSkipped: registration and enrichment are separate steps,
// and between them there is no scale target to collect from or scale. Emitting a
// variant with a blank target would put a phantom in the fleet the optimizer
// balances against.
func TestUnenrichedEntryIsSkipped(t *testing.T) {
	ctx := context.Background()
	cl := fake.NewClientBuilder().WithScheme(variantTestScheme(t)).Build()

	reg := registry.New(time.Minute)
	reg.Observe("ns1", "chat-so", map[string]string{registry.ModelIDKey: "meta/llama-3-8b"})

	if result := readyVariantAutoscalings(ctx, cl, reg); len(result) != 0 {
		t.Errorf("want no variant before the scale target is resolved, got %d", len(result))
	}
}

// TestVariantNameMetadataResolvesWithoutEnrichment is the escape hatch: a trigger
// that names its target directly needs no ScaledObject read to become a variant.
func TestVariantNameMetadataResolvesWithoutEnrichment(t *testing.T) {
	ctx := context.Background()
	cl := fake.NewClientBuilder().WithScheme(variantTestScheme(t)).Build()

	reg := registry.New(time.Minute)
	reg.Observe("ns1", "chat-so", map[string]string{
		registry.ModelIDKey:     "meta/llama-3-8b",
		registry.VariantNameKey: "chat-deploy",
	})

	result := readyVariantAutoscalings(ctx, cl, reg)
	if len(result) != 1 {
		t.Fatalf("want 1 variant from metadata alone, got %d", len(result))
	}
	if got := result[0].Spec.ScaleTargetRef.Name; got != "chat-deploy" {
		t.Errorf("scale target: got %q", got)
	}
	// KEDA's own scaleTargetRef defaults must be applied, since no object was read.
	if result[0].Spec.ScaleTargetRef.Kind != "Deployment" || result[0].Spec.ScaleTargetRef.APIVersion != "apps/v1" {
		t.Errorf("want KEDA's defaults, got %s %s",
			result[0].Spec.ScaleTargetRef.APIVersion, result[0].Spec.ScaleTargetRef.Kind)
	}
}

// TestUnusableTriggerMetadataIsSkipped: modelID is the grouping key for every
// multi-variant decision, so a variant without one cannot be optimized against a
// model. It is dropped here rather than at registration, where refusing it would
// make the misconfiguration invisible instead of merely inert.
func TestUnusableTriggerMetadataIsSkipped(t *testing.T) {
	ctx := context.Background()
	cl := fake.NewClientBuilder().WithScheme(variantTestScheme(t)).Build()

	reg := registry.New(time.Minute)
	reg.Observe("ns1", "chat-so", map[string]string{registry.VariantNameKey: "chat-deploy"})

	if result := readyVariantAutoscalings(ctx, cl, reg); len(result) != 0 {
		t.Errorf("want no variant from a trigger with no modelID, got %d", len(result))
	}
}

// TestExpiredEntryYieldsNoVariant: the TTL is what replaces deletion, so a
// workload KEDA has stopped calling about must leave the fleet.
func TestExpiredEntryYieldsNoVariant(t *testing.T) {
	ctx := context.Background()
	cl := fake.NewClientBuilder().WithScheme(variantTestScheme(t)).Build()

	// A TTL already elapsed: nothing has been observed since, so the entry is dead.
	reg := registry.New(time.Nanosecond)
	registerEnriched(reg, "ns1", "chat-so", "chat-deploy", "meta/llama-3-8b")
	time.Sleep(time.Millisecond)

	if result := readyVariantAutoscalings(ctx, cl, reg); len(result) != 0 {
		t.Errorf("want no variant once the entry has expired, got %d", len(result))
	}
}

// TestRegistryWinsOverTheAnnotationForTheSameTarget. Both discovery paths are
// live during the migration, and a workload that has been given trigger metadata
// while its annotations are still in place must be optimized ONCE — and on the
// evidence that KEDA is actively calling about it, not on an annotation someone
// left behind.
func TestRegistryWinsOverTheAnnotationForTheSameTarget(t *testing.T) {
	ctx := context.Background()
	cl := fake.NewClientBuilder().WithScheme(variantTestScheme(t)).WithObjects(
		managedSO("ns1", "chat-so", "chat-deploy", "stale-model-from-annotation"),
	).Build()

	reg := registry.New(time.Minute)
	registerEnriched(reg, "ns1", "chat-so", "chat-deploy", "model-from-trigger")

	result := readyVariantAutoscalings(ctx, cl, reg)
	if len(result) != 1 {
		t.Fatalf("the same scale target must yield one variant, got %d", len(result))
	}
	if result[0].Spec.ModelID != "model-from-trigger" {
		t.Errorf("the registry must win, got modelID %q", result[0].Spec.ModelID)
	}
}

// TestAnnotationSurvivesForAWorkloadNotYetRegistered: the migration has to be
// gradual, so a ScaledObject that has not been called about (or whose WVA has
// just restarted) must keep working off its annotations.
func TestAnnotationSurvivesForAWorkloadNotYetRegistered(t *testing.T) {
	ctx := context.Background()
	cl := fake.NewClientBuilder().WithScheme(variantTestScheme(t)).WithObjects(
		managedSO("ns1", "legacy-so", "legacy-deploy", "legacy-model"),
	).Build()

	result := readyVariantAutoscalings(ctx, cl, registry.New(time.Minute))
	if len(result) != 1 {
		t.Fatalf("want the annotated workload to survive, got %d", len(result))
	}
	if result[0].Spec.ModelID != "legacy-model" {
		t.Errorf("modelID: got %q", result[0].Spec.ModelID)
	}
}
