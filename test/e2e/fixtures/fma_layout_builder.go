package fixtures

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

// Fast Model Actuation layout, built without FMA.
//
// FMA splits a model server across two pods: a REQUESTER Deployment carrying the
// llm-d identity, which a scaler moves, and LAUNCHER pods owned by a
// LauncherConfig, which hold the GPU and run the engine. The ownerReferences gap
// is deliberate — FMA patches the provider's labels so no ReplicaSet adopts it —
// and it is why the collector's owner walk ends at nothing and falls back to the
// pairing declared on the pods.
//
// Reproducing that here needs no FMA controller and no CRDs, because the labels
// ARE the contract:
//
//   - a launcher pod's controller reference names Kind "LauncherConfig", a kind
//     the walk cannot follow. The object need not exist: walkOwnersUp stops at an
//     unknown kind before it tries to fetch it, returning the chain so far and no
//     error. That is exactly the production path.
//   - both halves carry dual-pods.llm-d.ai/dual naming each other, which is what
//     FMA's dual-pods controller maintains on a bound pair.
//
// A launcher is a plain pod, not a Deployment, which also matches production: the
// populator creates them directly.
const (
	fmaLauncherConfigAPIVersion = "fma.llm-d.ai/v1alpha1"
	fmaLauncherConfigKind       = "LauncherConfig"
)

// FMALayout names the objects a CreateFMALayout call produced, so a spec can
// assert against them without rebuilding the naming rules.
type FMALayout struct {
	// RequesterDeployment is the scale target — the half a ScaledObject points at.
	RequesterDeployment string
	// RequesterPod is the pod the launcher pairs with.
	RequesterPod string
	// LauncherPod holds the engine and reports the metrics.
	LauncherPod string
	// UnboundLauncherPod is a warm spare: a launcher with no pairing label. It
	// must NOT be attributed, and is included so a spec can prove that.
	UnboundLauncherPod string
}

// CreateFMALayout builds a bound FMA pair plus one unbound spare in namespace.
//
// modelLabel is the sanitized DNS-safe model label (llm-d.ai/model), not the
// model ID — the two are different strings and the collector compares this one
// only against another pod's copy of it.
//
// The requester Deployment is created with replicas=0 and its pod is created
// directly, so the pair can be wired to each other by name: a Deployment-managed
// pod gets a generated name that is not known until after it exists, and the
// pairing label has to name it.
func CreateFMALayout(ctx context.Context, k8sClient *kubernetes.Clientset, namespace, name, modelLabel string) (*FMALayout, error) {
	l := &FMALayout{
		RequesterDeployment: name,
		RequesterPod:        name + "-pod",
		LauncherPod:         "launcher-" + name,
		UnboundLauncherPod:  "launcher-" + name + "-spare",
	}

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      l.RequesterDeployment,
			Namespace: namespace,
			Labels:    map[string]string{"test-resource": defaultTestResourceLabelValue},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(0)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": l.RequesterDeployment}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					"app":                        l.RequesterDeployment,
					"llm-d.ai/role":              "requester",
					constants.ModelLabelKey:      modelLabel,
					"llm-d.ai/inference-serving": defaultLabelValueTrue,
				}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "inference-server", Image: defaultModelServiceSimulatorImage,
				}}},
			},
		},
	}
	if _, err := k8sClient.AppsV1().Deployments(namespace).Create(ctx, deploy, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create requester deployment: %w", err)
	}

	// The requester pod, owned by the Deployment via a ReplicaSet stand-in. A
	// direct Deployment ownerReference is enough for the walk: it looks for the
	// first ancestor of a scale-target kind, and a Deployment is one.
	created, err := k8sClient.AppsV1().Deployments(namespace).Get(ctx, l.RequesterDeployment, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read back requester deployment: %w", err)
	}
	requester := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      l.RequesterPod,
			Namespace: namespace,
			Labels: map[string]string{
				"llm-d.ai/role":                "requester",
				constants.ModelLabelKey:        modelLabel,
				constants.DualPodsPairLabelKey: l.LauncherPod,
				"test-resource":                defaultTestResourceLabelValue,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment",
				Name: created.Name, UID: created.UID, Controller: ptr.To(true),
			}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "inference-server", Image: defaultModelServiceSimulatorImage,
		}}},
	}
	if _, err := k8sClient.CoreV1().Pods(namespace).Create(ctx, requester, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create requester pod: %w", err)
	}

	bound := fmaLauncherPod(namespace, l.LauncherPod, modelLabel, l.RequesterPod)
	if _, err := k8sClient.CoreV1().Pods(namespace).Create(ctx, bound, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create bound launcher: %w", err)
	}

	spare := fmaLauncherPod(namespace, l.UnboundLauncherPod, modelLabel, "")
	if _, err := k8sClient.CoreV1().Pods(namespace).Create(ctx, spare, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create unbound launcher: %w", err)
	}

	return l, nil
}

// fmaLauncherPod builds a server-providing pod. partner empty means unbound —
// FMA removes the pairing label when a pair breaks, and that absence is how a
// warm spare declines to be counted as capacity.
func fmaLauncherPod(namespace, name, modelLabel, partner string) *corev1.Pod {
	labels := map[string]string{
		constants.ComponentLabelKey: constants.LauncherComponent,
		"test-resource":             defaultTestResourceLabelValue,
	}
	if partner != "" {
		// FMA stamps the serving labels onto a launcher only at bind time, which
		// is what puts it in the InferencePool. An unbound spare has neither
		// these nor the pairing label.
		labels[constants.DualPodsPairLabelKey] = partner
		labels[constants.ModelLabelKey] = modelLabel
		labels["llm-d.ai/inferenceServing"] = defaultLabelValueTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: fmaLauncherConfigAPIVersion,
				Kind:       fmaLauncherConfigKind,
				Name:       "fma-" + name,
				UID:        "00000000-0000-0000-0000-0000000000fa",
				Controller: ptr.To(true),
			}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "inference-server", Image: defaultModelServiceSimulatorImage,
		}}},
	}
}

// DeleteFMALayout removes everything CreateFMALayout made. Missing objects are
// not an error, so a spec can call it unconditionally in a cleanup step.
func DeleteFMALayout(ctx context.Context, k8sClient *kubernetes.Clientset, namespace string, l *FMALayout) error {
	if l == nil {
		return nil
	}
	for _, pod := range []string{l.RequesterPod, l.LauncherPod, l.UnboundLauncherPod} {
		if err := k8sClient.CoreV1().Pods(namespace).Delete(ctx, pod, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete pod %s: %w", pod, err)
		}
	}
	if err := k8sClient.AppsV1().Deployments(namespace).Delete(ctx, l.RequesterDeployment, metav1.DeleteOptions{}); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete deployment %s: %w", l.RequesterDeployment, err)
	}
	return nil
}
