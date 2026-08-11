#!/usr/bin/env bash
#
# Shared monitoring orchestration helpers.
# Keeps install.sh main flow concise while delegating to environment/plugin functions.
# Requires funcs: deploy_prometheus_stack(), log_info/log_warning/log_success,
# wait_deployment_available_nonfatal().
# Requires vars: DEPLOY_PROMETHEUS.
#

# foreign_prometheus echoes any Prometheus on this cluster that this install did
# NOT put there, or nothing.
#
# The Prometheus Operator's CRDs are cluster-scoped and singular: a second
# kube-prometheus-stack install brings its own operator, which then watches the
# SAME Prometheus/ServiceMonitor resources as the first. Two operators reconciling
# one set of objects fight over them, and the visible symptom is somebody else's
# monitoring going intermittently blind — a bad way to find out you installed WVA.
#
# "Foreign" means outside MONITORING_NAMESPACE. A Prometheus we deployed earlier
# lives there, and re-running the install over it must still upgrade it: this
# script is meant to be idempotent, and the e2e re-deploys onto a live cluster
# every run. Detecting merely "a Prometheus exists" would have turned that upgrade
# into a silent skip.
foreign_prometheus() {
    kubectl get crd prometheuses.monitoring.coreos.com >/dev/null 2>&1 || return 0
    kubectl get prometheuses.monitoring.coreos.com -A \
        -o go-template="{{range .items}}{{if ne .metadata.namespace \"${MONITORING_NAMESPACE}\"}}{{.metadata.namespace}}/{{.metadata.name}} {{end}}{{end}}" 2>/dev/null || true
    return 0
}

# install_operational_dashboard publishes the WVA dashboard as a sidecar-labelled
# ConfigMap.
#
# It lives OUTSIDE the kube-prometheus-stack install because the dashboard and the
# Prometheus are independent decisions. It used to be a block inside that install,
# so the dashboard shipped only when this script also deployed Prometheus — meaning
# the case that needs it most, an existing cluster with its own monitoring, was the
# one case that never got it. Whoever operates WVA there was left reading raw
# PromQL for panels we already ship.
#
# The Grafana sidecar discovers dashboards by label, not by namespace ownership, so
# a plain labelled ConfigMap is all an existing Grafana needs. DASHBOARD_NS targets
# a Grafana living somewhere other than MONITORING_NAMESPACE.
install_operational_dashboard() {
    [ "${DEPLOY_OPERATIONAL_DASHBOARD:-true}" = "true" ] || return 0

    local json="$WVA_PROJECT/deploy/grafana/operational-dashboard.json"
    local ns="${DASHBOARD_NS:-$MONITORING_NAMESPACE}"
    if [ ! -f "$json" ]; then
        log_warning "Operational dashboard JSON not found at $json — skipping"
        return 0
    fi
    kubectl get namespace "$ns" >/dev/null 2>&1 || {
        log_warning "Namespace $ns does not exist — skipping the operational dashboard. Set DASHBOARD_NS to the namespace your Grafana watches."
        return 0
    }

    if kubectl create configmap wva-operation-dashboard \
        --from-file=operational-dashboard.json="$json" \
        -n "$ns" --dry-run=client -o yaml \
        | kubectl label --local -f - grafana_dashboard=1 -o yaml \
        | kubectl apply -f - >/dev/null; then
        log_success "Operational dashboard published to $ns (ConfigMap wva-operation-dashboard, label grafana_dashboard=1)"
    else
        log_warning "Could not publish the operational dashboard to $ns"
    fi
}

deploy_monitoring_stack() {
    if [ "$DEPLOY_PROMETHEUS" != "true" ]; then
        log_info "Skipping Prometheus deployment (DEPLOY_PROMETHEUS=false) — WVA will read ${PROMETHEUS_URL:-the configured URL}"
        install_operational_dashboard
        return 0
    fi

    # DEPLOY_PROMETHEUS defaults to true because the new-cluster path needs a
    # Prometheus and most people running this have none. That default is wrong for
    # the OTHER common case — an existing llm-d cluster, which always has one — so
    # detect rather than obey. KEDA is handled the same way a few files over; this
    # was the one piece of shared infrastructure that still installed blind.
    local existing
    existing="$(foreign_prometheus)"
    if [ -n "$existing" ] && [ "${PROMETHEUS_FORCE_INSTALL:-false}" != "true" ]; then
        log_info "Prometheus is already on this cluster (${existing% }) — not installing a second one."
        log_info "  WVA will scrape ${PROMETHEUS_URL:-the shipped default}. If that is not the right endpoint, re-run with PROMETHEUS_URL=<url>."
        log_info "  Pass PROMETHEUS_FORCE_INSTALL=true to install anyway (two Prometheus Operators reconcile the same CRs and will fight)."
        # The dashboard is still ours to ship, and this is precisely the cluster
        # whose operator has a Grafana to put it in.
        install_operational_dashboard
        return 0
    fi
    if [ -n "$existing" ]; then
        log_warning "PROMETHEUS_FORCE_INSTALL=true with Prometheus already present (${existing% }). The two operators will contend over the same Prometheus and ServiceMonitor resources."
    fi

    deploy_prometheus_stack
}
