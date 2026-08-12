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

# wva_detect_prometheus_url echoes an in-cluster URL for a Prometheus that is
# already here, or nothing.
#
# The platform defaults name the Prometheus THIS INSTALLER WOULD DEPLOY. On a
# cluster that has its own — the common case, and the whole point of the
# existing-llm-d path — that default is silently wrong, and WVA exits on a
# Prometheus it cannot reach. It presents as CrashLoopBackOff, which reads like a
# broken image rather than a setting nobody was asked for.
#
# Detection order is most-specific first. Everything here is a READ; the caller
# decides whether to use the answer.
wva_detect_prometheus_url() {
    local ns name port scheme

    # OpenShift: THANOS, at a well-known address, and no lookup at all.
    #
    # The platform's aggregation point is thanos-querier in openshift-monitoring,
    # and that is a constant — not something to discover. Reading it would need
    # access to a platform namespace that a tenant does not have, so a version of
    # this that "detected" it reported NOTHING for exactly the reader who most
    # needs the answer. (Verified on a real cluster: a namespace admin gets
    # nothing back.) Knowing beats looking.
    if wva_is_openshift; then
        # Refine the port only if we happen to be able to read it. 9091 is the
        # TLS-terminated one the platform has served for years; the read is a
        # nicety, never a requirement.
        port="$(kubectl get svc thanos-querier -n openshift-monitoring \
            -o jsonpath='{.spec.ports[?(@.name=="web")].port}' 2>/dev/null || true)"
        echo "https://thanos-querier.openshift-monitoring.svc.cluster.local:${port:-9091}"
        return 0
    fi

    # A Prometheus Operator install: every Prometheus CR gets a `prometheus-operated`
    # Service in its own namespace, whatever the release is called — so this holds
    # for kube-prometheus-stack and for a hand-rolled operator install alike.
    while read -r ns name; do
        [ -n "$ns" ] || continue
        if kubectl get svc prometheus-operated -n "$ns" >/dev/null 2>&1; then
            port="$(kubectl get svc prometheus-operated -n "$ns" \
                -o jsonpath='{.spec.ports[?(@.name=="web")].port}' 2>/dev/null || true)"
            echo "http://prometheus-operated.${ns}.svc.cluster.local:${port:-9090}"
            return 0
        fi
    done < <(kubectl get prometheuses.monitoring.coreos.com -A \
        -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)

    # Last resort: a Service serving a Prometheus web port, in the namespaces this
    # install already knows about. Deliberately NOT `get svc -A`: on a large
    # shared cluster that is slow enough to look hung, and a tenant cannot list
    # services cluster-wide anyway, so it would be a long wait for a denial.
    for ns in "${MONITORING_NAMESPACE:-}" "${WVA_NS:-}" "${LLMD_NS:-}" monitoring prometheus; do
        [ -n "$ns" ] || continue
        while read -r name port; do
            [ -n "$name" ] || continue
            case "$name" in *prometheus*|*thanos-quer*) : ;; *) continue ;; esac
            scheme=http
            # The scheme follows the port: a TLS port answered over http gives
            # "connection reset", which is a far worse thing to debug than
            # "no Prometheus found".
            case "$port" in 9091|8443) scheme=https ;; esac
            echo "${scheme}://${name}.${ns}.svc.cluster.local:${port}"
            return 0
        done < <(kubectl get svc -n "$ns" \
            -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.ports[0].port}{"\n"}{end}' 2>/dev/null || true)
    done

    return 0
}

# wva_is_openshift reports whether this is an OpenShift cluster.
#
# ENVIRONMENT first, because the make targets set it and it costs nothing. The
# fallback asks API DISCOVERY, which every authenticated user may read — unlike
# the platform namespaces, which a tenant may not.
wva_is_openshift() {
    [ "${ENVIRONMENT:-}" = "openshift" ] && return 0
    kubectl api-resources --api-group=route.openshift.io -o name >/dev/null 2>&1 \
        && [ -n "$(kubectl api-resources --api-group=route.openshift.io -o name 2>/dev/null)" ]
}

# wva_report_prometheus tells the reader which Prometheus this install will use,
# and how that was decided. Called by the preflight, where "what do I pass for
# PROMETHEUS_URL?" is the question actually being asked.
wva_report_prometheus() {
    local detected
    if [ -n "${PROMETHEUS_URL_EXPLICIT:-}" ]; then
        log_info "Prometheus: $PROMETHEUS_URL_EXPLICIT (you set PROMETHEUS_URL)"
        return 0
    fi
    detected="$(wva_detect_prometheus_url)"
    if [ -n "$detected" ]; then
        log_success "Prometheus: $detected"
        if wva_is_openshift; then
            log_info "  OpenShift's Thanos Querier — a fixed address, so this needs no permission to know."
        fi
        log_info "  You do not need to pass PROMETHEUS_URL. Set it only to override this."
        return 0
    fi
    # Never fold "I may not look" into "there is none": one is a missing
    # permission and the other is a missing Prometheus, and they are fixed by
    # different people.
    if ! kubectl auth can-i list prometheuses.monitoring.coreos.com -A >/dev/null 2>&1; then
        log_warning "Could not look for a Prometheus (listing them cluster-wide is not permitted for you), so this cannot tell you the URL."
        log_warning "  Ask whoever runs your monitoring for it, and pass PROMETHEUS_URL=<url>."
        return 0
    fi
    log_warning "No Prometheus found on this cluster. The install will deploy one (DEPLOY_PROMETHEUS=true), or pass PROMETHEUS_URL=<url> to point at one outside the cluster."
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
