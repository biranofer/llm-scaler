#!/usr/bin/env bash
#
# Refuse to install a second WVA on top of an existing one.
#
# Requires vars: WVA_NS, WVA_ALLOW_COEXIST (optional).
# Requires funcs: log_info/log_success/log_warning/log_error, wva_install_scope.
#
# WVA is one per cluster (cluster-scoped) or one per namespace (namespace-scoped).
#
# Note what the danger is NOT. A workload registers with the WVA whose
# scalerAddress its trigger names, and each install has its own
# wva-external-scaler.<ns> service, so two controllers never see each other's
# workloads. Discovery is already partitioned.
#
# The danger is the GPU budget, which is not. Both controllers observe the same
# cluster, compute free capacity from the same nodes, and allocate against it
# without seeing the other's claim — so the same free GPUs are handed out twice.
# Each install is individually correct and the cluster is oversubscribed, which
# surfaces as pods that will not schedule rather than as an error from either.
#
# That is also why partitioning WORKLOADS does not help: disjoint fleets still draw
# on one pool of GPUs. Only one controller can hold a coherent budget.
#
# One collision is already fixed rather than merely refused: the cluster-scoped
# RoleBindings are applied under fixed names, and an apply REPLACES a
# ClusterRoleBinding's subject list, so a second install used to silently strip the
# first controller's permissions — no error, no restart, no event.
# deploy_wva_controller suffixes those names per namespace now (it did this for
# OpenShift only, on the theory that shared clusters were an OpenShift problem;
# they are not, and it reproduces on kind).
#

# wva_installations echoes "namespace name" for every WVA controller Deployment in
# the cluster.
wva_installations() {
    kubectl get deployments -A -l app.kubernetes.io/name=workload-variant-autoscaler \
        -o go-template='{{range .items}}{{.metadata.namespace}} {{.metadata.name}}{{"\n"}}{{end}}' 2>/dev/null
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

    if [ "${WVA_ALLOW_COEXIST:-false}" = "true" ]; then
        log_warning "WVA already installed in: $other_ns. Continuing because WVA_ALLOW_COEXIST=true."
        log_warning "  Both controllers will allocate from the same pool of free GPUs without seeing each other's claims, so the cluster can be oversubscribed — which shows up as pods that will not schedule, not as an error from either controller."
        return 0
    fi

    log_error "WVA is already installed in this cluster: $other_ns (installing into $WVA_NS).

WVA is one per cluster, or one per namespace. Their WORKLOADS would be separate —
a workload registers with the scaler address its trigger names — but their GPU
BUDGETS would not: both observe the same nodes and allocate the same free GPUs
without seeing the other's claim. The cluster ends up oversubscribed, and it
surfaces as pods that will not schedule rather than as an error from either.

Pick one:
  - upgrade the existing install:   WVA_NS=${other_ns%%/*} ...
  - remove it first:                make undeploy-wva-on-k8s WVA_NS=${other_ns%%/*}
  - or, if you are deliberately running two (a WVA upgrade side by side, say) and
    accept that both will decide for every workload that calls either of them:
    WVA_ALLOW_COEXIST=true"
}
