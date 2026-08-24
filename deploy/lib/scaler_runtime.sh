#!/usr/bin/env bash
#
# Scaler backend deployment/runtime helpers for deploy/install.sh.
# Requires vars: MONITORING_NAMESPACE, KEDA_NAMESPACE, KEDA_CHART_VERSION.
# Requires funcs: log_info/log_warning/log_success/log_error,
# should_skip_helm_repo_update().
#

deploy_keda() {
    log_info "Deploying KEDA (scaler backend)..."
    # Deploy flow is non-interactive: missing/failed scaler backend is a hard failure.

    # OpenShift: KEDA is cluster-managed (OLM/operator); never Helm-install — avoids
    # ClusterRole/release conflicts with an existing platform KEDA.
    if [ "$ENVIRONMENT" = "openshift" ]; then
        log_info "OpenShift: assuming platform-managed KEDA — skipping Helm install"
        if kubectl get crd scaledobjects.keda.sh >/dev/null 2>&1; then
            log_success "KEDA ScaledObject CRD is available on the cluster"
        else
            log_error "OpenShift: scaledobjects.keda.sh CRD not found — install cluster KEDA before E2E (SCALER_BACKEND=keda)"
        fi
        return
    fi

    # Kubernetes (e.g. CKS, shared clusters): assume cluster-managed KEDA; never Helm unless opted in.
    if [ "$ENVIRONMENT" = "kubernetes" ] && [ "${KEDA_HELM_INSTALL:-false}" != "true" ]; then
        log_info "Kubernetes: assuming cluster-managed KEDA — skipping Helm (set KEDA_HELM_INSTALL=true to install via Helm)"
        # Distinguish "absent" from "not allowed to look". A namespace admin cannot
        # read cluster-scoped CRDs, and treating that Forbidden as NotFound aborted
        # their install — for a KEDA that was present all along, and only AFTER the
        # controller had been applied, leaving it half-installed.
        # || keda_rc=$?, not ; keda_rc=$?. The plain form aborts the whole
        # install at this assignment the moment the CRD is absent, which is the
        # ONE case the message below exists to explain -- so the operator got
        # a bare exit 2 and a half-applied prerequisite set instead of being
        # told to set KEDA_HELM_INSTALL=true.
        local keda_probe keda_rc=0
        keda_probe=$(kubectl get crd scaledobjects.keda.sh 2>&1) || keda_rc=$?
        if [ $keda_rc -eq 0 ]; then
            log_success "KEDA ScaledObject CRD is available on the cluster"
        elif printf '%s' "$keda_probe" | grep -qi 'forbidden'; then
            log_warning "Cannot check for the KEDA CRD (reading cluster-scoped CRDs is not permitted for you), so this install assumes KEDA is present."
            log_warning "  If it is not, ScaledObjects will stay unready and nothing will scale. Confirm with whoever administers the cluster."
        else
            log_error "Kubernetes: scaledobjects.keda.sh CRD not found — install KEDA on the cluster or set KEDA_HELM_INSTALL=true"
        fi
        return
    fi

    # Skip install if KEDA is already fully operational on the cluster.
    # Check CRD + operator pods + external metrics APIService to avoid false positives
    # from stale CRDs left behind after a prior uninstall.
    if kubectl get crd scaledobjects.keda.sh >/dev/null 2>&1; then
        # Readiness, not `grep -q Running`. This decides whether to install KEDA
        # at all, so a "0/1 Running" operator answering yes here skips the install
        # that would have fixed it and leaves the cluster with a KEDA that never
        # reconciles anything.
        if wva_any_pod_ready -A "$KEDA_OPERATOR_LABEL_SELECTOR"; then
            if kubectl get apiservice "$EXTERNAL_METRICS_APISERVICE_NAME" >/dev/null 2>&1; then
                log_success "KEDA CRD, operator, and metrics APIService detected — skipping helm install"
                return
            fi
        fi
        # Shared clusters (e.g. CKS) often pre-install KEDA without the exact pod label / APIService
        # shape our probe expects, but ClusterRole keda-operator already exists without Helm metadata.
        # Helm install then fails with ownership errors — skip Helm when that pattern is present.
        if kubectl get clusterrole keda-operator >/dev/null 2>&1; then
            keda_cr_managed_by=$(kubectl get clusterrole keda-operator -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}' 2>/dev/null || true)
            if [ "$keda_cr_managed_by" != "Helm" ]; then
                log_info "KEDA CRD present and ClusterRole keda-operator is not Helm-managed — skipping Helm install (pre-installed KEDA)"
                return
            fi
        fi
        log_warning "KEDA ScaledObject CRD found but operator or metrics APIService not detected; proceeding with helm install"
    fi

    kubectl create namespace "$KEDA_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

    helm repo add kedacore https://kedacore.github.io/charts 2>/dev/null || true
    if [ "$(should_skip_helm_repo_update)" = "true" ]; then
        log_info "Skipping helm repo update for KEDA (SKIP_HELM_REPO_UPDATE=true)"
    else
        helm repo update
    fi

    if ! helm upgrade -i "$KEDA_RELEASE_NAME" kedacore/keda \
        --version "$KEDA_CHART_VERSION" \
        -n "$KEDA_NAMESPACE" \
        --set prometheus.metricServer.enabled=true \
        --set prometheus.operator.enabled=true \
        --wait \
        --timeout=5m; then
        log_error "KEDA Helm installation failed (SCALER_BACKEND=keda)"
    else
        log_success "KEDA deployed in $KEDA_NAMESPACE"
    fi
}

