#!/usr/bin/env bash
#
# Optional install step: create a default KEDA ScaledObject for each llm-d model
# server, so a fresh install actually autoscales something.
#
# Requires vars: WVA_NS, WVA_DEFAULT_SO, WVA_DEFAULT_SO_NS,
#                WVA_DEFAULT_SO_PLAN, WVA_DEFAULT_SO_MIN, WVA_DEFAULT_SO_MAX.
# Requires funcs: log_info/log_success/log_warning/log_error, wva_install_scope.
#
# Why this exists: a ScaledObject is not decoration, it is the REGISTRATION. WVA
# has no watch and no listing — it learns which workloads it manages from the KEDA
# calls it receives. So an install with no ScaledObject anywhere is a controller
# that will never be asked about anything, sitting idle and looking healthy.
#
# It is built as plan-then-apply rather than one shot, because creating autoscaling
# objects across a cluster is not something to discover the shape of afterwards.
# The plan is a plain TSV file: look at it, delete the rows you do not want, change
# a model or a replica bound, then apply it. The same file is the interchange for
# the interactive path and the scripted one, so there is no capability that needs a
# terminal — which matters, because these install scripts are otherwise
# non-interactive and CI depends on that.
#
# WVA_DEFAULT_SO:
#   false (default)  do nothing
#   plan             discover, print the table, write the plan, STOP
#   edit             plan, open $EDITOR, confirm, apply     (needs a terminal)
#   true             discover and apply it all, no questions
# WVA_DEFAULT_SO_PLAN=<file>
#   With an existing file: skip discovery and apply exactly that, edits included.
#   Otherwise: where the generated plan is written (default: a temp file).
# WVA_DEFAULT_SO_NS: a namespace, "wva" for WVA's own, or "all" for every namespace
#   holding model servers. Defaults to what this install can reach: "all" when
#   cluster-scoped, its own namespace when namespace-scoped.
#

readonly SO_PLAN_HEADER=$'#apply\tnamespace\tkind\tname\tmodelID\tinferencePool\tmin\tmax'

# so_model_id echoes the model a serving container runs: --served-model-name where
# the workload sets one (it is the name clients and the EPP use), else --model.
# Both "--flag value" and "--flag=value" are accepted; both appear in the wild.
#
# Empty output means the model could not be determined. The caller must record that
# and skip rather than guess: a ScaledObject with the wrong modelID groups a
# workload with a model it does not serve, and mis-scales both.
so_model_id() {
    local args="$1" flag tok next take
    for flag in --served-model-name --model; do
        take=""
        for tok in $args; do
            if [ -n "$take" ]; then
                case "$tok" in
                    --*) : ;;
                    *) echo "$tok"; return ;;
                esac
                take=""
            fi
            case "$tok" in
                "$flag"=*) next="${tok#*=}"; [ -n "$next" ] && { echo "$next"; return; } ;;
                "$flag")   take=1 ;;
            esac
        done
    done
}

# so_pool echoes the InferencePool whose selector matches a workload's pod labels,
# which is how WVA itself resolves it — the pool is derived, never declared. Shown
# in the plan for orientation only: it tells you which EPP queue a workload sits
# behind, and an empty column is a workload no pool has adopted.
so_pool() {
    local ns="$1" labels="$2" pool selector matched kv key value
    # Two selector shapes: inference.networking.k8s.io/v1 nests it under
    # matchLabels, the older x-k8s.io group had a bare map. Read both, as the
    # controller does — a plan that showed no pool because of an API version would
    # look exactly like a workload no pool has adopted.
    local tmpl='{{range .items}}{{.metadata.name}} {{if .spec.selector.matchLabels}}{{range $k,$v := .spec.selector.matchLabels}}{{$k}}={{$v}},{{end}}{{else}}{{range $k,$v := .spec.selector}}{{$k}}={{$v}},{{end}}{{end}}{{"\n"}}{{end}}'
    while read -r pool selector; do
        [ -n "$pool" ] || continue
        matched=yes
        for kv in $(echo "$selector" | tr ',' ' '); do
            [ -n "$kv" ] || continue
            key="${kv%%=*}"; value="${kv#*=}"
            case " $labels " in
                *" $key=$value "*) : ;;
                *) matched=""; break ;;
            esac
        done
        [ -n "$matched" ] && { echo "$pool"; return; }
    done < <(kubectl get inferencepools -n "$ns" -o go-template="$tmpl" 2>/dev/null)
}

# so_target_namespaces echoes the namespaces to scan.
#
# The DEFAULT follows the install's scope, because the scope already decides what
# this controller can reach:
#
#   cluster-scoped    every namespace holding model servers — it can manage them all
#   namespace-scoped  its own namespace — it restricts its cache to it and can
#                     manage nothing else, so scanning anywhere else would only
#                     produce ScaledObjects it will be called about and cannot read
#
# It used to default to LLMD_NS, which was wrong in both directions: it scanned one
# namespace on a cluster-scoped install that could have managed them all, and it
# scanned a namespace a namespace-scoped install cannot see.
so_target_namespaces() {
    local scope="${WVA_DEFAULT_SO_NS:-}"
    if [ -z "$scope" ]; then
        if [ "$(wva_install_scope)" = "cluster" ]; then scope=all; else scope=wva; fi
    fi
    case "$scope" in
        wva) echo "$WVA_NS"; return ;;
        all) : ;;
        *)   echo "$scope"; return ;;
    esac
    # Cluster-wide. Only meaningful for a cluster-scoped WVA: a namespace-scoped
    # install holds a Role, so it could only decline a workload anywhere else and a
    # ScaledObject there would call a scaler that refuses it.
    if [ "$(wva_install_scope)" != "cluster" ]; then
        log_warning "WVA_DEFAULT_SO_NS=all requested, but this is a namespace-scoped install — it restricts its cache to $WVA_NS and cannot read a workload anywhere else. Scanning $WVA_NS only."
        echo "$WVA_NS"
        return
    fi
    { kubectl get deployments -A -l llm-d.ai/inferenceServing=true \
        -o jsonpath='{range .items[*]}{.metadata.namespace}{"\n"}{end}' 2>/dev/null
      kubectl get leaderworkersets -A -l llm-d.ai/inferenceServing=true \
        -o jsonpath='{range .items[*]}{.metadata.namespace}{"\n"}{end}' 2>/dev/null
    } | sort -u
}

# scaledobject_exists reports whether some ScaledObject already targets this
# workload. Never adopt or overwrite one: it may be hand-tuned or GitOps-managed,
# and two ScaledObjects on one target is two HPAs fighting over a replica count.
scaledobject_exists() {
    [ -n "$(so_existing_name "$1" "$2")" ]
}

# so_existing_name echoes the name of the ScaledObject already targeting a
# workload, if any. Adoption has to patch THAT object: creating our own alongside
# it would put two ScaledObjects on one target, which is two HPAs writing the same
# replica count — the exact failure the skip-by-default exists to avoid.
so_existing_name() {
    local ns="$1" target="$2"
    kubectl get scaledobject -n "$ns" -o go-template='{{range .items}}{{.metadata.name}} {{.spec.scaleTargetRef.name}}{{"\n"}}{{end}}' 2>/dev/null \
        | awk -v t="$target" '$2 == t {print $1; exit}'
}

# so_discover writes plan rows to stdout, one per candidate workload. A row is
# marked "no" when it should not be applied, with the reason in the note, rather
# than dropped — the list you were shown is then the whole truth about what was
# found, and flipping a "no" to "yes" is a deliberate act.
so_discover() {
    local ns name args labels model pool kind apply note
    for ns in $(so_target_namespaces); do
        for kind in Deployment LeaderWorkerSet; do
            # go-template, not jsonpath: kubectl's jsonpath has no two-variable
            # range, so iterating a label map there is a parse error — and one that
            # produces an EMPTY result rather than a loud failure, which reads as
            # "no model servers found".
            local resource='deployments' pod='.spec.template'
            if [ "$kind" = "LeaderWorkerSet" ]; then
                resource='leaderworkersets'
                pod='.spec.leaderWorkerTemplate.leaderTemplate'
            fi
            local tmpl='{{range .items}}{{.metadata.name}}|{{range (index '"$pod"'.spec.containers 0).args}}{{.}} {{end}}|{{range $k,$v := '"$pod"'.metadata.labels}}{{$k}}={{$v}} {{end}}{{"\n"}}{{end}}'
            while IFS='|' read -r name args labels; do
                [ -n "$name" ] || continue
                apply=yes; note=""
                model=$(so_model_id "$args")
                if [ -z "$model" ]; then
                    apply=no; note="no --served-model-name or --model; set modelID by hand to include it"
                    model="UNKNOWN"
                fi
                if scaledobject_exists "$ns" "$name"; then
                    # Default is to leave it: it may be hand-tuned or
                    # GitOps-managed. WVA_DEFAULT_SO_ADOPT=true says you want it
                    # pointed at WVA, which is the case when you are adding WVA to
                    # a cluster whose workloads are already scaled by something
                    # else. The row is still yours to flip either way.
                    if [ "${WVA_DEFAULT_SO_ADOPT:-false}" = "true" ]; then
                        note="has a ScaledObject; will be UPDATED to use WVA (WVA_DEFAULT_SO_ADOPT=true)"
                    else
                        apply=no; note="already has a ScaledObject; set WVA_DEFAULT_SO_ADOPT=true to point it at WVA instead"
                    fi
                fi
                pool=$(so_pool "$ns" "$labels")
                printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
                    "$apply" "$ns" "$kind" "$name" "$model" "${pool:--}" \
                    "${WVA_DEFAULT_SO_MIN:-1}" "${WVA_DEFAULT_SO_MAX:-10}"
                [ -n "$note" ] && printf '# ^ %s/%s: %s\n' "$ns" "$name" "$note"
            done < <(kubectl get "$resource" -n "$ns" -l llm-d.ai/inferenceServing=true \
                -o go-template="$tmpl" 2>/dev/null)
        done
    done
}

so_show_plan() {
    local file="$1" rows
    rows=$(grep -cv '^#' "$file" 2>/dev/null || echo 0)
    echo ""
    echo "  Discovered llm-d model servers ($rows):"
    echo ""
    (echo "$SO_PLAN_HEADER"; cat "$file") | grep -v '^# \^' | column -t -s $'\t' | sed 's/^/    /'
    echo ""
    grep '^# \^' "$file" | sed 's/^# \^/    note:/' || true
    echo ""
}

# so_apply_plan creates a ScaledObject for every row marked yes.
so_apply_plan() {
    local file="$1" scaler_addr="wva-external-scaler.${WVA_NS}.svc.cluster.local:9090"
    local apply ns kind name model pool min max created=0 skipped=0
    while IFS=$'\t' read -r apply ns kind name model pool min max; do
        case "$apply" in ''|'#'*) continue ;; esac
        if [ "$apply" != "yes" ]; then
            skipped=$((skipped + 1)); continue
        fi
        if [ -z "$model" ] || [ "$model" = "UNKNOWN" ]; then
            log_warning "  $ns/$name: marked yes but modelID is UNKNOWN — skipping rather than guessing"
            skipped=$((skipped + 1)); continue
        fi
        local existing
        existing=$(so_existing_name "$ns" "$name")
        if [ -n "$existing" ]; then
            # Adoption: replace the triggers on the object that is already there,
            # leaving its envelope, behavior and everything else alone. Whoever
            # tuned min/max and stabilization had reasons; the only thing being
            # changed is who decides the count.
            if kubectl patch scaledobject "$existing" -n "$ns" --type=merge \
                -p "$(so_trigger_patch "$model" "$scaler_addr")" > /dev/null; then
                log_success "  $ns/$name ($kind) -> UPDATED existing ScaledObject $existing to scale on WVA (modelID: $model)"
                created=$((created + 1))
            else
                log_warning "  $ns/$name: failed to update ScaledObject $existing"
            fi
            continue
        fi
        if render_scaledobject "$ns" "$kind" "$name" "$model" "$scaler_addr" \
            "${min:-1}" "${max:-10}" | kubectl apply -f - > /dev/null; then
            log_success "  $ns/$name ($kind) -> ScaledObject ${name}-wva (modelID: $model)"
            created=$((created + 1))
        else
            log_warning "  $ns/$name: failed to create its ScaledObject"
        fi
    done < "$file"
    log_success "Default ScaledObjects: $created created, $skipped not applied"
}

install_default_scaledobjects() {
    local mode="${WVA_DEFAULT_SO:-false}"
    [ "$mode" != "false" ] || return 0

    local plan="${WVA_DEFAULT_SO_PLAN:-}"

    # An existing plan file is authoritative: this is the "edit the list and
    # continue" path, and it works with no terminal, which is what makes the
    # interactive capability available to scripts and CI as well.
    if [ -n "$plan" ] && [ -f "$plan" ]; then
        log_info "Applying the ScaledObject plan from $plan"
        so_show_plan "$plan"
        so_apply_plan "$plan"
        return 0
    fi

    [ -n "$plan" ] || plan=$(mktemp -t wva-scaledobject-plan.XXXXXX)
    log_info "Scanning for llm-d model servers..."
    so_discover > "$plan"

    if ! grep -qv '^#' "$plan"; then
        log_warning "No llm-d model servers found (label llm-d.ai/inferenceServing=true) in: $(so_target_namespaces | tr '\n' ' '). Deploy them first, then run 'make scaledobjects-apply'. Until a ScaledObject exists, WVA is never called and scales nothing."
        return 0
    fi

    so_show_plan "$plan"

    case "$mode" in
        plan)
            log_success "Plan written to $plan — nothing applied."
            log_info "Edit it (set the first column to yes/no, fix a modelID, change min/max), then:"
            log_info "    make scaledobjects-apply WVA_DEFAULT_SO_PLAN=$plan"
            return 0
            ;;
        edit)
            if [ ! -t 0 ]; then
                log_error "WVA_DEFAULT_SO=edit needs a terminal. Use WVA_DEFAULT_SO=plan, edit the file it writes, then apply it with WVA_DEFAULT_SO_PLAN=<file>."
            fi
            log_info "Opening the plan in ${EDITOR:-vi}. Set the first column to yes or no; delete rows to drop them."
            read -r -p "  Press Enter to edit, or Ctrl-C to stop with the plan at $plan " _
            ${EDITOR:-vi} "$plan"
            so_show_plan "$plan"
            read -r -p "  Apply this plan? [y/N] " reply
            case "$reply" in
                [yY]*) ;;
                *) log_info "Nothing applied. The plan is at $plan"; return 0 ;;
            esac
            ;;
        true) : ;;
        *) log_error "WVA_DEFAULT_SO must be one of: false, plan, edit, true (got '$mode')" ;;
    esac

    so_apply_plan "$plan"
}

# so_trigger_patch prints the merge patch that repoints an existing ScaledObject at
# WVA. Triggers only: the envelope, behavior and everything else on that object
# stay as whoever tuned them left them.
#
# `triggers` is a list, so a merge patch REPLACES it wholesale — which is what is
# wanted. An object scaled by a prometheus or cpu trigger must stop being scaled by
# it, or two scalers feed one HPA and the larger answer silently wins.
so_trigger_patch() {
    local model="$1" scaler_addr="$2"
    jq -nc --arg m "$model" --arg a "$scaler_addr" \
        '{spec:{triggers:[{type:"external-push",name:"wva-external-scaler",
          metadata:{scalerAddress:$a, modelID:$m}}]}}'
}

# render_scaledobject prints one ScaledObject: the shipped shape, or yours.
#
# WVA_DEFAULT_SO_TEMPLATE=<file> substitutes your own template instead, so a fleet
# with house conventions — fallback policy, stabilization windows, labels its
# tooling expects — gets those rather than a shape it then has to edit back.
# Placeholders, all optional:
#
#   {{NAMESPACE}} {{NAME}} {{KIND}} {{APIVERSION}} {{MODEL_ID}}
#   {{SCALER_ADDRESS}} {{MIN}} {{MAX}}
#
# Substitution is literal, so a template is also just a valid manifest with the
# placeholders written in — you can `kubectl apply` it by hand to check the shape
# before letting the installer fill it in for every model server you have.
render_scaledobject() {
    local ns="$1" kind="$2" target="$3" model="$4" scaler_addr="$5" min="$6" max="$7"
    local api="apps/v1"
    [ "$kind" = "LeaderWorkerSet" ] && api="leaderworkerset.x-k8s.io/v1"

    local tmpl="${WVA_DEFAULT_SO_TEMPLATE:-}"
    if [ -n "$tmpl" ]; then
        if [ ! -f "$tmpl" ]; then
            log_error "WVA_DEFAULT_SO_TEMPLATE=$tmpl does not exist"
        fi
        sed -e "s|{{NAMESPACE}}|${ns}|g" \
            -e "s|{{NAME}}|${target}|g" \
            -e "s|{{KIND}}|${kind}|g" \
            -e "s|{{APIVERSION}}|${api}|g" \
            -e "s|{{MODEL_ID}}|${model}|g" \
            -e "s|{{SCALER_ADDRESS}}|${scaler_addr}|g" \
            -e "s|{{MIN}}|${min}|g" \
            -e "s|{{MAX}}|${max}|g" \
            "$tmpl"
        return
    fi
    render_default_scaledobject "$ns" "$kind" "$target" "$model" "$scaler_addr" "$min" "$max"
}

# render_default_scaledobject prints one ScaledObject.
#
# external-push, not external: KEDA then holds a StreamIsActive stream open and WVA
# pushes activation the moment it decides, which is what lets a workload parked at
# zero wake in about the detection interval instead of a poll period.
#
# min defaults to 1 even where scale-to-zero is enabled: parking a model costs its
# next request a cold start, and that is a decision about that workload's users,
# not one an installer should make for them.
render_default_scaledobject() {
    local ns="$1" kind="$2" target="$3" model="$4" scaler_addr="$5" min="$6" max="$7"
    local api="apps/v1"
    [ "$kind" = "LeaderWorkerSet" ] && api="leaderworkerset.x-k8s.io/v1"

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
    apiVersion: ${api}
    kind: ${kind}
    name: ${target}
  pollingInterval: 5
  cooldownPeriod: 30
  minReplicaCount: ${min}
  maxReplicaCount: ${max}
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
