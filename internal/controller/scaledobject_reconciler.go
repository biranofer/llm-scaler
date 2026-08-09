/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
)

// ScaledObjectReconciler tracks namespaces for annotation-based WVA discovery via KEDA
// ScaledObject objects. Its sole job is to call NamespaceTrack / NamespaceUntrack so
// the engine's polling loop can scope its List calls to namespaces that contain managed
// scalers. Registered only when the KEDA CRD is detected at startup.
type ScaledObjectReconciler struct {
	client.Client
	Datastore datastore.Datastore
}

// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get;list;watch
// Note: Kubernetes RBAC does not support annotation-based selectors, so this grant is
// necessarily cluster-wide. Access is read-only (get;list;watch). AnnotatedScalerPredicate
// limits event processing to managed objects (see TODO #1134 for scoped List calls).

// Reconcile tracks or untracks the namespace of a ScaledObject bearing llm-d.ai/managed: "true".
func (r *ScaledObjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	so := &kedav1alpha1.ScaledObject{}
	if err := r.Get(ctx, req.NamespacedName, so); err != nil {
		if apierrors.IsNotFound(err) {
			r.Datastore.NamespaceUntrack("Scaler", req.Name, req.Namespace)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Every ScaledObject's namespace is tracked, not just annotated ones. WVA no
	// longer knows from the object whether it is WVA's — that is decided by KEDA
	// calling the external scaler, which this reconciler cannot see. Tracking is
	// only used to scope ConfigMap loading, so the cost of over-tracking is
	// watching config in a namespace WVA turns out not to manage, while
	// under-tracking would silently drop a managed workload's configuration.
	if !so.DeletionTimestamp.IsZero() {
		r.Datastore.NamespaceUntrack("Scaler", req.Name, req.Namespace)
	} else {
		r.Datastore.NamespaceTrack("Scaler", req.Name, req.Namespace)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers ScaledObjectReconciler with the controller manager.
func (r *ScaledObjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kedav1alpha1.ScaledObject{}).
		Named("scaledobject").
		Complete(r)
}
