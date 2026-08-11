#!/usr/bin/env bash
#
# Refuse to install a second WVA on top of an existing one.
#
# Requires vars: WVA_NS, WVA_ALLOW_COEXIST (optional).
# Requires funcs: log_info/log_success/log_warning/log_error, wva_install_scope.
#
# WVA is one per cluster (cluster-scoped) or one per namespace (namespace-scoped).
# A second, unpartitioned install is not additive — both controllers then manage
# the same workloads.
#
# What that costs: every unlabelled workload is optimized by both, and both publish
# a decision for the same ScaledObject. Whichever wrote last wins, so the replica
# count is whichever controller happened to finish second, and no decision can be
# attributed to either. Nothing errors; the fleet simply scales
# non-deterministically.
#
# It used to cost more. The cluster-scoped RoleBindings are applied under fixed
# names, and an apply REPLACES a ClusterRoleBinding's subject list, so a second
# install silently repointed them and left the first controller's ServiceAccount
# with no permissions at all — no error, no restart, no event. deploy_wva_controller
# now suffixes those names per namespace (it did this for OpenShift only, on the
# theory that shared clusters were an OpenShift problem; they are not, and it
# reproduces on kind), so that failure is gone and what remains is the duplicate
# management above.
#
# CONTROLLER_INSTANCE is the supported way to run more than one: each instance
# manages only the workloads whose ScaledObject carries its name, so the fleets are
# disjoint by construction and this check lets it through.
#

# wva_installations echoes "namespace name" for every WVA controller Deployment in
# the cluster.
wva_installations() {
    kubectl get deployments -A -l app.kubernetes.io/name=workload-variant-autoscaler \
        -o go-template='{{range .items}}{{.metadata.namespace}} {{.metadata.name}}{{"\n"}}{{end}}' 2>/dev/null
}

# wva_binding_owner echoes the namespace a shared ClusterRoleBinding currently
# points at — i.e. which install owns the cluster-scoped RBAC right now.
wva_binding_owner() {
    kubectl get clusterrolebinding wva-manager-rolebinding \
        -o go-template='{{range .subjects}}{{.namespace}}{{end}}' 2>/dev/null
}

check_single_installation() {
    local found=0 other_ns="" ns name
    while read -r ns name; do
        [ -n "$ns" ] || continue
        # Our own namespace is an upgrade, not a collision.
        if [ "$ns" = "$WVA_NS" ]; then
            log_info "Existing WVA in $WVA_NS — this install upgrades it in place."
            continue
        fi
        found=$((found + 1))
        other_ns="${other_ns:+$other_ns, }$ns/$name"
    done < <(wva_installations)

    [ "$found" -gt 0 ] || return 0

    local owner
    owner=$(wva_binding_owner)

    # A named instance is the supported way to run several controllers: each
    # manages only the workloads whose ScaledObject carries its name, and each
    # install now gets its own ClusterRoleBinding names, so they do not take RBAC
    # from one another. That combination is a deliberate topology, not the accident
    # this check exists to stop.
    if [ -n "${CONTROLLER_INSTANCE:-}" ]; then
        log_info "WVA already installed in: $other_ns. Continuing: CONTROLLER_INSTANCE=$CONTROLLER_INSTANCE partitions the fleet, and each install has its own ClusterRoleBinding names."
        log_info "  This instance manages ONLY workloads whose ScaledObject carries wva.llmd.ai/controller-instance=$CONTROLLER_INSTANCE. Anything unlabelled stays with an instance-less install, and anything labelled for another instance is invisible here."
        return 0
    fi

    if [ "${WVA_ALLOW_COEXIST:-false}" = "true" ]; then
        log_warning "WVA already installed in: $other_ns. Continuing because WVA_ALLOW_COEXIST=true."
        log_warning "  Both controllers will manage every unlabelled workload, and both will publish a decision for the same ScaledObject — the replica count becomes whichever one wrote last, and no decision is attributable. Use CONTROLLER_INSTANCE to give each a disjoint fleet instead."
        return 0
    fi

    log_error "WVA is already installed in this cluster: $other_ns (installing into $WVA_NS).

WVA is one per cluster, or one per namespace. Two unpartitioned controllers both
manage every unlabelled workload and both publish a decision for the same
ScaledObject${owner:+ (cluster RBAC currently owned by $owner)}: the replica count
becomes whichever wrote last, and no decision can be attributed to either. Nothing
errors — the fleet just scales non-deterministically.

Pick one:
  - upgrade the existing install:   WVA_NS=${other_ns%%/*} ...
  - remove it first:                make undeploy-wva-on-k8s WVA_NS=${other_ns%%/*}
  - partition the fleet properly:    CONTROLLER_INSTANCE=<name> ...
    Each instance manages only the workloads whose ScaledObject carries
    wva.llmd.ai/controller-instance=<name>, and each install gets its own
    ClusterRoleBinding names. This is the supported way to run more than one.
  - or, if you know what you are doing and want neither: WVA_ALLOW_COEXIST=true"
}
