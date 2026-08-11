#!/usr/bin/env bash
#
# Refuse to install a second WVA on top of an existing one.
#
# Requires vars: WVA_NS, WVA_ALLOW_COEXIST (optional).
# Requires funcs: log_info/log_success/log_warning/log_error, wva_install_scope.
#
# WVA is one per cluster (cluster-scoped) or one per namespace (namespace-scoped).
# A second install is not additive — it TAKES OVER, and quietly:
#
# Every overlay, at either scope, applies the same cluster-scoped RoleBindings
# under fixed names — wva-manager-rolebinding, wva-metrics-auth-rolebinding,
# wva-epp-metrics-reader-role-binding. A ClusterRoleBinding's subject list is
# replaced by an apply, so a second install REPOINTS all three at its own
# namespace. The first controller keeps running with its ServiceAccount silently
# stripped of permissions: no error at install time, no restart, no event — it
# simply starts failing every API call it makes.
#
# That is not a scope-versus-scope problem, which is why this check does not only
# compare scopes. Even a namespace-scoped install into an unrelated namespace does
# it, because those bindings are in the shared base.
#
# Two installs are only sensible when they partition the fleet by
# CONTROLLER_INSTANCE, which needs an overlay of its own (the multi-controller e2e
# has one). WVA_ALLOW_COEXIST=true is for that case: it says you have handled the
# RBAC, and it warns rather than proceeding silently.
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

    if [ "${WVA_ALLOW_COEXIST:-false}" = "true" ]; then
        log_warning "WVA already installed in: $other_ns. Continuing because WVA_ALLOW_COEXIST=true."
        log_warning "  The shared ClusterRoleBindings (wva-manager-rolebinding, wva-metrics-auth-rolebinding, wva-epp-metrics-reader-role-binding) will be REPOINTED at $WVA_NS${owner:+, taking them from $owner}. That install's controller keeps running with no permissions and will not say so. Give each install its own RBAC names and CONTROLLER_INSTANCE, or expect the older one to stop working."
        return 0
    fi

    log_error "WVA is already installed in this cluster: $other_ns (installing into $WVA_NS).

WVA is one per cluster, or one per namespace — never two sharing RBAC. Installing
now would repoint the shared ClusterRoleBindings${owner:+ (currently owned by $owner)}
at $WVA_NS, and the existing controller would keep running with its permissions
silently removed: no error, no restart, no event, just every API call failing.

Pick one:
  - upgrade the existing install:   WVA_NS=${other_ns%%/*} ...
  - remove it first:                make undeploy-wva-on-k8s WVA_NS=${other_ns%%/*}
  - or, if you are deliberately partitioning the fleet by CONTROLLER_INSTANCE and
    have given each install its own RBAC names: WVA_ALLOW_COEXIST=true"
}
