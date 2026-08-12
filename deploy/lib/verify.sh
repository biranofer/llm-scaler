#!/usr/bin/env bash
#
# Shared verification and deployment summary helpers for deploy/install.sh.
# Covers WVA / Prometheus / scaler backend only — llm-d is installed separately via llm-d project guides.
# Requires funcs: log_info/log_warning/log_success, containsElement().
# Uses constants: DEFAULT_VERIFY_STARTUP_SLEEP_SECONDS and shared selectors.
#

# verify_deployment returns non-zero when the thing this script exists to install
# is not running. The caller MUST honour that: it used to compute `all_good` and
# then discard it, so an install whose controller never started printed
# "Deployment complete!" and exited 0 — which is what CI and every wrapper script
# reads. A silent false success is worse than a loud failure.
#
# Only WVA itself is fatal. Prometheus, Grafana and KEDA may be pre-existing,
# externally managed, or genuinely still pulling images; they stay warnings.
verify_deployment() {
    log_info "Verifying deployment..."

    local all_good=true

    # --- WVA
    log_info "Waiting for the WVA controller to become available..."
    # `kubectl get pods | grep Running` was not a readiness check: a pod stuck at
    # "0/1 Running" — crash-looping on a bad config, or unable to reach Prometheus —
    # matches it. Ask the Deployment whether it is Available instead, which is the
    # condition that means the container passed its probes.
    if kubectl wait --for=condition=Available \
        deployment -n "$WVA_NS" -l "$WVA_CONTROLLER_LABEL_SELECTOR" \
        --timeout="${WVA_VERIFY_TIMEOUT:-180s}" >/dev/null 2>&1; then
        log_success "WVA controller is running and ready"
    else
        all_good=false
        log_warning "WVA controller did NOT become ready within ${WVA_VERIFY_TIMEOUT:-180s}."
        kubectl get pods -n "$WVA_NS" -l "$WVA_CONTROLLER_LABEL_SELECTOR" >&2 2>/dev/null || true

        # Say WHY, here, rather than printing two commands to run. The container's
        # own last words are almost always the answer and they are one API call
        # away — a released image rejecting a flag this branch's manifests pass
        # ("unknown flag: --external-scaler-bind-address") looks identical to a
        # network problem until you read them.
        local reason
        reason=$(kubectl get pods -n "$WVA_NS" -l "$WVA_CONTROLLER_LABEL_SELECTOR" \
            -o jsonpath='{range .items[*].status.containerStatuses[*]}{.state.waiting.reason}{" "}{.state.waiting.message}{.lastState.terminated.reason}{"\n"}{end}' 2>/dev/null \
            | grep -v '^[[:space:]]*$' | head -3)
        [ -n "$reason" ] && log_warning "  Container state: $(printf '%s' "$reason" | tr '\n' ';')"

        local logs
        logs=$(kubectl logs -n "$WVA_NS" -l "$WVA_CONTROLLER_LABEL_SELECTOR" \
            --tail=8 --all-containers 2>/dev/null \
            || kubectl logs -n "$WVA_NS" -l "$WVA_CONTROLLER_LABEL_SELECTOR" \
                --tail=8 --previous 2>/dev/null)
        if [ -n "$logs" ]; then
            log_warning "  Last lines from the container:"
            printf '%s\n' "$logs" | sed 's/^/      /' >&2
        fi

        log_warning "  Full logs: kubectl -n $WVA_NS logs -l $WVA_CONTROLLER_LABEL_SELECTOR --tail=50 --previous"
        log_warning "  Events:    kubectl -n $WVA_NS get events --sort-by=.lastTimestamp | tail -20"
        log_warning "  If it is rejecting a flag, IMG and these manifests are different versions: build and push this tree (make docker-build docker-push IMG=<ref>) and re-run with that IMG."
    fi

    # --- Monitoring
    if [ "$DEPLOY_PROMETHEUS" = "true" ]; then
        log_info "Checking Prometheus..."
        if kubectl get pods -n "$MONITORING_NAMESPACE" -l app.kubernetes.io/name=prometheus 2>/dev/null | grep -q Running; then
            log_success "Prometheus is running"
        else
            log_warning "Prometheus may still be starting"
        fi
    fi

    if [ "$DEPLOY_OPERATIONAL_DASHBOARD" = "true" ]; then
        log_info "Checking Grafana..."
        if kubectl get pods -n "$MONITORING_NAMESPACE" -l app.kubernetes.io/name=grafana 2>/dev/null | grep -q Running; then
            log_success "Grafana is running"
        else
            log_warning "Grafana may still be starting"
        fi
    fi

    # --- Scaler backend
    if [ "$SCALER_BACKEND" = "keda" ]; then
        log_info "Checking KEDA..."
        if kubectl get pods -n "$KEDA_NAMESPACE" -l "$KEDA_OPERATOR_LABEL_SELECTOR" 2>/dev/null | grep -q Running; then
            log_success "KEDA is running"
        else
            log_warning "KEDA may still be starting"
        fi
    elif [ "$SCALER_BACKEND" = "none" ]; then
        log_info "Scaler backend skipped (SCALER_BACKEND=none) — assuming external metrics API is pre-installed"
    fi

    if [ "$all_good" = true ]; then
        log_success "All components verified successfully!"
        return 0
    fi
    return 1
}

# print_summary is the most-read text this install produces: it is what someone
# looks at to find out whether it worked and what to do next. So it says the one
# thing that is true and easy to miss — an installed WVA scales nothing until a
# ScaledObject exists — and it reports the settings that will otherwise surprise
# someone later (no GPU budget, scale-to-zero, a named instance).
print_summary() {
    echo ""
    echo "=========================================="
    echo " Workload-Variant-Autoscaler installed"
    echo "=========================================="
    echo ""
    echo "  Controller:   $WVA_NS  ($WVA_IMAGE_REPO:$WVA_IMAGE_TAG)"
    echo "  Scope:        $(wva_install_scope)-scoped"
    if [ "$(wva_install_scope)" = "cluster" ]; then
        echo "  Manages:      every namespace"
    else
        echo "  Manages:      $WVA_NS only (its cache is restricted to it)"
    fi
    if [ "$DEPLOY_PROMETHEUS" = "true" ]; then
        echo "  Prometheus:   deployed in $MONITORING_NAMESPACE"
    else
        echo "  Prometheus:   ${PROMETHEUS_URL:-<the shipped default>} (not deployed by this install)"
    fi
    if [ "$SCALER_BACKEND" = "keda" ]; then
        echo "  KEDA:         installed or already present"
    else
        echo "  KEDA:         skipped (SCALER_BACKEND=$SCALER_BACKEND) — WVA needs KEDA to actuate"
    fi
    [ "$DEPLOY_OPERATIONAL_DASHBOARD" = "true" ] && echo "  Grafana:      deployed in $MONITORING_NAMESPACE"
    # CONTROLLER_INSTANCE is deliberately NOT reported here. No install path puts
    # it on the Deployment, so this line read the installer's shell environment and
    # then asserted a scoping rule the running controller was not applying — the
    # most misleading kind of summary, because it is specific and confident.
    echo ""

    # The step everyone misses. WVA has no watch and no listing: it is asked about
    # a workload by KEDA, or it never hears of it.
    echo "  NEXT: nothing scales yet."
    echo ""
    echo "  A ScaledObject is how a workload registers with WVA. Until one exists,"
    echo "  the controller is running and idle. To see what it would create:"
    echo ""
    echo "      make scaledobjects-plan"
    echo ""
    echo "  then apply it, or an edited copy of it:"
    echo ""
    echo "      make scaledobjects-apply"
    echo ""

    echo "  Worth knowing:"
    case "$(effective_limiter_summary)" in
        none) echo "    - No GPU limiter: scaling is UNBOUNDED. Set WVA_LIMITER=gpu-inventory or quota" ;;
        *)    echo "    - GPU limiter: $(effective_limiter_summary). A workload whose accelerator does not resolve gets no budget and will not scale up" ;;
    esac
    if [ "${ENABLE_SCALE_TO_ZERO:-false}" = "true" ]; then
        echo "    - Scale-to-zero is ON: an idle model can be parked at 0 and woken on demand"
    else
        echo "    - Scale-to-zero is OFF (ENABLE_SCALE_TO_ZERO=false)"
    fi
    echo ""

    echo "  Checking on it:"
    echo "    kubectl -n $WVA_NS logs -l app.kubernetes.io/name=workload-variant-autoscaler -f"
    echo "    kubectl get scaledobject -A          # what WVA has been told about"
    echo "    kubectl get hpa -A                   # KEDA acts through these"
    echo ""
    echo "  Guide: deploy/README.md"
    echo "=========================================="
}

# effective_limiter_summary echoes the limiter this install declared.
effective_limiter_summary() {
    echo "${WVA_LIMITER:-none}"
}
