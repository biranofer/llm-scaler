#!/usr/bin/env bash
#
# Core install orchestration for deploy/install.sh.
# Requires vars: ENVIRONMENT, SCRIPT_DIR, SKIP_CHECKS, deployment toggles.
# Requires funcs sourced by install.sh: parse_args(), check_prerequisites(),
# set_tls_verification(), set_wva_logging_level(), create_namespaces(), deploy_*(), verify_deployment(), print_summary().
# llm-d install: see llm-d project guides or deploy/install-epp.sh for kind EPP setup.
#

# require_prereqs_present refuses a controller-only install when the admin phase
# has not run for this namespace.
#
# It names every missing object rather than letting `kubectl apply` fail on the
# first one: the person running this phase is, by design, the person who CANNOT
# create them, so the useful output is the list to hand to an admin — not a
# Forbidden on one object with the rest unknown.
require_prereqs_present() {
    local missing=() kind name
    while read -r kind name; do
        [ -n "$kind" ] && [ -n "$name" ] || continue
        case "$kind" in
            ServiceMonitor)
                kubectl get servicemonitor "$name" -n "$WVA_NS" >/dev/null 2>&1 \
                    || missing+=("$kind/$name (in $WVA_NS)") ;;
            *)
                kubectl get "$kind" "$name" >/dev/null 2>&1 || missing+=("$kind/$name") ;;
        esac
    done < <(wva_rendered_prereq_objects)

    if [ ${#missing[@]} -eq 0 ]; then
        log_success "Prerequisites are in place for $WVA_NS"
        return 0
    fi

    log_error "The cluster-admin prerequisites for $WVA_NS are not in place. Missing:
$(printf '  - %s\n' "${missing[@]}")

Ask a cluster admin to run, once, for this namespace:

    make setup-prereqs-$(wva_install_scope)-on-$([ "$ENVIRONMENT" = openshift ] && echo openshift || echo k8s) WVA_NS=$WVA_NS

after which this phase needs no cluster-scoped rights and you can re-run it, and
every later upgrade, yourself."
}

# print_prereqs_summary tells the admin what they just created and what to hand
# over. The handover is the point of the phase, so it is stated rather than left
# to be inferred from a list of objects.
print_prereqs_summary() {
    local scope platform
    scope="$(wva_install_scope)"
    platform=$([ "$ENVIRONMENT" = openshift ] && echo openshift || echo k8s)
    echo ""
    echo "=========================================="
    echo " Prerequisites ready for $WVA_NS"
    echo "=========================================="
    echo ""
    echo "  Created (or already present):"
    wva_rendered_prereq_objects | sed 's|^\([^ ]*\) |    \1/|'
    echo "    plus the scaler backend and monitoring, if this cluster had none"
    echo ""
    echo "  NOT created: the controller itself. Whoever owns $WVA_NS installs it,"
    echo "  and every later upgrade, with no cluster-scoped rights:"
    echo ""
    echo "      make deploy-wva-${scope}-on-${platform} WVA_NS=${WVA_NS}"
    echo ""
    echo "  Re-run this phase only when the WVA version changes what it needs"
    echo "  cluster-wide (new RBAC rules), not for an ordinary controller upgrade."
    echo ""
}

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
        # "What do I pass for PROMETHEUS_URL?" is the question a reader actually
        # arrives with, and the answer is knowable from here.
        wva_report_prometheus
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

    # INSTALL_PHASE splits the install where the permissions split.
    #
    #   prereqs     what a CLUSTER ADMIN must do once per namespace: the namespace
    #               itself, the cluster-scoped RBAC, the shared infrastructure
    #               (Prometheus, KEDA) and the ServiceMonitor.
    #   wva         the controller, which a namespace admin can then install and
    #               upgrade on their own for as long as the prereqs stand.
    #   all         both, in order — the single-command install, and the default,
    #               so nothing about the existing behaviour changes.
    #
    # The phases are ordered, not independent: `wva` assumes `prereqs` has run,
    # and says so when it has not rather than failing on the first missing object.
    local phase="${INSTALL_PHASE:-all}"
    local do_prereqs=false do_wva=false
    case "$phase" in
        prereqs) do_prereqs=true ;;
        wva)     do_wva=true ;;
        all)     do_prereqs=true; do_wva=true ;;
        *)       log_error "INSTALL_PHASE must be prereqs, wva or all (got '$phase')" ;;
    esac

    if [ "$do_prereqs" = true ]; then
        create_namespaces

        deploy_monitoring_stack

        # Environment-specific WVA prerequisites (on OpenShift: the SA token
        # Secret and the service-ca bundle the ServiceMonitor authenticates with).
        if [ "$DEPLOY_WVA" = "true" ]; then
            deploy_wva_prerequisites
        fi

        if [ "$DEPLOY_WVA" = "true" ]; then
            WVA_APPLY_SCOPE=$([ "$phase" = "all" ] && echo all || echo prereqs) \
                deploy_wva_controller
        fi

        deploy_scaler_backend
    fi

    if [ "$do_wva" = true ]; then
        if [ "$DEPLOY_WVA" != "true" ]; then
            log_info "Skipping WVA deployment (DEPLOY_WVA=false)"
        elif [ "$phase" = "wva" ]; then
            require_prereqs_present
            WVA_APPLY_SCOPE=controller deploy_wva_controller
        fi

        # After the scaler backend: a ScaledObject is meaningless until KEDA is there
        # to reconcile it.
        install_default_scaledobjects
    fi

    if [ "$phase" = "prereqs" ]; then
        print_prereqs_summary
        exit 0
    fi

    # Honour the verdict. `verify_deployment` computed one and threw it away, so a
    # controller that never started still ended with "complete!" and exit 0 — the
    # two things a wrapper or a CI job actually reads.
    local verified=true
    verify_deployment || verified=false

    print_summary

    if [ "$verified" != true ]; then
        echo ""
        log_warning "Deployment on $ENVIRONMENT FINISHED WITH ERRORS — the WVA controller is not ready."
        log_warning "Everything above was created; nothing is scaling. Diagnose with the commands printed above, then re-run this script (it is idempotent)."
        return 1
    fi

    log_success "Deployment on $ENVIRONMENT complete!"
}
