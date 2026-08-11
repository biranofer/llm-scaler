#!/usr/bin/env bash
#
# Core install orchestration for deploy/install.sh.
# Requires vars: ENVIRONMENT, SCRIPT_DIR, SKIP_CHECKS, deployment toggles.
# Requires funcs sourced by install.sh: parse_args(), check_prerequisites(),
# set_tls_verification(), set_wva_logging_level(), create_namespaces(), deploy_*(), verify_deployment(), print_summary().
# llm-d install: see llm-d project guides or deploy/install-epp.sh for kind EPP setup.
#

main() {
    parse_args "$@"

    # Preflight-only mode: the same check the install runs, run on its own so it
    # can be answered before committing to an install that creates namespaces and
    # RBAC before it discovers a missing tool.
    if [ "${CHECK_ONLY:-false}" = "true" ]; then
        check_prerequisites
        # Environment-specific "checks" are only run when they actually check.
        # kind-emulator's creates and tears down the cluster and loads images —
        # it is setup wearing a check's name, and a preflight that provisions
        # infrastructure is not one you can safely run to ask a question.
        if [ "$ENVIRONMENT" = "kind-emulator" ]; then
            log_info "Skipping kind-emulator environment checks: they provision the cluster rather than inspect it. Run 'make create-kind-cluster' for that."
        elif [ -f "$SCRIPT_DIR/$ENVIRONMENT/install.sh" ]; then
            # shellcheck source=/dev/null
            source "$SCRIPT_DIR/$ENVIRONMENT/install.sh"
            if declare -f check_specific_prerequisites > /dev/null; then
                check_specific_prerequisites
            fi
        fi
        check_permissions
        check_single_installation
        log_success "Preflight passed for ENVIRONMENT=$ENVIRONMENT, WVA_SCOPE=${WVA_SCOPE:-<platform default>}, WVA_LIMITER=${WVA_LIMITER:-none}"
        exit 0
    fi

    # Undeploy mode
    if [ "$UNDEPLOY" = "true" ]; then
        log_info "Starting Workload-Variant-Autoscaler Undeployment on $ENVIRONMENT"
        log_info "============================================================="
        echo ""

        if [ -f "$SCRIPT_DIR/$ENVIRONMENT/install.sh" ]; then
            # shellcheck source=/dev/null
            source "$SCRIPT_DIR/$ENVIRONMENT/install.sh"
        else
            log_error "Environment-specific script not found: $SCRIPT_DIR/$ENVIRONMENT/install.sh"
        fi

        cleanup
        exit 0
    fi

    log_info "Starting Workload-Variant-Autoscaler Deployment on $ENVIRONMENT"
    log_info "==========================================================="
    echo ""

    if [ "$SKIP_CHECKS" != "true" ]; then
        check_prerequisites
    fi

    # Before anything is created: a second install repoints the shared
    # ClusterRoleBindings and leaves the existing controller without permissions,
    # which nothing reports.
    if [ "$DEPLOY_WVA" = "true" ]; then
        # Before anything is created: a missing permission discovered halfway
        # leaves a partial install behind.
        if [ "$SKIP_CHECKS" != "true" ]; then
            check_permissions
        fi
        check_single_installation
    fi

    set_tls_verification
    set_wva_logging_level

    if [[ "${CLUSTER_TYPE:-}" == "kind" ]]; then
        log_info "Kind cluster detected - setting environment to kind-emulated"
        ENVIRONMENT="kind-emulator"
    fi

    log_info "Loading environment-specific functions for $ENVIRONMENT..."
    if [ -f "$SCRIPT_DIR/$ENVIRONMENT/install.sh" ]; then
        # shellcheck source=/dev/null
        source "$SCRIPT_DIR/$ENVIRONMENT/install.sh"

        if declare -f check_specific_prerequisites > /dev/null; then
            if [ "$SKIP_CHECKS" != "true" ]; then
                check_specific_prerequisites
            fi
        fi
    else
        log_error "Environment script not found: $SCRIPT_DIR/$ENVIRONMENT/install.sh"
    fi

    log_info "Using configuration:"
    echo "    Deployed on:          $ENVIRONMENT"
    echo "    WVA Image:            $WVA_IMAGE_REPO:$WVA_IMAGE_TAG"
    echo "    WVA Namespace:        $WVA_NS"
    echo "    llm-d Namespace:      $LLMD_NS"
    echo "    Monitoring Namespace: $MONITORING_NAMESPACE"
    echo "    Scaler Backend:       $SCALER_BACKEND"
    echo ""

    create_namespaces

    deploy_monitoring_stack

    # Deploy WVA prerequisites first (environment-specific).
    if [ "$DEPLOY_WVA" = "true" ]; then
        deploy_wva_prerequisites
    fi

    # Deploy WVA controller via Kustomize.
    if [ "$DEPLOY_WVA" = "true" ]; then
        deploy_wva_controller
    else
        log_info "Skipping WVA deployment (DEPLOY_WVA=false)"
    fi

    deploy_scaler_backend

    # After the scaler backend: a ScaledObject is meaningless until KEDA is there
    # to reconcile it.
    install_default_scaledobjects

    verify_deployment

    print_summary

    log_success "Deployment on $ENVIRONMENT complete!"
}
