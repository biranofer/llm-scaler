// Package scaler implements KEDA's ExternalScaler gRPC service for WVA.
//
// WVA computes the desired replica count with its capacity model; this scaler
// delivers that decision to KEDA/HPA over the external-scaler contract, so KEDA
// actuates (no Prometheus round-trip, no prometheus-adapter). It resolves the
// scale target from the KEDA ScaledObject (namespace/name -> scaleTargetRef) and
// reads WVA's latest decision from the in-memory store the actuator feeds
// (internal/decision).
package scaler

import (
	"context"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
)

// MetricName is the external metric WVA exposes to KEDA/HPA. It is advertised
// with a target of 1, so HPA computes ceil(metricValue / 1) = metricValue and
// scales the target to exactly WVA's desired replica count.
const MetricName = "wva-desired-replicas"

// variantNameKey is an optional scalerMetadata override that names the scale
// target directly, skipping the ScaledObject read.
const variantNameKey = "variantName"

// Handler implements pb.ExternalScalerServer.
type Handler struct {
	pb.UnimplementedExternalScalerServer
	client client.Client
	store  *decision.Store
}

// NewHandler builds a Handler. A nil store falls back to decision.Default.
func NewHandler(c client.Client, store *decision.Store) *Handler {
	if store == nil {
		store = decision.Default
	}
	return &Handler{client: c, store: store}
}

// targetName resolves the scale-target name for a ScaledObjectRef. An explicit
// scalerMetadata["variantName"] wins (and avoids the read); otherwise it reads
// the KEDA ScaledObject and returns its scaleTargetRef.name.
func (h *Handler) targetName(ctx context.Context, ref *pb.ScaledObjectRef) (string, error) {
	if ref == nil {
		return "", status.Error(codes.InvalidArgument, "scaledObjectRef is required")
	}
	if v := ref.GetScalerMetadata()[variantNameKey]; v != "" {
		return v, nil
	}
	var so kedav1alpha1.ScaledObject
	nn := types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}
	if err := h.client.Get(ctx, nn, &so); err != nil {
		return "", status.Errorf(codes.NotFound, "getting ScaledObject %s: %v", nn, err)
	}
	if so.Spec.ScaleTargetRef == nil || so.Spec.ScaleTargetRef.Name == "" {
		return "", status.Errorf(codes.FailedPrecondition, "ScaledObject %s has no scaleTargetRef.name", nn)
	}
	return so.Spec.ScaleTargetRef.Name, nil
}

// desired returns WVA's latest desired replicas for the ref and whether a
// decision exists yet.
func (h *Handler) desired(ctx context.Context, ref *pb.ScaledObjectRef) (int32, bool, error) {
	name, err := h.targetName(ctx, ref)
	if err != nil {
		return 0, false, err
	}
	d, ok := h.store.Get(ref.Namespace, name)
	if !ok {
		return 0, false, nil
	}
	return d.DesiredReplicas, true, nil
}

// GetMetricSpec advertises the WVA metric with a target of 1 so HPA scales the
// target to exactly the value GetMetrics returns.
func (h *Handler) GetMetricSpec(_ context.Context, _ *pb.ScaledObjectRef) (*pb.GetMetricSpecResponse, error) {
	return &pb.GetMetricSpecResponse{
		MetricSpecs: []*pb.MetricSpec{{MetricName: MetricName, TargetSize: 1}},
	}, nil
}

// GetMetrics returns WVA's desired replica count as the metric value. Before the
// first optimization decision exists it returns 0, so HPA holds the target at
// minReplicaCount rather than acting on a guess.
func (h *Handler) GetMetrics(ctx context.Context, req *pb.GetMetricsRequest) (*pb.GetMetricsResponse, error) {
	d, ok, err := h.desired(ctx, req.GetScaledObjectRef())
	if err != nil {
		return nil, err
	}
	var value int64
	if ok {
		value = int64(d)
	}
	return &pb.GetMetricsResponse{
		MetricValues: []*pb.MetricValue{{MetricName: MetricName, MetricValue: value}},
	}, nil
}

// IsActive reports the target active unless WVA has decided it needs zero
// replicas. With no decision yet it reports active, so KEDA does not scale a
// freshly discovered target to zero before the first optimization runs.
func (h *Handler) IsActive(ctx context.Context, ref *pb.ScaledObjectRef) (*pb.IsActiveResponse, error) {
	d, ok, err := h.desired(ctx, ref)
	if err != nil {
		return nil, err
	}
	active := !ok || d > 0
	log.FromContext(ctx).V(1).Info("external scaler IsActive",
		"namespace", ref.GetNamespace(), "scaledObject", ref.GetName(), "active", active)
	return &pb.IsActiveResponse{Result: active}, nil
}
