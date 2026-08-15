package variant

import (
	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EventTarget returns the object a Kubernetes event about this variant should
// hang on.
//
// Not the VariantAutoscaling itself. Those are synthesized per cycle from a
// ScaledObject, never persisted, and deliberately absent from the manager's
// scheme (see scheme.go). Recording against one therefore produced, on every
// optimization cycle:
//
//	"Could not construct reference, will not report event"
//	err="no kind is registered for the type variant.VariantAutoscaling in scheme"
//
// an error log in place of the event, so every condition the events exist to
// report stayed invisible: ScaledUp, ScaledDown, ScaledToZero,
// OptimizationFailed, ResourceConstrained, MetricsUnavailable,
// UnattributedReadyPods and AcceleratorNotResolved alike.
//
// Registering the type would silence the error and still leave the events
// useless, because an event's involvedObject would name something nobody can
// describe. The ScaledObject is the real object behind the synthetic variant --
// it is what caused the variant to exist, it carries the same name and
// namespace, keda.sh/v1alpha1 is in the scheme, and it is where an operator
// looks: `kubectl describe scaledobject` shows these.
//
// It lives here, beside the type and the scheme note that explains the absence,
// because the collector and the engine both need it and two copies would have to
// be kept in step -- the engine already went one release without a fix the
// collector had.
func EventTarget(va *VariantAutoscaling) *kedav1alpha1.ScaledObject {
	return &kedav1alpha1.ScaledObject{
		TypeMeta: metav1.TypeMeta{
			APIVersion: kedav1alpha1.SchemeGroupVersion.String(),
			Kind:       "ScaledObject",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      va.Name,
			Namespace: va.Namespace,
		},
	}
}
