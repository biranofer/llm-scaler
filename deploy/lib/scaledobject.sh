#!/usr/bin/env bash
#
# Optional install step: create a default KEDA ScaledObject for each llm-d model
# server, so a fresh install actually autoscales something.
#
# Requires vars: WVA_NS, LLMD_NS, WVA_DEFAULT_SO, WVA_DEFAULT_SO_NS,
#                WVA_DEFAULT_SO_MIN, WVA_DEFAULT_SO_MAX, WVA_SCOPE (optional).
# Requires funcs: log_info/log_success/log_warning/log_error, wva_install_scope.
#
# Why this exists: a ScaledObject is not decoration, it is the REGISTRATION. WVA
# has no watch and no listing — it learns which workloads it manages from the KEDA
# calls it receives. So an install with no ScaledObject anywhere is a controller
# that will never be asked about anything, sitting idle and looking healthy. This
# closes that gap for the common case without asking the operator to hand-write
# one per model.
#

# so_model_id echoes the model a serving container runs: --served-model-name where
# the deployment sets one (it is the name clients and the EPP use), else --model.
# Both the "--flag value" and "--flag=value" forms are accepted, because both
# appear in the wild.
#
# Empty output means the model could not be determined, and the caller must skip
# rather than guess: a ScaledObject with the wrong modelID groups a workload with
# a model it does not serve, and mis-scales both.
#
# Parsed in shell rather than with yq: the input is a space-separated arg list, and
# a JSON-path expression over it was both harder to read and wrong.
so_model_id() {
    local args="$1" flag tok next take
    for flag in --served-model-name --model; do
        take=""
        for tok in $args; do
            if [ -n "$take" ]; then
                # The token after the flag, unless the flag was last or is
                # followed by another flag — then it carried no value.
                case "$tok" in
                    --*) : ;;
                    *) echo "$tok"; return ;;
                esac
                take=""
            fi
            case "$tok" in
                "$flag"=*)  next="${tok#*=}"
                            [ -n "$next" ] && { echo "$next"; return; } ;;
                "$flag")    take=1 ;;
            esac
        done
    done
}

# so_target_namespaces echoes the namespaces to create ScaledObjects in.
so_target_namespaces() {
    local scope="${WVA_DEFAULT_SO_NS:-$LLMD_NS}"
    if [ "$scope" != "all" ]; then
        echo "$scope"
        return
    fi
    # Cluster-wide. Only meaningful for a cluster-scoped WVA: a namespace-scoped
    # install has a Role, not a ClusterRole, so it cannot manage a workload
    # anywhere else and a ScaledObject there would call a scaler that declines it.
    if [ "$(wva_install_scope)" != "cluster" ]; then
        log_warning "WVA_DEFAULT_SO_NS=all requested, but this is a namespace-scoped install — it can only manage $WVA_NS. Creating ScaledObjects in $LLMD_NS only."
        echo "$LLMD_NS"
        return
    fi
    kubectl get deployments -A -l llm-d.ai/inferenceServing=true \
        -o jsonpath='{range .items[*]}{.metadata.namespace}{"\n"}{end}' 2>/dev/null | sort -u
}

# scaledobject_exists reports whether some ScaledObject in the namespace already
# targets this Deployment. Never adopt or overwrite one: it may be hand-tuned, or
# GitOps-managed, and two ScaledObjects on one target is a fight between two HPAs.
scaledobject_exists() {
    local ns="$1" target="$2"
    kubectl get scaledobject -n "$ns" \
        -o jsonpath='{range .items[*]}{.spec.scaleTargetRef.name}{"\n"}{end}' 2>/dev/null \
        | grep -qx "$target"
}

install_default_scaledobjects() {
    [ "${WVA_DEFAULT_SO:-false}" = "true" ] || return 0

    log_info "Creating default ScaledObjects for llm-d model servers..."
    local scaler_addr="wva-external-scaler.${WVA_NS}.svc.cluster.local:9090"
    local created=0 skipped=0 unknown=0
    local namespaces
    namespaces=$(so_target_namespaces)

    if [ -z "$namespaces" ]; then
        log_warning "No namespaces to scan for llm-d model servers; skipping default ScaledObjects."
        return 0
    fi

    local ns name args model
    for ns in $namespaces; do
        while IFS='|' read -r name args; do
            [ -n "$name" ] || continue

            if scaledobject_exists "$ns" "$name"; then
                log_info "  $ns/$name: a ScaledObject already targets it, leaving it alone"
                skipped=$((skipped + 1))
                continue
            fi

            model=$(so_model_id "$args")
            if [ -z "$model" ] || [ "$model" = "null" ]; then
                log_warning "  $ns/$name: cannot determine the model from its container args (no --served-model-name or --model). Skipping: a ScaledObject with the wrong modelID would group this workload with a model it does not serve."
                unknown=$((unknown + 1))
                continue
            fi

            if render_default_scaledobject "$ns" "$name" "$model" "$scaler_addr" | kubectl apply -f - > /dev/null; then
                log_success "  $ns/$name -> ScaledObject ${name}-wva (modelID: $model)"
                created=$((created + 1))
            else
                log_warning "  $ns/$name: failed to create its ScaledObject"
            fi
            # Args emitted space-separated rather than as the JSON array, so the
            # shell can read them as words.
        done < <(kubectl get deployments -n "$ns" -l llm-d.ai/inferenceServing=true \
            -o jsonpath='{range .items[*]}{.metadata.name}|{range .spec.template.spec.containers[0].args[*]}{@}{" "}{end}{"\n"}{end}' 2>/dev/null)
    done

    if [ "$created" -eq 0 ] && [ "$skipped" -eq 0 ] && [ "$unknown" -eq 0 ]; then
        log_warning "No llm-d model servers found (label llm-d.ai/inferenceServing=true). Deploy your model servers first, then re-run with WVA_DEFAULT_SO=true — or write ScaledObjects yourself. Until one exists, WVA is never called and will not scale anything."
        return 0
    fi
    log_success "Default ScaledObjects: $created created, $skipped left alone, $unknown skipped for an undeterminable model"
}

# render_default_scaledobject prints one ScaledObject.
#
# external-push, not external: KEDA then holds a StreamIsActive stream open and
# WVA pushes activation the moment it decides, which is what lets a workload
# parked at zero wake in about the detection interval instead of a poll period.
#
# minReplicaCount is 1 by default. Zero is not the default even where scale-to-zero
# is enabled: parking a model costs the next request a cold start, and that is a
# decision about a workload's users, not one an installer should make for them.
render_default_scaledobject() {
    local ns="$1" target="$2" model="$3" scaler_addr="$4"
    cat <<EOF
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: ${target}-wva
  namespace: ${ns}
  labels:
    app.kubernetes.io/managed-by: workload-variant-autoscaler
    app.kubernetes.io/component: default-scaledobject
  annotations:
    llm-d.ai/created-by: "deploy/lib/scaledobject.sh"
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: ${target}
  pollingInterval: 5
  cooldownPeriod: 30
  minReplicaCount: ${WVA_DEFAULT_SO_MIN:-1}
  maxReplicaCount: ${WVA_DEFAULT_SO_MAX:-10}
  advanced:
    restoreToOriginalReplicaCount: true
  triggers:
    - type: external-push
      name: wva-external-scaler
      metadata:
        scalerAddress: ${scaler_addr}
        modelID: ${model}
EOF
}
