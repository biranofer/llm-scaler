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

package indexers

import (
	"context"
	"fmt"
	"strings"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var _ = Describe("Indexers", Ordered, func() {
	var (
		testCtx   context.Context
		cancel    context.CancelFunc
		namespace string
		mgr       manager.Manager
		mgrClient client.Client
	)

	BeforeAll(func() {
		testCtx, cancel = context.WithCancel(context.Background()) //nolint:fatcontext // shared across BeforeAll/AfterAll
		namespace = fmt.Sprintf("test-indexers-%d", GinkgoRandomSeed())

		// Create the test namespace
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(k8sClient.Create(testCtx, ns)).To(Succeed())

		// Create a manager with indexes for testing
		var err error
		mgr, err = manager.New(cfg, manager.Options{
			Metrics: metricsserver.Options{BindAddress: "0"}, // disable metrics server in tests
		})
		Expect(err).NotTo(HaveOccurred())

		// Register all indexes for the main indexer suite; focused gating
		// coverage below verifies the KEDA-disabled startup path.
		err = SetupIndexes(testCtx, mgr, true)
		Expect(err).NotTo(HaveOccurred())

		// Start the manager's cache
		go func() {
			defer GinkgoRecover()
			_ = mgr.Start(testCtx)
		}()

		// Wait for cache to sync
		Expect(mgr.GetCache().WaitForCacheSync(testCtx)).To(BeTrue())
		mgrClient = mgr.GetClient()
	})

	AfterAll(func() {
		// Cancel the context to stop the manager goroutine
		cancel()

		// Clean up the namespace
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(client.IgnoreNotFound(k8sClient.Delete(context.Background(), ns))).To(Succeed())
	})

	Describe("SetupIndexes", func() {
		It("should register the scale target indexes successfully", func() {
			// The indexes are set up in the BeforeAll
			// If we got here without error, the indexes were registered successfully
			Expect(mgr).NotTo(BeNil())
		})

		It("registers nothing when KEDA is absent", func() {
			// Simulates a cluster without KEDA. There is no longer a second index to
			// fall back on: indexing a kind starts a cluster-wide LIST+WATCH informer
			// for it, so the HPA index went with the HPA discovery path. Setup must
			// still succeed — WVA runs, it simply discovers nothing until KEDA calls.
			ctx, cancelDisabledMgr := context.WithCancel(context.Background())
			defer cancelDisabledMgr()

			disabledMgr, err := manager.New(cfg, manager.Options{
				Metrics: metricsserver.Options{BindAddress: "0"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(SetupIndexes(ctx, disabledMgr, false)).To(Succeed())
		})
	})

	Describe("ScaledObject index", func() {
		It("returns the ScaledObject for its Deployment scaleTargetRef", func() {
			ns := namespace + "-so-1"
			Expect(k8sClient.Create(testCtx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
			defer func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))).To(Succeed())
			}()

			so := &kedav1alpha1.ScaledObject{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "wva-so",
					Namespace: ns,
				},
				Spec: kedav1alpha1.ScaledObjectSpec{
					ScaleTargetRef: &kedav1alpha1.ScaleTarget{
						APIVersion: "apps/v1", Kind: "Deployment", Name: "so-deploy",
					},
					// Required by the KEDA CRD's OpenAPI schema; envtest rejects creates without it.
					Triggers: []kedav1alpha1.ScaleTriggers{
						{Type: "prometheus", Metadata: map[string]string{"serverAddress": "http://prometheus:9090", "query": "up", "threshold": "1"}},
					},
				},
			}
			if err := k8sClient.Create(testCtx, so); err != nil {
				if strings.Contains(err.Error(), "no kind") || strings.Contains(err.Error(), "no matches for kind") {
					Skip("KEDA CRDs not available in this envtest setup")
				}
				Expect(err).ToNot(HaveOccurred())
			}
			defer func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, so))).To(Succeed())
			}()

			ref := autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "so-deploy",
			}
			Eventually(func() error {
				return mgrClient.Get(testCtx, client.ObjectKeyFromObject(so), &kedav1alpha1.ScaledObject{})
			}).Should(Succeed())

			got, err := FindSOForScaleTarget(testCtx, mgrClient, ref, ns)
			Expect(err).ToNot(HaveOccurred())
			Expect(got).ToNot(BeNil())
			Expect(got.Name).To(Equal("wva-so"))
		})

		It("indexes a ScaledObject with no WVA configuration at all", func() {
			// The index no longer filters on an annotation. Whether a workload is
			// WVA's is decided by KEDA calling the external scaler, which an index
			// function cannot observe, so filtering here would drop exactly the
			// workloads configured the current way. Being indexed is not a claim of
			// management — it only says "this is the scaler for that workload",
			// which is true regardless of who manages it.
			ns := namespace + "-so-2"
			Expect(k8sClient.Create(testCtx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
			defer func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))).To(Succeed())
			}()

			so := &kedav1alpha1.ScaledObject{
				ObjectMeta: metav1.ObjectMeta{Name: "unconfigured-so", Namespace: ns},
				Spec: kedav1alpha1.ScaledObjectSpec{
					ScaleTargetRef: &kedav1alpha1.ScaleTarget{
						APIVersion: "apps/v1", Kind: "Deployment", Name: "so-deploy-2",
					},
					// Required by the KEDA CRD's OpenAPI schema; envtest rejects creates without it.
					Triggers: []kedav1alpha1.ScaleTriggers{
						{Type: "prometheus", Metadata: map[string]string{"serverAddress": "http://prometheus:9090", "query": "up", "threshold": "1"}},
					},
				},
			}
			if err := k8sClient.Create(testCtx, so); err != nil {
				if strings.Contains(err.Error(), "no kind") || strings.Contains(err.Error(), "no matches for kind") {
					Skip("KEDA CRDs not available in this envtest setup")
				}
				Expect(err).ToNot(HaveOccurred())
			}
			defer func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(testCtx, so))).To(Succeed())
			}()

			ref := autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "so-deploy-2",
			}
			Eventually(func() error {
				return mgrClient.Get(testCtx, client.ObjectKeyFromObject(so), &kedav1alpha1.ScaledObject{})
			}).Should(Succeed())

			got, err := FindSOForScaleTarget(testCtx, mgrClient, ref, ns)
			Expect(err).ToNot(HaveOccurred())
			Expect(got).ToNot(BeNil(), "the scaler for a workload is its scaler regardless of annotations")
			Expect(got.Name).To(Equal("unconfigured-so"))
		})
	})
})
