#!/usr/bin/env bash
#
# Shared kube-prometheus-stack install for Kubernetes-like environments
# (vanilla Kubernetes, Kind emulator, etc.). Sourced by deploy/*/install.sh.
# Requires vars: MONITORING_NAMESPACE, PROMETHEUS_SECRET_NAME,
# PROMETHEUS_PORT, PROMETHEUS_URL.
# Requires funcs: log_info/log_warning/log_success.
#

deploy_prometheus_kube_stack() {
    log_info "Deploying kube-prometheus-stack with TLS..."

    helm repo add prometheus-community https://prometheus-community.github.io/helm-charts || true
    if [ "${SKIP_HELM_REPO_UPDATE:-}" = "true" ]; then
        log_info "Skipping helm repo update (SKIP_HELM_REPO_UPDATE=true)"
    else
        helm repo update
    fi

    log_info "Creating self-signed TLS certificate for Prometheus"
    openssl req -x509 -newkey rsa:2048 -nodes \
        -keyout /tmp/prometheus-tls.key \
        -out /tmp/prometheus-tls.crt \
        -days 365 \
        -subj "/CN=prometheus" \
        -addext "subjectAltName=DNS:kube-prometheus-stack-prometheus.${MONITORING_NAMESPACE}.svc.cluster.local,DNS:kube-prometheus-stack-prometheus.${MONITORING_NAMESPACE}.svc,DNS:prometheus,DNS:localhost" \
        &> /dev/null

    log_info "Creating Kubernetes secret for Prometheus TLS"
    # Not `&> /dev/null`. This apply failing is how the install learns the
    # monitoring namespace is missing, and suppressing its output turned that into
    # a bare exit under `set -e`: the last thing printed was "Creating Kubernetes
    # secret for Prometheus TLS" and there was no error anywhere.
    if ! kubectl create secret tls "$PROMETHEUS_SECRET_NAME" \
        --cert=/tmp/prometheus-tls.crt \
        --key=/tmp/prometheus-tls.key \
        -n "$MONITORING_NAMESPACE" \
        --dry-run=client -o yaml | kubectl apply -f -; then
        rm -f /tmp/prometheus-tls.key /tmp/prometheus-tls.crt
        log_error "Could not create the Prometheus TLS secret in $MONITORING_NAMESPACE (see the error above)."
    fi

    rm -f /tmp/prometheus-tls.key /tmp/prometheus-tls.crt

    log_info "Installing kube-prometheus-stack with TLS configuration"
    install_operational_dashboard

    # kube-prometheus-stack is one release in one namespace shared by every WVA on
    # the cluster, and "is there one already" is checked before this runs — which
    # is not atomic. Two first-time installs starting together both see no stack
    # and both write the same release. Helm marks an in-flight release
    # pending-install/upgrade, so refuse on that rather than racing it.
    local phelm_status
    phelm_status="$(helm status kube-prometheus-stack -n "$MONITORING_NAMESPACE" -o json 2>/dev/null | jq -r '.info.status // ""' 2>/dev/null || true)"
    case "$phelm_status" in
        pending-*)
            log_error "Another install is deploying kube-prometheus-stack in $MONITORING_NAMESPACE right now (helm reports it $phelm_status). It is shared by every WVA on this cluster, so this install would race it. Wait for it to finish and re-run, or pass PROMETHEUS_URL=<url> to use a Prometheus this install does not deploy."
            ;;
    esac

    helm upgrade --install kube-prometheus-stack prometheus-community/kube-prometheus-stack \
        -n "$MONITORING_NAMESPACE" \
        --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false \
        --set prometheus.prometheusSpec.podMonitorSelectorNilUsesHelmValues=false \
        --set prometheus.service.type=ClusterIP \
        --set prometheus.service.port="$PROMETHEUS_PORT" \
        --set prometheus.prometheusSpec.web.tlsConfig.cert.secret.name="$PROMETHEUS_SECRET_NAME" \
        --set prometheus.prometheusSpec.web.tlsConfig.cert.secret.key=tls.crt \
        --set prometheus.prometheusSpec.web.tlsConfig.keySecret.name="$PROMETHEUS_SECRET_NAME" \
        --set prometheus.prometheusSpec.web.tlsConfig.keySecret.key=tls.key \
        --set grafana.enabled="$DEPLOY_OPERATIONAL_DASHBOARD" \
        --set grafana.sidecar.dashboards.enabled=true \
        --set grafana.sidecar.dashboards.label=grafana_dashboard \
        --set 'grafana.datasources.datasources\.yaml.apiVersion=1' \
        --set-string 'grafana.datasources.datasources\.yaml.datasources[0].name=Prometheus' \
        --set-string 'grafana.datasources.datasources\.yaml.datasources[0].type=prometheus' \
        --set-string 'grafana.datasources.datasources\.yaml.datasources[0].url=https://kube-prometheus-stack-prometheus.'"$MONITORING_NAMESPACE"'.svc.cluster.local:9090' \
        --set-string 'grafana.datasources.datasources\.yaml.datasources[0].access=proxy' \
        --set-string 'grafana.datasources.datasources\.yaml.datasources[0].jsonData.httpMethod=POST' \
        --set-string 'grafana.datasources.datasources\.yaml.datasources[0].jsonData.timeInterval=30s' \
        --set 'grafana.datasources.datasources\.yaml.datasources[0].jsonData.tlsSkipVerify=true' \
        --set alertmanager.enabled=false \
        --timeout=10m \
        --wait

    log_success "kube-prometheus-stack deployed with TLS"
    log_info "Prometheus URL: $PROMETHEUS_URL"
}

undeploy_prometheus_kube_stack() {
    log_info "Uninstalling kube-prometheus-stack..."

    helm uninstall kube-prometheus-stack -n "$MONITORING_NAMESPACE" 2>/dev/null || \
        log_warning "Prometheus stack not found or already uninstalled"

    kubectl delete secret "$PROMETHEUS_SECRET_NAME" -n "$MONITORING_NAMESPACE" --ignore-not-found

    log_success "Prometheus stack uninstalled"
}
