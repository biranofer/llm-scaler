package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
)

// ResolvePolicyNamespace decides where this controller reads limiters and quotas
// from, and records the decision on cfg. It runs once, before the manager starts.
//
// Order:
//
//  1. the wva.llmd.ai/policy-namespace LABEL on the managed Namespace
//  2. the well-known namespace, config.WellKnownPolicyNamespace — self-managed only
//  3. the controller's own namespace
//
// Reading step 1 needs `get namespaces`, which a namespace-scoped install does not
// have. That read is best-effort and a denial is logged, not fatal — but it means
// an admin who labels a tenant namespace must also grant that read, or the label
// is silently not seen and the tenant keeps its own configuration.
//
// Rule 1 is an annotation on a NAMESPACE because that object is cluster-scoped: a
// namespace admin holds RBAC inside their namespace and cannot edit it. So an
// admin can direct a controller they did not deploy.
//
// # What this is and is not
//
// This is a GUARDRAIL, not an enforcement boundary, and the distinction is worth
// being blunt about because an earlier version of this function got it wrong.
//
// That version refused to start when a controller managed its own namespace and
// found no policy — reasoning that such a controller is owned by the tenant it is
// supposed to bound, so it must not set its own limits. Both halves of that were
// wrong in practice:
//
//   - it did not work. `selfManaged` is derived from --watch-namespace, an
//     argument on the Deployment the tenant owns. Deleting one flag made the
//     controller look admin-owned and skipped the check entirely. A gate whose
//     trigger is self-reported by its subject is not a gate.
//   - it broke everyone else. Namespace scope is the DEFAULT on OpenShift, so
//     every such install — with no limiter configured and nothing to enforce —
//     would have refused to start until someone created a namespace they had
//     never heard of.
//
// Maximum breakage, zero containment. A tenant who owns the controller's
// Deployment owns its image, so no in-process rule can bind them; a check that
// pretends otherwise mainly misleads the admin relying on it.
//
// Enforcement against a tenant you do not trust belongs at admission, where it
// cannot be argued with: a ResourceQuota on the GPU resource in their namespace.
// What WVA can honestly offer is that the budget is authoritative for every
// controller actually running WVA — which is misconfiguration, drift and copied
// manifests, the things that occur far more often than malice — and, for the
// arrangement where the tenant does NOT own the controller (it runs in an admin
// namespace with WVA_WATCH_NS pointed at theirs), a bound that genuinely holds.
func ResolvePolicyNamespace(ctx context.Context, c client.Reader, cfg *config.Config) error {
	logger := log.FromContext(ctx)
	systemNS := config.SystemNamespace()
	managedNS := cfg.WatchNamespace()

	// The tenant-owned shape: this controller manages exactly the namespace it
	// lives in, so whoever administers that namespace administers this controller.
	selfManaged := managedNS != "" && managedNS == systemNS

	wellKnownExists, err := namespaceExists(ctx, c, config.WellKnownPolicyNamespace)
	if err != nil {
		// Never guess. Falling back on an API error would quietly downgrade a
		// cluster-imposed bound to a self-chosen one, and nothing afterwards would
		// show that this controller came up under the weaker rule.
		return fmt.Errorf("cannot determine whether the %s namespace exists, refusing to guess which policy applies: %w",
			config.WellKnownPolicyNamespace, err)
	}

	if !selfManaged {
		// An admin-owned controller keeps reading its OWN ConfigMap unless it was
		// explicitly told otherwise. The well-known namespace deliberately does NOT
		// win here.
		//
		// Letting it win looked tidier and silently broke working installs: a
		// cluster-scoped controller, correctly bounded by limiters in its own
		// admin-owned ConfigMap, would switch sources the moment anyone created
		// wva-policy for an unrelated tenant. If that namespace's ConfigMap carried
		// a default entry with no limiters — the normal way to say "I have not
		// configured this yet" — the switch would take the cluster from bounded to
		// UNBOUNDED, with no edit to the install that changed.
		//
		// The override only earns its keep where the reader might otherwise choose
		// its own limits, which is the branch below.
		cfg.SetPolicyNamespace(systemNS, config.PolicySourceLocal)
		logger.V(1).Info("Cluster policy comes from this controller's own namespace", "namespace", systemNS)
		if wellKnownExists {
			logger.Info("A "+config.WellKnownPolicyNamespace+" namespace exists but is NOT used here: this controller does not manage its own namespace, so its own ConfigMap is authoritative.",
				"policyNamespace", config.WellKnownPolicyNamespace)
		}
		return nil
	}

	// Self-managed: the namespace this controller runs in is the one it manages, so
	// an admin may have annotated that Namespace to direct it. Reading it is
	// best-effort — a controller with no namespace read is an ordinary
	// namespace-scoped deployment, and refusing to start over it would break the
	// default shape on OpenShift for no gain.
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, client.ObjectKey{Name: managedNS}, ns); err != nil {
		if !apierrors.IsNotFound(err) && !apierrors.IsForbidden(err) {
			return fmt.Errorf("failed reading namespace %s to resolve cluster policy: %w", managedNS, err)
		}
		// Info, not V(1): this is the difference between "no policy applies" and
		// "policy may apply and I cannot see it", and an admin who labelled this
		// namespace has no other way to find out the label was never read.
		logger.Info("Could not read the managed Namespace, so any wva.llmd.ai/policy-namespace label on it "+
			"has NOT been applied. Grant this ServiceAccount get on namespaces if a cluster admin "+
			"directs policy that way.",
			"namespace", managedNS, "reason", err.Error())
	}

	// Label first, then annotation. The label is the selectable form — an admin can
	// ask `kubectl get ns -l wva.llmd.ai/policy-namespace=X` which namespaces a
	// policy governs — so it is the one to prefer when both are somehow present.
	if pointed := ns.Labels[constants.PolicyNamespaceLabelKey]; pointed != "" {
		cfg.SetPolicyNamespace(pointed, config.PolicySourceWellKnown)
		logger.Info("Cluster policy comes from the namespace this one is labelled with",
			"policyNamespace", pointed, "label", constants.PolicyNamespaceLabelKey)
		return nil
	}

	if wellKnownExists {
		cfg.SetPolicyNamespace(config.WellKnownPolicyNamespace, config.PolicySourceWellKnown)
		logger.Info("Cluster policy comes from the well-known namespace", "policyNamespace", config.WellKnownPolicyNamespace)
		return nil
	}

	cfg.SetPolicyNamespace(systemNS, config.PolicySourceLocal)
	logger.V(1).Info("Cluster policy comes from this controller's own namespace", "namespace", systemNS)
	return nil
}

// namespaceExists reports whether a namespace is present.
//
// Forbidden is reported as absent, and only here: this answer is used to FIND
// policy, never to decide whether policy is required. A controller that cannot see
// the well-known namespace still has to satisfy one of the other branches, so
// treating it as missing narrows what it may do rather than widening it.
func namespaceExists(ctx context.Context, c client.Reader, name string) (bool, error) {
	ns := &corev1.Namespace{}
	err := c.Get(ctx, client.ObjectKey{Name: name}, ns)
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err), apierrors.IsForbidden(err):
		return false, nil
	default:
		return false, err
	}
}
