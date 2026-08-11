// Package constants provides centralized constant definitions for the autoscaler.
// This file contains Kubernetes label keys used for filtering and identification.
package constants

// Kubernetes Label Keys
// Label keys used on Kubernetes resources for filtering and identification.
const (
	// ControllerInstanceLabelKey is the label key used to associate VAs with specific controller instances.
	// Used for multi-controller isolation where each controller only manages VAs with matching labels.
	ControllerInstanceLabelKey = "wva.llmd.ai/controller-instance"

	// NamespaceConfigEnabledLabelKey is the label key used to opt-in namespaces for namespace-local ConfigMap overrides.
	// When a namespace has this label set to "true", the controller will watch for namespace-local ConfigMaps
	// even if no VariantAutoscaling resources exist in that namespace yet.
	// This enables creating namespace-local ConfigMaps before VAs are created, avoiding race conditions.
	NamespaceConfigEnabledLabelKey = "wva.llmd.ai/config-enabled"

	// VariantLabelKey is the label key stamped on pod templates of managed workloads.
	// The value is set to the VariantAutoscaling resource name, enabling direct pod-to-VA mapping
	// without ownership traversal. This label is used by the metrics collector to filter pods
	// and associate them with their managing VariantAutoscaling resource.
	VariantLabelKey = "llm-d.ai/variant"

	// PolicyNamespaceAnnotationKey, on a NAMESPACE object, names where WVA reads
	// limiters and quotas for workloads in that namespace.
	//
	// It lives on the Namespace deliberately. A namespace admin holds RBAC INSIDE
	// their namespace; the Namespace object itself is cluster-scoped, and editing
	// it is a permission they do not have. So this is a pointer the subject of the
	// policy can read but cannot rewrite — which the same value carried on the
	// controller's Deployment could never be, since a tenant who owns the namespace
	// owns that Deployment.
	PolicyNamespaceAnnotationKey = "wva.llmd.ai/policy-namespace"
)

// Kubernetes Annotation Keys
// Annotation keys used on Kubernetes resources for metadata and exclusion.
const (
	// NamespaceExcludeAnnotationKey is the annotation key used to exclude namespaces from WVA management.
	// When a namespace has this annotation set to "true", the controller will not watch it
	// for namespace-local ConfigMaps or reconcile VariantAutoscaling resources in it,
	// even if the namespace has VAs or opt-in labels.
	// This provides explicit control to exclude namespaces from WVA management.
	NamespaceExcludeAnnotationKey = "wva.llmd.ai/exclude"
)

// AnnotationValueTrue is the canonical string value for boolean annotations and labels.
const AnnotationValueTrue = "true"
