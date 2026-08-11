#!/usr/bin/env bash
#
# Shared verification and deployment summary helpers for deploy/install.sh.
# Covers WVA / Prometheus / scaler backend only — llm-d is installed separately via llm-d project guides.
# Requires funcs: log_info/log_warning/log_success, containsElement().
# Uses constants: DEFAULT_VERIFY_STARTUP_SLEEP_SECONDS and shared selectors.
#

verify_deployment() {
    log_info "Verifying deployment..."

    local all_good=true

    # --- WVA
    log_info "Checking WVA controller pods..."
    sleep "$DEFAULT_VERIFY_STARTUP_SLEEP_SECONDS"
    if kubectl get pods -n "$WVA_NS" -l "$WVA_CONTROLLER_LABEL_SELECTOR" 2>/dev/null | grep -q Running; then
        log_success "WVA controller is running"
    else
        log_warning "WVA controller may still be starting"
        all_good=false
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
    else
        log_warning "Some components may still be starting. Check the logs above."
    fi
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
    echo "  Watching:     $LLMD_NS"
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
    [ -n "${CONTROLLER_INSTANCE:-}" ] &&         echo "  Instance:     $CONTROLLER_INSTANCE (manages ONLY workloads labelled wva.llmd.ai/controller-instance=$CONTROLLER_INSTANCE)"
    echo ""

    # The step everyone misses. WVA has no watch and no listing: it is asked about
    # a workload by KEDA, or it never hears of it.
    echo "  NEXT: nothing scales yet."
    echo ""
    echo "  A ScaledObject is how a workload registers with WVA. Until one exists,"
    echo "  the controller is running and idle. To see what it would create:"
    echo ""
    echo "      make scaledobjects-plan LLMD_NS=$LLMD_NS"
    echo ""
    echo "  then apply it, or an edited copy of it:"
    echo ""
    echo "      make scaledobjects-apply LLMD_NS=$LLMD_NS"
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
