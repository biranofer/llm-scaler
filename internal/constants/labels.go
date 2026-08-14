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

	// PolicyNamespaceLabelKey, on a NAMESPACE object, names the namespace WVA reads
	// limiters and quotas from for workloads in that namespace.
	//
	// It lives on the Namespace because that object is cluster-scoped: a namespace
	// admin holds RBAC INSIDE their namespace and cannot edit it. So this is a
	// pointer the subject of the policy can read but not rewrite.
	//
	// A namespace name is a valid label value, and a label is the form that can be
	// selected on. `kubectl get ns -l wva.llmd.ai/policy-namespace=platform-policy`
	// answers "which namespaces does this policy govern?" — the question an admin
	// actually asks when auditing, and one an annotation cannot answer.
	//
	PolicyNamespaceLabelKey = "wva.llmd.ai/policy-namespace"
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
