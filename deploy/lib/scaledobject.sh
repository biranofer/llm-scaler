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
# The plan is a YAML file that carries its own documentation: every field it
# accepts is explained in the comments it is written with, so editing it needs
# nothing open next to it. It was a TSV, and a table has nowhere to say what a
# column means or which values a column accepts — so the file could only be edited
# by someone who had already read the docs, and the docs had to repeat it all.
#
# The same file is the interchange for the interactive path and the scripted one,
# so there is no capability that needs a terminal — which matters, because these
# install scripts are otherwise non-interactive and CI depends on that. It is
# printed as a table as well: YAML is what you edit, a table is what you read.
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

readonly SO_PLAN_HEADER=$'APPLY\tNAMESPACE\tKIND\tNAME\tMODELID\tMIN\tMAX\tCOST\tPOLICY'

# The variantCost an entry gets when nothing else decides one. It is WVA's own
# default for a trigger that omits the field, so writing it into every entry
# changes no behaviour — it makes the number visible and editable, which is what
# matters the first time a model gets a second variant.
readonly SO_DEFAULT_VARIANT_COST=10.0

# so_plan_preamble writes the plan's header comment: what every field means, and
# which values `apply` takes. It is generated with the plan and read in the editor,
# which is the whole point of the file being YAML.
so_plan_preamble() {
    cat <<'EOF'
# WVA ScaledObject plan. Nothing here has been applied yet.
#
# Edit this file, then apply exactly what you leave in it:
#
#     make scaledobjects-apply WVA_DEFAULT_SO_PLAN=<this file>
#
# Commented lines are never read back. Anything informational is written as a
# comment for exactly that reason: editing it would change what you were told,
# not what gets applied. Each entry's `apply:` line carries the values that entry
# actually accepts — `adopt` only appears where there is something to adopt.
#
# apply          Required. One of:
#                  yes    create a ScaledObject for this workload
#                  no     leave the workload alone
#                  adopt  it already has a ScaledObject — repoint that one at WVA
#                         instead of adding a second. Two ScaledObjects on one
#                         target is two HPAs writing the same replica count.
#                         Offered only for workloads that have one.
#
# modelID        Required. What the container serves: --served-model-name, or
#                --model where there is no served name. It is also the grouping
#                key — entries sharing a modelID are sized against each other, so
#                a wrong one mis-scales both of them.
#
# minReplicas    The bounds KEDA holds this workload between; WVA decides within
# maxReplicas    them. Defaults 1 and 10. minReplicas: 0 parks the workload when
#                idle and costs its next request a cold start. maxReplicas is the
#                only ceiling on this workload unless a GPU limiter is configured.
#
# variantCost    The relative price of one replica of this variant. Only the ratio
#                between variants of the same model matters, so with one variant
#                per model it changes nothing and 10.0 is as good as any number.
#                Where a model has two, this is what makes WVA prefer one: give
#                the cheaper variant the smaller cost.
#
# scalingPolicy  Optional, and commented out because absent is the right value for
#                most installs. It names a reusable TIER — "interactive",
#                "standard", "batch" — defined in the scaling-policy ConfigMap,
#                whose thresholds this variant scales under. Leave it out and the
#                cluster's own default applies, which an admin can change for
#                every workload at once; name one here and this workload stops
#                following that. A name no tier matches falls back to the default
#                silently, so an entry naming one is a claim that can go stale.
#
# namespace      Where the workload is; with kind and name, what the ScaledObject
# kind           will target.
# name
#
# The comments each entry carries:
#
#   scaledObject   the ScaledObject already scaling this workload, if any. Its
#                  name is what `apply: adopt` would repoint.
#   inferencePool  the EPP queue this workload sits behind, resolved by matching
#                  pod labels against each pool's selector — the same way WVA
#                  resolves it. "(none)" means no pool selects these pods.
EOF
}

# so_plan_entry writes one plan entry, with its note (if any) as the comment above
# it. Notes are comments rather than a column so that they survive editing and can
# be a sentence: the reason a row says `no` is the thing a reader most needs.
so_plan_entry() {
    local apply="$1" ns="$2" kind="$3" name="$4" model="$5" min="$6" max="$7" cost="$8"
    local policy="$9" pool="${10}" existing="${11}" note="${12:-}"
    # The values this entry accepts, not the values the field accepts: offering
    # `adopt` for a workload with nothing to adopt invites a choice that can only
    # be a mistake, and offering it where there is something is the one place
    # anybody would look for it.
    local choices="yes | no"
    [ -z "$existing" ] || choices="yes | no | adopt"
    echo ""
    [ -z "$note" ] || printf '  # note: %s\n' "$note"
    # apply and modelID are quoted: YAML reads a bare yes/no/on/off as a boolean
    # under some parsers, and a model called `3.5` or `1e6` as a number. Both would
    # arrive here as something that is not the text anybody typed.
    printf '  - apply: "%s"  # %s\n' "$apply" "$choices"
    printf '    namespace: %s\n' "$ns"
    printf '    kind: %s\n' "$kind"
    printf '    name: %s\n' "$name"
    printf '    modelID: "%s"\n' "$model"
    printf '    minReplicas: %s\n' "$min"
    printf '    maxReplicas: %s\n' "$max"
    # Quoted, so an edit keeps what you typed: unquoted 10.0 is a YAML float, and
    # a float that round-trips through JSON comes back as "10".
    printf '    variantCost: "%s"\n' "$cost"
    if [ -n "$policy" ]; then
        printf '    scalingPolicy: "%s"\n' "$policy"
    else
        printf '    # scalingPolicy: "standard"\n'
    fi
    # Informational, and comments so they stay that way: neither is read back,
    # so changing one cannot change what happens behind the reader's back.
    [ -z "$existing" ] || printf '    # scaledObject: %s\n' "$existing"
    printf '    # inferencePool: %s\n' "${pool:-(none)}"
}

# so_plan_rows echoes one row per plan entry, fields separated by US (\037): the
# file is YAML for whoever edits it, and a record stream for everything that
# consumes it.
#
# NOT tab-separated. Tab is an IFS *whitespace* character, so `IFS=$'\t' read`
# collapses a run of them into one delimiter and an empty field simply vanishes —
# every later field then arrives one place to the left. A workload whose model
# could not be read has an empty modelID, so it got a ScaledObject whose modelID
# was its minReplicas: the value 1, registered as a model name, silently. US is
# not IFS whitespace, so empty fields survive; values are scrubbed of it below,
# though nothing a Kubernetes object can hold contains one.
#
# yq converts and jq selects, rather than yq doing both: expressing "this field or
# this default, joined" in yq's filter language needs quoting that is its own
# hazard.
#
# An unparseable plan is fatal and says why. The alternative — skipping what could
# not be read — would apply part of a plan, and on this file a partial apply means
# creating autoscaling objects for a subset nobody chose.
so_plan_rows() {
    local file="$1" json
    if ! json=$(yq -o=json '.' "$file" 2>&1); then
        log_error "$file is not valid YAML, so nothing was applied: $json"
    fi
    # An EMPTY plan is not a broken plan. so_write_plan writes `plan:` with no
    # entries when the namespace holds no model servers yet — the ordinary state
    # of a namespace where the controller was installed before the workloads —
    # and yq parses that to null. Treating null as unreadable made the documented
    # sequence print an ERROR from the read-only planning step and then fail
    # `scaledobjects-apply` with exit 2 on the very file it had just written.
    # Absent key: still an error, because that file is not a plan at all.
    case "$(printf '%s' "$json" | jq -r 'if (.plan | type) == "array" then "ok" elif (has("plan") and .plan == null) then "empty" else "bad" end' 2>/dev/null)" in
        ok)    : ;;
        empty) return 0 ;;
        *)     log_error "$file has no 'plan:' list. It must keep a top-level plan: key holding one entry per workload — see the comments at the top of the file." ;;
    esac
    printf '%s' "$json" | jq -r --arg cost "$SO_DEFAULT_VARIANT_COST" '
        .plan[]
        | [ (if (.apply | type) == "boolean"
             then (if .apply then "yes" else "no" end)
             else (.apply // "" | tostring) end | ascii_downcase),
            (.namespace // ""), (.kind // ""), (.name // ""), (.modelID // ""),
            (.minReplicas // 1 | tostring), (.maxReplicas // 10 | tostring),
            (.variantCost // $cost | tostring), (.scalingPolicy // "") ]
        | map(tostring | gsub("[
	]"; " "))
        | join("")'
}

# What marks a workload as an llm-d model server.
#
# It is matched against the POD TEMPLATE's labels, and the object's own only as a
# fallback — NOT with `kubectl get -l`, which only ever looks at the object. llm-d
# puts these labels where they do the work: on the pod template, because that is
# what the InferencePool's EPP selects on, and on the selector. The Deployment
# itself carries none of them. So `-l llm-d.ai/inferenceServing=true` matched
# nothing on a real install, and the scan reported "no llm-d model servers found"
# — an install that then autoscales nothing, and says only that it found nothing
# to autoscale.
readonly SO_SERVING_MARKER='llm-d.ai/inferenceServing=true'

# Where each kind keeps its pod template, as a jq path. An LWS may omit
# leaderTemplate entirely, and plenty of workloads have a container with no args
# — getpath and // handle both, which a go-template does not: `range` over a
# missing field is an execution error that empties the WHOLE listing, and this
# scan reads every workload in the namespace, not only the model servers.
readonly SO_POD_PATH_DEPLOYMENT='["spec","template"]'
readonly SO_POD_PATH_LWS='["spec","leaderWorkerTemplate","leaderTemplate"]'

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
        [ -n "$matched" ] && { echo "$pool"; return 0; }
    done < <(kubectl get inferencepools -n "$ns" -o go-template="$tmpl" 2>/dev/null)
    # No pool adopted this workload. That is a normal answer — the column is shown
    # for orientation — so it must not be a non-zero status: the caller assigns
    # this in a command substitution under `set -e`, and a "no match" would abort
    # the whole scan on the first workload no pool selects.
    return 0
}

# so_target_namespaces echoes the namespaces to scan, one per line.
#
# stdout here is DATA, consumed by `for ns in $(so_target_namespaces)`. Anything
# else written to it becomes a namespace name — which is what happened while the
# log helpers wrote to stdout: the warning below was split into words and each was
# used as a namespace, so every lookup failed into 2>/dev/null and the scan
# reported "no model servers found".
#
# The DEFAULT follows the install's scope, because the scope already decides what
# this controller can reach:
#
#   cluster-scoped    every namespace holding model servers — it can manage them all
#   namespace-scoped  its own namespace — it restricts its cache to it and can
#                     manage nothing else, so scanning anywhere else would only
#                     produce ScaledObjects it will be called about and cannot read
#
# It used to default to NAMESPACE, which was wrong in both directions: it scanned one
# namespace on a cluster-scoped install that could have managed them all, and it
# scanned a namespace a namespace-scoped install cannot see.
so_target_namespaces() {
    local scope="${WVA_DEFAULT_SO_NS:-}"
    if [ -z "$scope" ]; then
        if [ "$(wva_install_scope)" = "cluster" ]; then scope=all; else scope=wva; fi
    fi
    case "$scope" in
        # The namespace this controller MANAGES, which is its own unless it was
        # installed to watch another. Scanning WVA_NS unconditionally meant that an
        # admin-owned controller — the whole point of which is to sit outside the
        # namespace it manages — planned against its own empty namespace and
        # reported that there was nothing to autoscale.
        wva) echo "${WVA_WATCH_NS:-$WVA_NS}"; return ;;
        all) : ;;
        *)   echo "$scope"; return ;;
    esac
    # Cluster-wide. Only meaningful for a cluster-scoped WVA: a namespace-scoped
    # install restricts its cache to its own namespace, so it could not read a
    # workload anywhere else and a
    # ScaledObject there would call a scaler that refuses it.
    if [ "$(wva_install_scope)" != "cluster" ]; then
        log_warning "WVA_DEFAULT_SO_NS=all requested, but this is a namespace-scoped install — it restricts its cache to $WVA_NS and cannot read a workload anywhere else. Scanning $WVA_NS only."
        echo "$WVA_NS"
        return
    fi
    wva_namespaces_with_model_servers
}

# wva_namespaces_with_model_servers echoes every namespace holding an llm-d model
# server, one per line. Empty output means either none or "not allowed to look" —
# callers that care must ask which, because they are different problems.
wva_namespaces_with_model_servers() {
    { so_namespaces_of deployments "$SO_POD_PATH_DEPLOYMENT"
      so_namespaces_of leaderworkersets "$SO_POD_PATH_LWS"
    } | sort -u
}

# so_namespaces_of echoes the namespace of every workload of one kind that carries
# the model-server marker, on its pod template or on the object itself.
so_namespaces_of() {
    local resource="$1" pod="$2"
    # `|| true` is load-bearing. A cluster-wide list is exactly what a NAMESPACE
    # ADMIN may not do, and a denied kubectl exits non-zero: under the callers'
    # `set -e` with pipefail, that took the whole preflight down without printing
    # a single line — a tenant running `make check-prereqs-namespace-on-k8s` got
    # silence. Not finding namespaces is a legitimate answer here; the caller
    # decides what it means.
    kubectl get "$resource" -A -o json 2>/dev/null \
        | jq -r --argjson p "$pod" --arg marker "$SO_SERVING_MARKER" '
            .items[]
            | ((getpath($p + ["metadata","labels"]) // {}) + (.metadata.labels // {}))
              as $labels
            | select($labels | to_entries
                     | any(.key + "=" + (.value|tostring) == $marker))
            | .metadata.namespace' 2>/dev/null || true
}

# so_existing_info echoes `name min max cost` for the ScaledObject already targeting a
# workload, if any. Adoption has to patch THAT object: creating our own alongside
# it would put two ScaledObjects on one target, which is two HPAs writing the same
# replica count — the exact failure the skip-by-default exists to avoid.
#
# The bounds come with the name because an `adopt` row must show the bounds the
# workload actually has, not the ones a fresh install would have picked. A plan
# that showed 1-10 for a workload someone had pinned to 4-4 would quietly widen it
# on apply, and the file gave no hint that it was about to.
#
# jq, not a go-template: `{{if .spec.minReplicaCount}}` is false for 0, so
# scale-to-zero workloads would read as "unset" and come back as 1. The `//`
# defaults below are KEDA's own for an absent field.
so_existing_info() {
    local ns="$1" target="$2"
    kubectl get scaledobject -n "$ns" -o json 2>/dev/null \
        | jq -r --arg t "$target" '
            .items[]
            | select(.spec.scaleTargetRef.name == $t)
            | [ .metadata.name,
                (.spec.minReplicaCount // 0 | tostring),
                (.spec.maxReplicaCount // 100 | tostring),
                ([ .spec.triggers[]? | select(.type | startswith("external"))
                   | .metadata.variantCost // empty ] | first // "10.0"),
                ([ .spec.triggers[]? | select(.type | startswith("external"))
                   | .metadata.scalingPolicy // empty ] | first // "") ]
            | join(" ")' 2>/dev/null \
        | head -1
}

# so_existing_name echoes just the name, for callers that only ask whether one is
# there. Re-read at apply time: a plan can be applied long after it was written.
so_existing_name() {
    so_existing_info "$1" "$2" | awk '{print $1}'
}

# so_discover writes the plan to stdout: the documented preamble, then one entry
# per candidate workload. An entry is marked "no" when it should not be applied,
# with the reason in its note, rather than dropped — the file you were shown is
# then the whole truth about what was found, and turning a "no" into a "yes" is a
# deliberate act rather than an undiscoverable one.
so_discover() {
    local ns name args labels objlabels model pool kind apply note
    local existing existing_name existing_min existing_max existing_cost existing_policy
    local min max cost policy
    so_plan_preamble
    echo "plan:"
    for ns in $(so_target_namespaces); do
        for kind in Deployment LeaderWorkerSet; do
            local resource='deployments' pod="$SO_POD_PATH_DEPLOYMENT"
            if [ "$kind" = "LeaderWorkerSet" ]; then
                resource='leaderworkersets'
                pod="$SO_POD_PATH_LWS"
            fi
            # args LAST: a serving container's args can legitimately contain a
            # pipe (a shell string, a chat template), and `read` gives the final
            # variable the whole remainder — so only a trailing field is safe.
            while IFS='|' read -r name labels objlabels args; do
                [ -n "$name" ] || continue
                # The marker, on the pod template or on the object. Kept separate
                # from $labels, which is POD labels only: so_pool matches those
                # against InferencePool selectors, and folding the object's in
                # could claim a pool that does not actually select these pods.
                case " $labels $objlabels " in
                    *" $SO_SERVING_MARKER "*) : ;;
                    *) continue ;;
                esac
                apply=yes; note=""
                min="${WVA_DEFAULT_SO_MIN:-1}"; max="${WVA_DEFAULT_SO_MAX:-10}"
                # Reset every per-workload variable here, not only the ones this
                # workload sets: existing_name is assigned inside a conditional, so
                # a stale one would offer `adopt` on the next workload and name an
                # object that scales something else entirely.
                cost="$SO_DEFAULT_VARIANT_COST"; policy=""; existing_name=""
                model=$(so_model_id "$args")
                if [ -z "$model" ]; then
                    apply=no
                    note="no --served-model-name or --model on the container, so the model could not be read. Fill in modelID and set apply: yes to include it."
                fi
                existing=$(so_existing_info "$ns" "$name")
                if [ -n "$existing" ]; then
                    # Default is to leave it alone: it may be hand-tuned or
                    # GitOps-managed. `apply: adopt` is how you say you want it
                    # pointed at WVA — the case when you are adding WVA to a
                    # cluster whose workloads something else already scales.
                    #
                    # Its own bounds are carried into the entry, so adopting it
                    # unedited changes only who decides the count.
                    read -r existing_name existing_min existing_max existing_cost existing_policy <<< "$existing"
                    min="$existing_min"; max="$existing_max"; cost="$existing_cost"
                    policy="$existing_policy"
                    apply=no
                    # Appended, not assigned: a workload can both be scaled by
                    # something else and have no readable model, and adopting it
                    # would then point a ScaledObject at an empty modelID.
                    note="${note:+$note }Already scaled by ScaledObject $existing_name (min $existing_min, max $existing_max). Set apply: adopt to point that one at WVA instead of adding a second."
                fi
                pool=$(so_pool "$ns" "$labels")
                so_plan_entry "$apply" "$ns" "$kind" "$name" "$model" \
                    "$min" "$max" "$cost" "$policy" "$pool" "$existing_name" "$note"
            done < <(kubectl get "$resource" -n "$ns" -o json 2>/dev/null \
                | jq -r --argjson p "$pod" '
                    .items[]
                    | . as $o
                    | (getpath($p) // {}) as $t
                    | [ $o.metadata.name,
                        (($t.metadata.labels // {}) | to_entries
                         | map(.key + "=" + (.value|tostring)) | join(" ")),
                        (($o.metadata.labels // {}) | to_entries
                         | map(.key + "=" + (.value|tostring)) | join(" ")),
                        # Newlines collapsed to spaces: read below takes one
                        # record per line, and the llm-d modelservice chart
                        # wraps vllm serve as a single multi-line args string (a
                        # bash -c script), which would otherwise split one
                        # record across many lines and hide its
                        # --served-model-name past the first line. \r goes with
                        # it: a chart authored on Windows carries CRLF inside
                        # the block scalar, and CR is not in bash IFS, so the
                        # modelID would keep an invisible trailing character and
                        # match no metric series.
                        (($t.spec.containers[0].args // [])
                         | map(tostring | gsub("[\n\r]"; " ")) | join(" "))
                      ] | join("|")')
        done
    done
}

# so_show_plan prints the plan as a table. The file stays the thing you edit; this
# is the thing you read before deciding to.
#
# Empty fields become "-": `column -t` folds consecutive delimiters into one, so a
# workload with no readable model would print with every later column shifted one
# place left, and the table would say its min was its model.
so_show_plan() {
    local file="$1" rows count
    # `|| exit 1`, not `|| true`: so_plan_rows dies on a plan it cannot read, and
    # that death happens inside this command substitution's subshell. Without this
    # the caller would print an empty table and carry on to apply nothing, having
    # already shown the user the reason and then contradicted it.
    rows=$(so_plan_rows "$file") || exit 1
    count=$(printf '%s' "$rows" | grep -c . || true)
    echo ""
    echo "  Discovered llm-d model servers ($count):"
    echo ""
    { echo "$SO_PLAN_HEADER"
      # `$1 = $1` forces the record to be rebuilt with OFS. Without it awk only
      # rebuilds when a field is assigned, so a row that happened to have no empty
      # field printed with its original separators still in it — and the table
      # showed that row as one unsplit run of text while its neighbours lined up.
      printf '%s\n' "$rows" \
        | awk -F'\037' -v OFS='\t' '{for (i = 1; i <= NF; i++) if ($i == "") $i = "-"; $1 = $1; print}'
    } | column -t -s $'\t' | sed 's/^/    /'
    echo ""
    grep -E '^[[:space:]]*# note:' "$file" 2>/dev/null | sed -E 's/^[[:space:]]*# note:/    note:/' || true
    echo ""
}

# so_apply_plan acts on every entry: creating for yes, repointing for adopt.
so_apply_plan() {
    local file="$1" scaler_addr="wva-external-scaler.${WVA_NS}.svc.cluster.local:9090"
    local apply ns kind name model min max cost policy existing rows
    local created=0 adopted=0 skipped=0 unresolved=0

    # Read the whole plan before touching anything, and stop if it could not be
    # read. so_plan_rows exits on an unparseable file, but that exit happens in
    # this subshell — without `|| exit 1` the loop saw no rows, applied nothing,
    # and reported a clean "0 created" for a file whose error it had just printed.
    rows=$(so_plan_rows "$file") || exit 1

    # An empty plan applies nothing, and says so in those words. It reaches here
    # whenever the namespace has no model servers yet, which is not an error and
    # must not read like one.
    if [ -z "$rows" ]; then
        log_info "The plan has no entries, so nothing was applied. Deploy the model servers, re-run 'make scaledobjects-plan', and apply that."
        return 0
    fi

    while IFS=$'\037' read -r apply ns kind name model min max cost policy; do
        [ -n "$name" ] || continue
        case "$apply" in
            yes|adopt) : ;;
            no) skipped=$((skipped + 1)); continue ;;
            '') log_warning "  $ns/$name: no apply: field — skipping. It must be yes, no or adopt."
                skipped=$((skipped + 1)); continue ;;
            *)  log_warning "  $ns/$name: apply: $apply is not one of yes, no, adopt — skipping."
                skipped=$((skipped + 1)); continue ;;
        esac
        # No modelID, no object. It is the grouping key and the only thing tying a
        # ScaledObject to what the container actually serves: an object created
        # without it registers a variant of a model nobody runs, and WVA then sizes
        # a group that does not exist. Guessing is worse than refusing.
        if [ -z "$model" ]; then
            log_warning "  $ns/$name: apply: $apply, but modelID is empty — NOT creating anything for it. Set the model the container serves (--served-model-name, else --model) and run this again."
            unresolved=$((unresolved + 1)); skipped=$((skipped + 1)); continue
        fi
        # Bounds are edited by hand, and reach kubectl as JSON numbers. Anything
        # that is not one falls back to the default rather than being passed
        # through: a `minReplicas: two` would otherwise produce an invalid patch
        # against a live object, which is a worse way to find out.
        case "$min" in ''|*[!0-9]*) log_warning "  $ns/$name: minReplicas '$min' is not a number — using 1."; min=1 ;; esac
        case "$max" in ''|*[!0-9]*) log_warning "  $ns/$name: maxReplicas '$max' is not a number — using 10."; max=10 ;; esac
        case "$cost" in
            ''|*[!0-9.]*|*.*.*)
                log_warning "  $ns/$name: variantCost '$cost' is not a number — using $SO_DEFAULT_VARIANT_COST."
                cost="$SO_DEFAULT_VARIANT_COST" ;;
        esac

        existing=$(so_existing_name "$ns" "$name")
        if [ "$apply" = "yes" ] && [ -n "$existing" ]; then
            # Not silently adopted instead: "yes" asked for a new object, and
            # creating one beside $existing is two ScaledObjects on one target,
            # which is two HPAs writing one replica count. Adoption is a different
            # act, which is why it has its own value.
            log_warning "  $ns/$name: apply: yes, but ScaledObject $existing already targets it — skipping. Use apply: adopt to point that one at WVA."
            skipped=$((skipped + 1)); continue
        fi
        if [ "$apply" = "adopt" ] && [ -z "$existing" ]; then
            log_warning "  $ns/$name: apply: adopt, but nothing targets it any more — creating one instead."
            apply=yes
        fi

        if [ "$apply" = "adopt" ]; then
            # Triggers and bounds, and nothing else: the rest of that object —
            # polling interval, fallback, stabilization, whoever's labels — stays
            # as its author left it. The bounds are included because the plan
            # showed them, carried over from this very object, so applying them
            # unedited is a no-op and editing them is how you change them.
            if kubectl patch scaledobject "$existing" -n "$ns" --type=merge \
                -p "$(so_adopt_patch "$model" "$scaler_addr" "$min" "$max" "$cost" "$policy")" > /dev/null; then
                log_success "  $ns/$name ($kind) -> ScaledObject $existing now scales on WVA (modelID: $model, $min-$max)"
                adopted=$((adopted + 1))
            else
                log_warning "  $ns/$name: failed to update ScaledObject $existing"
            fi
            continue
        fi

        if render_scaledobject "$ns" "$kind" "$name" "$model" "$scaler_addr" \
            "$min" "$max" "$cost" "$policy" | kubectl apply -f - > /dev/null; then
            log_success "  $ns/$name ($kind) -> ScaledObject ${name}-wva (modelID: $model, $min-$max)"
            created=$((created + 1))
        else
            log_warning "  $ns/$name: failed to create its ScaledObject"
        fi
    done <<< "$rows"
    log_success "ScaledObjects: $created created, $adopted adopted, $skipped not applied"

    # Non-zero, after the rest of the plan has been applied: an entry asking to be
    # created with no model is a mistake in the file, and a caller that scripts
    # this — the installer, CI — must not read "some of what you asked for" as
    # success. The entries that were complete are already applied; nothing here
    # needs undoing, and running it again after filling in the model is safe.
    if [ "$unresolved" -gt 0 ]; then
        if [ "$unresolved" = 1 ]; then
            log_error "1 entry asked to be applied with no modelID and was skipped. Fill in its modelID, or set apply: no."
        fi
        log_error "$unresolved entries asked to be applied with no modelID and were skipped. Fill in their modelID, or set apply: no."
    fi
}

install_default_scaledobjects() {
    local mode="${WVA_DEFAULT_SO:-false}"
    [ "$mode" != "false" ] || return 0

    local plan="${WVA_DEFAULT_SO_PLAN:-}"

    # An existing plan file is authoritative: this is the "edit the list and
    # continue" path, and it works with no terminal, which is what makes the
    # interactive capability available to scripts and CI as well.
    #
    # Except in `plan` mode, which promises to apply nothing. Without that
    # exception, running `make scaledobjects-plan WVA_DEFAULT_SO_PLAN=<file>` a
    # second time — the obvious thing to do after editing the file, and what a
    # scripted caller does on every run — APPLIED it. A command whose entire
    # contract is "look, do not touch" created autoscaling objects instead.
    if [ "$mode" != "plan" ] && [ -n "$plan" ] && [ -f "$plan" ]; then
        log_info "Applying the ScaledObject plan from $plan"
        so_show_plan "$plan"
        so_apply_plan "$plan"
        return 0
    fi

    [ -n "$plan" ] || plan=$(mktemp -t wva-scaledobject-plan.XXXXXX.yaml)
    log_info "Scanning for llm-d model servers..."
    so_discover > "$plan"

    if ! so_plan_rows "$plan" | grep -q .; then
        log_warning "No llm-d model servers found (label llm-d.ai/inferenceServing=true) in: $(so_target_namespaces | tr '\n' ' '). Deploy them first, then run 'make scaledobjects-apply'. Until a ScaledObject exists, WVA is never called and scales nothing."
        return 0
    fi

    so_show_plan "$plan"

    case "$mode" in
        plan)
            log_success "Plan written to $plan — nothing applied."
            log_info "Edit it — apply: yes/no/adopt, the modelID, the replica bounds — then:"
            log_info "    make scaledobjects-apply WVA_DEFAULT_SO_PLAN=$plan"
            log_info "Every field it takes is explained in the comments at the top of that file."
            return 0
            ;;
        edit)
            if [ ! -t 0 ]; then
                log_error "WVA_DEFAULT_SO=edit needs a terminal. Use WVA_DEFAULT_SO=plan, edit the file it writes, then apply it with WVA_DEFAULT_SO_PLAN=<file>."
            fi
            log_info "Opening the plan in ${EDITOR:-vi}. Set each apply: to yes, no or adopt; the comments at the top explain every field."
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

# so_adopt_patch prints the merge patch that repoints an existing ScaledObject at
# WVA: its triggers, and the bounds and cost the plan carried. Everything else on that
# object — polling interval, cooldown, fallback, advanced behavior, its labels —
# stays as whoever tuned it left it.
#
# `triggers` is a list, so a merge patch REPLACES it wholesale, which is what is
# wanted. An object scaled by a prometheus or cpu trigger must stop being scaled by
# it, or two scalers feed one HPA and the larger answer silently wins.
so_adopt_patch() {
    local model="$1" scaler_addr="$2" min="$3" max="$4" cost="$5" policy="${6:-}"
    # scalingPolicy is added only when the entry names one: an empty value would
    # be a metadata key whose value says "the default", which is what its absence
    # already says, and it would survive in the object long after anybody knew why.
    jq -nc --arg m "$model" --arg a "$scaler_addr" --arg c "$cost" --arg p "$policy" \
        --argjson min "$min" --argjson max "$max" \
        '{spec:{minReplicaCount:$min, maxReplicaCount:$max,
          triggers:[{type:"external-push",name:"wva-external-scaler",
          metadata:({scalerAddress:$a, modelID:$m, variantCost:$c}
                    + (if $p == "" then {} else {scalingPolicy:$p} end))}]}}'
}

# render_scaledobject prints one ScaledObject: the shipped shape, or yours.
#
# WVA_DEFAULT_SO_TEMPLATE=<file> substitutes your own template instead, so a fleet
# with house conventions — fallback policy, stabilization windows, labels its
# tooling expects — gets those rather than a shape it then has to edit back.
# Placeholders, all optional:
#
#   {{NAMESPACE}} {{NAME}} {{KIND}} {{APIVERSION}} {{MODEL_ID}}
#   {{SCALER_ADDRESS}} {{MIN}} {{MAX}} {{VARIANT_COST}}
#
# Substitution is literal, so a template is also just a valid manifest with the
# placeholders written in — you can `kubectl apply` it by hand to check the shape
# before letting the installer fill it in for every model server you have.
render_scaledobject() {
    local ns="$1" kind="$2" target="$3" model="$4" scaler_addr="$5" min="$6" max="$7" cost="${8:-}"
    local policy="${9:-}"
    local api="apps/v1"

    # The last gate before a manifest exists. The caller checks this too, and the
    # caller's check was once defeated by a field-splitting bug that shifted a
    # replica count into this argument — so the check that matters is the one next
    # to the thing being built, where no amount of upstream plumbing can skip it.
    [ -n "$model" ] || log_error "refusing to build a ScaledObject for $ns/$target with an empty modelID"
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
            -e "s|{{VARIANT_COST}}|${cost:-$SO_DEFAULT_VARIANT_COST}|g" \
            -e "s|{{SCALING_POLICY}}|${policy}|g" \
            "$tmpl"
        return
    fi
    render_default_scaledobject "$ns" "$kind" "$target" "$model" "$scaler_addr" "$min" "$max" "$cost" "$policy"
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
    local ns="$1" kind="$2" target="$3" model="$4" scaler_addr="$5" min="$6" max="$7" cost="${8:-}"
    local policy="${9:-}"
    local api="apps/v1"
    [ "$kind" = "LeaderWorkerSet" ] && api="leaderworkerset.x-k8s.io/v1"

    # Written only when the entry names a tier — an absent key and an empty one
    # mean the same thing to WVA, and only one of them says so to a reader.
    local policy_line=""
    [ -z "$policy" ] || policy_line=$'\n        scalingPolicy: "'"$policy"'"'

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
        modelID: "${model}"
        variantCost: "${cost:-$SO_DEFAULT_VARIANT_COST}"${policy_line}
EOF
}
