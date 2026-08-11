#!/usr/bin/env bash
#
# Undeploy and cleanup helpers for deploy/install.sh.
# Requires vars: KEDA_NAMESPACE, MONITORING_NAMESPACE, LLMD_NS, WVA_NS, WVA_PROJECT.
# Requires funcs: containsElement(),
# undeploy_prometheus_stack(), delete_namespaces(), undeploy_epp(), log_*().
#

undeploy_keda() {
    if [ "$ENVIRONMENT" = "openshift" ]; then
        log_info "OpenShift: skipping KEDA uninstall (platform-managed)"
        return
    fi
    if [ "$ENVIRONMENT" = "kubernetes" ] && [ "${KEDA_HELM_INSTALL:-false}" != "true" ]; then
        log_info "Kubernetes: skipping KEDA uninstall (cluster-managed; set KEDA_HELM_INSTALL=true if this script installed KEDA)"
        return
    fi
    log_info "Uninstalling KEDA..."
    helm uninstall "$KEDA_RELEASE_NAME" -n "$KEDA_NAMESPACE" 2>/dev/null || \
        log_warning "KEDA not found or already uninstalled"
    kubectl delete namespace "$KEDA_NAMESPACE" --ignore-not-found --timeout=120s 2>/dev/null || true
    log_success "KEDA uninstalled"
}

undeploy_wva_controller() {
    log_info "Uninstalling Workload-Variant-Autoscaler..."

    local kustomize_overlay
    kustomize_overlay="$(wva_overlay_dir)"

    local tmp_overlay
    tmp_overlay=$(mktemp -d)
    ln -s "$kustomize_overlay" "$tmp_overlay/base"
    cat > "$tmp_overlay/kustomization.yaml" <<EOF
namespace: $WVA_NS
resources:
- ./base
EOF

    # The SAME rename the install applied. Without it, `delete -k` resolves the
    # FIXED binding names and removes whichever install currently owns them — so
    # uninstalling one WVA stripped a different, healthy one of its permissions —
    # while leaking this install's own suffixed bindings. Found by installing and
    # uninstalling side by side on kind.
    wva_append_crb_name_patches "$tmp_overlay/kustomization.yaml" "$WVA_NS"

    kubectl delete -k "$tmp_overlay" --ignore-not-found 2>/dev/null || \
        log_warning "Workload-Variant-Autoscaler resources not found or already removed"
    rm -rf "$tmp_overlay"

    rm -f "$PROM_CA_CERT_PATH"

    log_success "WVA uninstalled"
}

cleanup() {
    log_info "Starting undeployment process..."
    log_info "======================================"
    echo ""

    # Shared infrastructure — Prometheus, KEDA, llm-d's EPP — is left alone unless
    # you ask for it, and this default is not symmetry with the install.
    #
    # It used to key off DEPLOY_PROMETHEUS / SCALER_BACKEND, whose defaults say what
    # a FRESH install would deploy, not what THIS install actually did. So
    # `make undeploy-wva-on-k8s WVA_NS=mine` on a cluster that already had them tore
    # down the cluster's Prometheus, KEDA and EPP — infrastructure the install never
    # touched and other things depend on. Reproduced here: an undeploy of a scratch
    # namespace emptied the monitoring namespace, and every WVA on the cluster lost
    # its metrics backend.
    #
    # Removing WVA is the job. Removing what WVA was pointed AT is a separate
    # decision, and now an explicit one.
    if [ "${UNDEPLOY_SHARED:-false}" = "true" ]; then
        log_warning "UNDEPLOY_SHARED=true: also removing Prometheus, the scaler backend and EPP. Anything else on this cluster using them will lose them."
        if [ "$DEPLOY_PROMETHEUS" = "true" ]; then
            undeploy_prometheus_stack
        fi
        if [ "$SCALER_BACKEND" = "keda" ]; then
            undeploy_keda
        elif [ "$SCALER_BACKEND" = "none" ]; then
            log_info "Skipping scaler backend undeployment (SCALER_BACKEND=none)"
        else
            log_warning "Unsupported SCALER_BACKEND: $SCALER_BACKEND (supported: keda, none); skipping scaler backend undeployment — any installed backend may be left behind"
        fi
        undeploy_epp
    else
        log_info "Leaving Prometheus, the scaler backend and EPP in place — they are shared and this install may not have created them. Pass UNDEPLOY_SHARED=true to remove them too."
    fi

    if [ "$DEPLOY_WVA" = "true" ]; then
        undeploy_wva_controller
    fi

    # Delete namespaces if requested
    if [ "$DELETE_NAMESPACES" = "true" ] || [ "$DELETE_CLUSTER" = "true" ]; then
        delete_namespaces
    else
        log_info "Keeping namespaces (use --delete-namespaces or set DELETE_NAMESPACES=true to remove)"
    fi

    echo ""
    log_success "Undeployment complete!"
    echo ""
    echo "=========================================="
    echo " Undeployment Summary for $ENVIRONMENT"
    echo "=========================================="
    echo ""
    echo "Removed components:"
    [ "$SCALER_BACKEND" = "keda" ] && echo "✓ KEDA"
    [ "$DEPLOY_WVA" = "true" ] && echo "✓ WVA Controller"
    [ "$DEPLOY_PROMETHEUS" = "true" ] && echo "✓ Prometheus Stack"

    if [ "$DELETE_NAMESPACES" = "true" ]; then
        echo "✓ Namespaces"
    else
        echo ""
        echo "Namespaces preserved:"
        echo "  - $LLMD_NS"
        echo "  - $WVA_NS"
        echo "  - $MONITORING_NAMESPACE"
        [ "$SCALER_BACKEND" = "keda" ] && echo "  - $KEDA_NAMESPACE"
    fi
    echo ""
    echo "=========================================="
}
