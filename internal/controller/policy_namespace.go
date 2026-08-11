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
// from, and records the decision on cfg. It runs once, before the manager starts,
// because the manager's cache is built from the answer.
//
// It is FAIL-CLOSED for the one shape where the subject of a policy could
// otherwise choose their own: a controller that runs in the namespace it manages.
// There, the tenant owns this Deployment — its args, its env, its ServiceAccount —
// so nothing carried on it can constrain them. Such a controller must find policy
// it did not choose, or be told by a cluster admin that none is required, or it
// refuses to start. "Could not find any policy" never silently becomes "no limits".
//
// Everything authoritative here is a cluster-scoped object, which is what makes it
// out of reach: a namespace admin holds RBAC inside their namespace and cannot
// create a Namespace or edit one's annotations.
//
//  1. the wva.llmd.ai/policy-namespace annotation on the managed Namespace
//  2. the well-known namespace, config.WellKnownPolicyNamespace, if it exists
//  3. the wva.llmd.ai/unbounded=allowed annotation on the managed Namespace
//  4. otherwise: refuse to start
//
// A controller whose namespace is NOT the one it manages — a cluster-scoped
// install, or an admin-run controller pointed at a tenant — is already outside
// the tenant's reach, so it keeps the ordinary behaviour: policy from the
// well-known namespace if present, else from its own configuration.
func ResolvePolicyNamespace(ctx context.Context, c client.Reader, cfg *config.Config) error {
	logger := log.FromContext(ctx)
	systemNS := config.SystemNamespace()
	configured := config.ConfiguredPolicyNamespace()
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
		switch {
		case wellKnownExists:
			cfg.SetPolicyNamespace(config.WellKnownPolicyNamespace, config.PolicySourceWellKnown)
			logger.Info("Cluster policy comes from the well-known namespace", "policyNamespace", config.WellKnownPolicyNamespace)
		case configured != systemNS:
			cfg.SetPolicyNamespace(configured, config.PolicySourceConfigured)
			logger.Info("Cluster policy comes from the configured namespace", "policyNamespace", configured)
		default:
			cfg.SetPolicyNamespace(systemNS, config.PolicySourceLocal)
			logger.V(1).Info("Cluster policy comes from this controller's own namespace", "namespace", systemNS)
		}
		return nil
	}

	// From here on: the tenant could be choosing their own limits, so only
	// admin-owned objects count.
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, client.ObjectKey{Name: managedNS}, ns); err != nil {
		// Including Forbidden. A controller that cannot read the Namespace object
		// cannot see the constraints placed on it, and continuing would mean
		// ignoring an admin's policy annotation purely because we failed to look.
		return fmt.Errorf("this controller manages the namespace it runs in (%s), so it must read that "+
			"Namespace object to learn which policy applies, and cannot: %w\n"+
			"Grant its ServiceAccount get on namespaces, or run the controller outside the namespace it manages",
			managedNS, err)
	}

	if pointed := ns.Annotations[constants.PolicyNamespaceAnnotationKey]; pointed != "" {
		cfg.SetPolicyNamespace(pointed, config.PolicySourceWellKnown)
		logger.Info("Cluster policy comes from the namespace this one is annotated with",
			"policyNamespace", pointed, "annotation", constants.PolicyNamespaceAnnotationKey)
		return nil
	}

	if wellKnownExists {
		cfg.SetPolicyNamespace(config.WellKnownPolicyNamespace, config.PolicySourceWellKnown)
		logger.Info("Cluster policy comes from the well-known namespace", "policyNamespace", config.WellKnownPolicyNamespace)
		return nil
	}

	if ns.Annotations[constants.UnboundedAllowedAnnotationKey] == constants.PolicyUnboundedAllowed {
		cfg.SetPolicyNamespace(systemNS, config.PolicySourceLocal)
		logger.Info("Running WITHOUT a cluster GPU bound: a cluster admin has allowed it for this namespace. "+
			"Scaling here is limited only by each workload's maxReplicas.",
			"namespace", managedNS, "annotation", constants.UnboundedAllowedAnnotationKey)
		return nil
	}

	return fmt.Errorf("refusing to start: this controller manages the namespace it runs in (%s), so it cannot be "+
		"trusted to set its own GPU limits, and no cluster policy applies to it.\n\n"+
		"A cluster admin must do ONE of:\n"+
		"  - create the %s namespace with a scaling-policy ConfigMap, and grant this ServiceAccount read on it\n"+
		"  - annotate this namespace with %s=<namespace holding the policy>\n"+
		"  - annotate this namespace with %s=%s to allow unbounded scaling here\n"+
		"  - or run the controller in a namespace the workloads' owner does not administer",
		managedNS, config.WellKnownPolicyNamespace,
		constants.PolicyNamespaceAnnotationKey,
		constants.UnboundedAllowedAnnotationKey, constants.PolicyUnboundedAllowed)
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
