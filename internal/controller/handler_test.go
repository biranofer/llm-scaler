/*
Copyright 2025 The llm-d Authors

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
	"testing"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
)

func scalerTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add clientgoscheme: %v", err)
	}
	if err := kedav1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add kedav1alpha1: %v", err)
	}
	return s
}

// --- HPAReconciler tests ---

// --- ScaledObjectReconciler tests ---

func TestScaledObjectReconciler_TracksNamespace(t *testing.T) {
	s := scalerTestScheme(t)
	so := &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "so-a",
			Namespace: "ns1",
		},
	}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(so).Build()
	ds := datastore.NewDatastore(config.NewTestConfig())

	r := &ScaledObjectReconciler{Client: cl, Datastore: ds}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "so-a", Namespace: "ns1"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ds.IsNamespaceTracked("ns1") {
		t.Error("want ns1 tracked after a ScaledObject reconcile")
	}
}

func TestScaledObjectReconciler_UntracksOnNotFound(t *testing.T) {
	s := scalerTestScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	ds := datastore.NewDatastore(config.NewTestConfig())
	ds.NamespaceTrack("Scaler", "so-a", "ns1")

	r := &ScaledObjectReconciler{Client: cl, Datastore: ds}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "so-a", Namespace: "ns1"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ds.IsNamespaceTracked("ns1") {
		t.Error("want ns1 untracked when ScaledObject is not found (deleted)")
	}
}

// TestScaledObjectReconciler_UntracksOnDeletion: tracking now follows the
// object's existence rather than an annotation on it. WVA cannot tell from a
// ScaledObject whether it is WVA's — that is settled by KEDA calling the external
// scaler — so it tracks every namespace that has one, and stops when the object
// is being deleted.
func TestScaledObjectReconciler_UntracksOnDeletion(t *testing.T) {
	s := scalerTestScheme(t)
	now := metav1.Now()
	so := &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "so-a",
			Namespace:         "ns1",
			Finalizers:        []string{"test"},
			DeletionTimestamp: &now,
		},
	}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(so).Build()
	ds := datastore.NewDatastore(config.NewTestConfig())
	ds.NamespaceTrack("Scaler", "so-a", "ns1")

	r := &ScaledObjectReconciler{Client: cl, Datastore: ds}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "so-a", Namespace: "ns1"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ds.IsNamespaceTracked("ns1") {
		t.Error("want ns1 untracked once the ScaledObject is being deleted")
	}
}
