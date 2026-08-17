#!/usr/bin/env bash
#
# Common logging and utility helpers for deploy/install.sh.
# Requires vars: BLUE, GREEN, YELLOW, RED, NC.
# Exposes funcs: log_info/log_warning/log_success/log_error,
# containsElement(), should_skip_helm_repo_update().
#

log_info() {
    echo -e "${BLUE}[INFO]${NC} $1" >&2
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1" >&2
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1" >&2
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1" >&2
    exit 1
}

# Helm repo update behavior:
# - Default: DO NOT skip (`helm repo update` runs)
# - Opt-in: set `SKIP_HELM_REPO_UPDATE=true` to skip (faster, but requires repo indexes to already exist)
should_skip_helm_repo_update() {
    local skip="${SKIP_HELM_REPO_UPDATE:-false}"
    echo "$skip"
}

# Used to check if the environment variable is in a list
containsElement() {
    local e match="$1"
    shift
    for e; do [[ "$e" == "$match" ]] && return 0; done
    return 1
}

# wva_install_scope echoes the install scope: "cluster" or "namespace".
#
# The two differ in WHO CAN INSTALL as much as in what the controller reads:
#
#   cluster     manages every namespace. Its manager ClusterRole reads every
#               namespace, and nodes.
#   namespace   manages ONE namespace. Its manager role is a namespaced Role, so
#               it reads nothing outside — but it still carries cluster-scoped
#               RBAC for the things no Role can express: the gpu-inventory
#               limiter (nodes), authenticated metrics (TokenReview) and EPP
#               metrics (nonResourceURLs).
#
# Neither scope is installable by a namespace admin in one command, and pretending
# otherwise was the bug: the PHASE split is what makes a tenant install real. An
# admin runs the prereqs phase once for a namespace; its owner installs and
# upgrades the controller from then on.
#
# WVA_SCOPE selects it; the default preserves the historical inference.
wva_install_scope() {
    local scope="${WVA_SCOPE:-}"
    if [ -z "$scope" ]; then
        if [ "${ENVIRONMENT:-}" = "openshift" ]; then
            scope="namespace"
        else
            scope="cluster"
        fi
    fi
    case "$scope" in
        cluster|namespace) echo "$scope" ;;
        *) log_error "WVA_SCOPE must be 'cluster' or 'namespace', got '$scope'" ;;
    esac
}

# wva_scope_is_tenant reports whether this install manages ONE namespace.
#
# It says nothing about what the install creates. That is a question about the
# RENDERED overlay — ask wva_rendered_kinds, never this. Both scopes carry
# cluster-scoped RBAC; what makes a namespace-scoped install a tenant's is that
# an admin created that RBAC in the prereqs phase, not the scope name.
wva_scope_is_tenant() {
    [ "$(wva_install_scope)" = "namespace" ]
}

# wva_scaler_namespaces echoes every namespace running a WVA external-scaler
# Service, one per line -- that is, every install a KEDA trigger could name.
#
# The Service is the right thing to look for rather than the Deployment: the
# address in a ScaledObject resolves to a Service, so a Service is what makes an
# address answerable, and one without a ready controller behind it is a
# different fault with a different fix.
#
# Empty output means "none found OR not allowed to look", and the two are
# different problems. Callers that act on the answer must say which they assumed.
wva_scaler_namespaces() {
    local out
    if out=$(kubectl get svc -A -l app.kubernetes.io/name=workload-variant-autoscaler \
        --field-selector metadata.name=wva-external-scaler \
        -o jsonpath='{range .items[*]}{.metadata.namespace}{"\n"}{end}' 2>/dev/null); then
        printf '%s\n' "$out" | grep -v '^[[:space:]]*$' | sort -u
        return 0
    fi
    # No cluster-wide list. The one namespace we can name is still worth checking.
    kubectl get svc wva-external-scaler -n "$WVA_NS" > /dev/null 2>&1 && echo "$WVA_NS"
    return 0
}

# wva_any_pod_ready reports whether any pod matching selector $2 in namespace $1
# is READY. Pass -A for cluster-wide.
#
# `kubectl get pods | grep -q Running` is not this check, and the difference is
# the one that matters when something is wrong: a pod stuck at "0/1 Running" --
# crash-looping, or failing a probe -- matches Running and is not ready. On a
# multi-pod list it is worse, because it answers yes when ANY line says Running,
# including one belonging to a replica that is fine while the one you care about
# is not.
#
# The Ready CONDITION is what "is it up" means, so that is what this reads. A
# failed call returns non-zero rather than "not ready": being unable to look is
# not the same as looking and finding nothing, and a caller that gates on this
# must be able to tell them apart.
wva_any_pod_ready() {
    local scope="$1" selector="$2" out
    local path='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}'
    if [ "$scope" = "-A" ]; then
        out=$(kubectl get pods -A -l "$selector" -o jsonpath="$path" 2>/dev/null) || return 2
    else
        out=$(kubectl get pods -n "$scope" -l "$selector" -o jsonpath="$path" 2>/dev/null) || return 2
    fi
    printf '%s\n' "$out" | grep -qx 'True'
}

# wva_pods_exist reports whether any pod matches selector $2 in namespace $1
# (-A for cluster-wide), regardless of readiness.
#
# Pairs with wva_any_pod_ready to separate "it is here and unhealthy" from "it is
# not here". Only the first justifies telling someone their cluster is broken;
# the second usually means we looked in the wrong place.
wva_pods_exist() {
    local scope="$1" selector="$2" out
    if [ "$scope" = "-A" ]; then
        out=$(kubectl get pods -A -l "$selector" -o name 2>/dev/null) || return 2
    else
        out=$(kubectl get pods -n "$scope" -l "$selector" -o name 2>/dev/null) || return 2
    fi
    [ -n "$(printf '%s' "$out" | tr -d '[:space:]')" ]
}

# wva_scaler_has_endpoints reports whether the external-scaler Service in $1 has a
# ready backend -- a controller actually listening, not merely an address that
# resolves.
#
# A Service with no Endpoints is the shape a half-removed install leaves behind,
# and it is indistinguishable from a live one by name alone. Anything CHOOSING
# between installs must use this; anything deciding whether to leave another
# install's object alone must not, because there the Service existing at all is
# reason enough to keep hands off.
#
# EndpointSlice is read first (Endpoints is deprecated and unserved in newer
# clusters) with Endpoints as the fallback, so this works either side of that
# change rather than silently reporting "no backend" on every Service.
wva_scaler_has_endpoints() {
    local ns="$1" out
    if out=$(kubectl get endpointslice -n "$ns" -l kubernetes.io/service-name=wva-external-scaler \
        -o jsonpath='{range .items[*]}{range .endpoints[*]}{.addresses[0]}{"\n"}{end}{end}' 2>/dev/null); then
        [ -z "$(printf '%s' "$out" | tr -d '[:space:]')" ] || return 0
    fi
    out=$(kubectl get endpoints wva-external-scaler -n "$ns" \
        -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null) || return 1
    [ -n "$(printf '%s' "$out" | tr -d '[:space:]')" ]
}

# WVA_SHARED_CLUSTER_ROLE_BINDINGS are the cluster-scoped bindings every overlay
# applies under fixed names. They are renamed per install so two installs cannot
# take them from one another.
WVA_SHARED_CLUSTER_ROLE_BINDINGS=(
    wva-epp-metrics-reader-role-binding
    wva-manager-cluster-monitoring-view
    wva-manager-rolebinding
    wva-metrics-auth-rolebinding
    wva-metrics-reader-rolebinding
    wva-prometheus-cluster-monitoring-view
    # Namespace scope only (the cluster-scoped manager role already reads nodes).
    # Listed unconditionally anyway: the rename patch is a no-op when the object
    # is absent, and leaving it out meant two installs shared one binding — the
    # second apply replaced its subject list, so the first controller lost node
    # access and its readiness gate turned that into a NotReady pod.
    wva-node-reader-rolebinding
)

# WVA_OWNED_CLUSTER_ROLES are the ClusterRoles this project defines, mapped to the
# binding that references each. They are renamed per install for the same reason
# the bindings are, and it took a shared cluster to make the reason concrete:
# pokprod001 had ten WVA installs sharing four ClusterRoles under fixed names.
# Identical rules make an apply a no-op, so nothing had gone wrong yet — but an
# install carrying different rules would have rewritten the permissions of ten
# other controllers, cluster-wide, with no error and no restart to notice.
#
# Only OUR roles. `cluster-monitoring-view` is OpenShift's own: its bindings are
# renamed, its roleRef must not be.
#
# Kustomize's name-reference transformer would repoint roleRefs automatically for
# a nameSuffix, but nameSuffix renames every resource — including the
# external-scaler Service that every ScaledObject trigger addresses by name. So
# the roleRef is repointed explicitly, alongside the rename.
WVA_OWNED_CLUSTER_ROLES=(
    "wva-manager-role:wva-manager-rolebinding"
    "wva-metrics-auth-role:wva-metrics-auth-rolebinding"
    "wva-metrics-reader:wva-metrics-reader-rolebinding"
    "wva-epp-metrics-reader-role:wva-epp-metrics-reader-role-binding"
    # Namespace scope only; the patches are no-ops when the object is absent.
    # The role is node-reader-ROLE — the binding drops the suffix, the role does
    # not, and getting that wrong leaves the role unrenamed and the binding
    # pointing at a name nothing creates.
    "wva-node-reader-role:wva-node-reader-rolebinding"
)

# wva_ns_suffix echoes the per-namespace suffix appended to those names.
# sha256sum is GNU; macOS ships shasum. The suffix must be IDENTICAL on whatever
# host installs and whatever host uninstalls — a different suffix means the
# uninstall leaks every ClusterRoleBinding it was meant to remove — so this picks
# one implementation deterministically rather than depending on the box.
wva_ns_suffix() {
    if command -v sha256sum >/dev/null 2>&1; then
        printf '%s' "$1" | sha256sum | cut -c1-8
    else
        printf '%s' "$1" | shasum -a 256 | cut -c1-8
    fi
}

# wva_append_crb_name_patches appends the rename patches to a kustomization file.
#
# Deploy AND undeploy must both use this. They diverged once, with real cost: the
# install renamed the bindings and the uninstall did not, so `kubectl delete -k`
# resolved the FIXED names and removed whichever install currently owned them —
# an uninstall of one WVA stripping a different, healthy one of its permissions —
# while leaking its own suffixed bindings. One definition, used by both.
wva_append_crb_name_patches() {
    local kustomization="$1" ns="$2" suffix crb pair role binding
    suffix="$(wva_ns_suffix "$ns")"
    printf 'patches:\n' >> "$kustomization"
    for crb in "${WVA_SHARED_CLUSTER_ROLE_BINDINGS[@]}"; do
        cat >> "$kustomization" <<EOF
- patch: |-
    - op: replace
      path: /metadata/name
      value: ${crb}-${suffix}
  target:
    kind: ClusterRoleBinding
    name: ${crb}
EOF
    done
    # The roles, and the roleRef of the binding that names each. Both, or the
    # binding points at a ClusterRole that no longer exists under that name and
    # the controller silently has no permissions at all.
    for pair in "${WVA_OWNED_CLUSTER_ROLES[@]}"; do
        role="${pair%%:*}"; binding="${pair#*:}"
        cat >> "$kustomization" <<EOF
- patch: |-
    - op: replace
      path: /metadata/name
      value: ${role}-${suffix}
  target:
    kind: ClusterRole
    name: ${role}
- patch: |-
    - op: replace
      path: /roleRef/name
      value: ${role}-${suffix}
  target:
    kind: ClusterRoleBinding
    name: ${binding}
EOF
    done
    # The OpenShift ServiceMonitor pins the serving cert's name with
    # tlsConfig.serverName, and that is a plain STRING: `namespace:` rewrites
    # namespace fields, not a namespace spelled inside a value. Left alone it
    # keeps naming the overlay's default namespace, the service-ca SAN never
    # matches, and Prometheus rejects every scrape with
    #
    #   http: TLS handshake error ...: remote error: tls: bad certificate
    #
    # once per scrape interval, forever. Nothing else fails: the controller is
    # healthy and idle, so the only symptom is that no wva_* series ever appear
    # — which also silently empties any benchmark report built from them. Found
    # by installing into a namespace that is not the overlay default and reading
    # the controller log.
    #
    # It belongs here rather than in the component because only this layer knows
    # the namespace: a component's replacement runs before the outer
    # `namespace:` transform, when the Service still has no namespace at all.
    if [ "${ENVIRONMENT:-}" = "openshift" ]; then
        cat >> "$kustomization" <<EOF
- patch: |-
    - op: replace
      path: /spec/endpoints/0/tlsConfig/serverName
      value: wva-controller-manager-metrics-service.${ns}.svc
  target:
    kind: ServiceMonitor
EOF
    fi
}

# wva_overlay_dir echoes the absolute Kustomize overlay directory for the selected
# scope and platform. Deploy and cleanup MUST resolve it the same way, or an
# uninstall leaves behind exactly the resources the other overlay owns.
wva_overlay_dir() {
    local scope platform
    scope="$(wva_install_scope)"
    if [ "${ENVIRONMENT:-}" = "openshift" ]; then
        platform="openshift"
    else
        platform="kubernetes"
    fi
    local dir="$WVA_PROJECT/config/overlays/${scope}-scoped/${platform}"
    [ -d "$dir" ] || log_error "No overlay for scope '${scope}' on '${platform}' (looked in $dir)"
    (cd "$dir" && pwd)
}

# The kinds a CLUSTER ADMIN owns, and the phase split built on them.
#
# Everything here either is cluster-scoped or needs a permission a namespace admin
# does not have with the stock `admin` ClusterRole. Splitting the install on this
# line is what makes a tenant install real rather than aspirational: an admin runs
# the prereqs phase once for a namespace, and whoever owns that namespace can then
# install and upgrade the controller without holding any cluster-scoped right.
#
#   Namespace           cluster-scoped to create.
#   ClusterRole/Binding cluster-scoped, obviously.
#   ServiceMonitor      namespaced, but the stock `admin` ClusterRole does NOT
#                       grant monitoring.coreos.com — verified on a real cluster,
#                       where it was one of two denials standing between a
#                       namespace admin and a working "self-service" install.
#   Role/RoleBinding    namespaced, and still not the tenant's to create: RBAC
#                       forbids granting permissions you do not hold yourself
#                       (privilege-escalation prevention), and the controller's
#                       Role names resources the stock `admin` ClusterRole does
#                       not carry. A tenant applying it gets "attempting to grant
#                       RBAC permissions not currently held", the Role is skipped,
#                       its RoleBinding then fails with "not found", and the
#                       controller comes up unable to read a ConfigMap in its own
#                       namespace — a CrashLoopBackOff two removes from its cause.
#                       Verified with a token-scoped kubeconfig; it cannot be
#                       fixed by granting more to the tenant without granting them
#                       everything the controller has.
# ServiceAccount and Secret are here for the ServiceMonitor's sake, and the
# ordering is the whole point.
#
# The ServiceMonitor authenticates with `bearerTokenSecret:
# wva-controller-manager-token`. The prometheus-operator resolves that Secret when
# it first sees the ServiceMonitor, and if the Secret is absent it REJECTS the
# object -- `reason=InvalidConfiguration`, "unable to get secret" -- and does not
# retry when the Secret later appears. Nothing re-evaluates it: a metadata write
# is not enough, and re-running this phase applies identical content, so
# `kubectl apply` reports "unchanged" and the operator never looks again.
#
# With the Secret in the CONTROLLER phase, the two-phase install therefore created
# the ServiceMonitor 33 minutes before the Secret it needs, and WVA's own metrics
# were permanently uncollected -- no `up` series, no `wva_*` series -- while every
# install step reported success. Measured on pokprod001: the install said
# "All components verified successfully!" and Thanos had nothing.
#
# The single-command install (INSTALL_PHASE=all) was unaffected, because it applies
# everything at once and the render orders Secret before ServiceMonitor. That is
# why this survived: it is the two-person split, the shape the guides recommend,
# that breaks. The two namespaces on that cluster running this code with working
# metrics both had their Secret created 3 seconds BEFORE their ServiceMonitor.
#
# Both kinds belong to an admin anyway -- they are namespaced, and creating them
# needs no rights a namespace owner lacks, but they are part of the same
# monitoring wiring the ServiceMonitor is. Moving them here keeps the three
# objects that reference each other in one phase, applied in render order, which
# puts ServiceAccount and Secret ahead of ServiceMonitor.
#
# This list also drives the phase-2 gate (wva_rendered_prereq_objects), so the
# controller phase now checks for them and names them if an admin's phase 1
# predates this change.
WVA_PREREQ_KINDS=(Namespace ClusterRole ClusterRoleBinding ServiceMonitor Role RoleBinding ServiceAccount Secret)

# wva_prereq_kind_filter echoes a yq expression selecting (or with $1=exclude,
# rejecting) the admin-owned kinds. One definition, so the prereqs phase and the
# controller phase cannot disagree about where the line is and leave a kind that
# neither applies.
wva_prereq_kind_filter() {
    local mode="${1:-select}" k expr=""
    for k in "${WVA_PREREQ_KINDS[@]}"; do
        [ -n "$expr" ] && expr="$expr or "
        expr="${expr}.kind == \"$k\""
    done
    if [ "$mode" = "exclude" ]; then
        echo "select(($expr) | not)"
    else
        echo "select($expr)"
    fi
}

# wva_resolve_namespace points a namespace-scoped install at NAMESPACE when the
# caller named no WVA_NS.
#
# Here rather than in the Makefile because every entry point needs it — install,
# undeploy and check — and `wva_install_scope` is the only thing that knows the
# scope once WVA_SCOPE is empty and the platform default applies. The make-side
# version reached only the 12 phase targets, so `NAMESPACE=x make
# undeploy-wva-on-k8s` resolved to the DEFAULT namespace while the install it was
# meant to undo had gone to x.
#
# Cluster scope is untouched: that controller manages every namespace and belongs
# in its own, not inside whichever one runs llm-d.
# wva_bootstrap_env gives a caller that sources these libraries directly — rather
# than going through install.sh — the same namespace install.sh would have used.
#
# The targets that plan and apply ScaledObjects do exactly that, so none of
# install.sh's preamble runs for them: they saw WVA_NS's default and ignored
# `export NAMESPACE=…` entirely. The plan then scanned the wrong namespace and
# found nothing, or — worse, because it looks like it worked — wrote every trigger
# with a scaler address in a namespace where no scaler runs. KEDA reports that as
# a failing trigger on a healthy-looking install, which reads as "WVA is broken"
# rather than "WVA was handed the wrong address".
#
# ${VAR-…} without the colon, so an explicitly empty value set by install.sh is
# left alone: a nested call must not re-capture a WVA_NS that a default filled in.
wva_bootstrap_env() {
    WVA_NS_EXPLICIT=${WVA_NS_EXPLICIT-${WVA_NS:-}}
    NAMESPACE_EXPLICIT=${NAMESPACE_EXPLICIT-${NAMESPACE:-}}
    export WVA_NS_EXPLICIT NAMESPACE_EXPLICIT
    WVA_NS=${WVA_NS:-workload-variant-autoscaler-system}
    export WVA_NS
    wva_resolve_namespace
}

wva_resolve_namespace() {
    WVA_NS_SOURCE=default
    export WVA_NS_SOURCE
    [ -z "${WVA_NS_EXPLICIT:-}" ] || { WVA_NS_SOURCE=explicit; return 0; }
    [ -n "${NAMESPACE_EXPLICIT:-}" ] || return 0   # stays "default": discovery may still fill it
    [ "$(wva_install_scope)" = "namespace" ] || return 0
    WVA_NS="$NAMESPACE_EXPLICIT"
    WVA_NS_SOURCE=namespace-env
    export WVA_NS WVA_NS_SOURCE
}

# wva_prepare_overlay_base populates $1/base with the overlay this install will
# apply.
#
# The preflight, the deploy and the undeploy all go through here. They must: a
# preflight that infers the shape instead of reading it is how a namespace-scoped
# OpenShift install passed `--check` and then failed partway through
# `kubectl apply -k`, and an undeploy that built the base differently left
# cluster-scoped objects behind after reporting a clean removal.
#
# It used to re-render the overlay under WVA_ADMIN_GRANTS, dropping the
# unauthenticated-metrics component and adding node-reader, so that one command
# could serve both a cluster admin and a tenant. The phase split removed the
# question: everything cluster-scoped belongs to the prereqs phase, which is the
# admin's by definition, so the overlay is now the same for both and this is a
# symlink.
wva_prepare_overlay_base() {
    local tmp_overlay="$1" kustomize_overlay
    kustomize_overlay="$(wva_overlay_dir)"
    # Symlinked so kustomization.yaml can reference it with a relative path —
    # Kustomize rejects absolute paths in resources.
    ln -s "$kustomize_overlay" "$tmp_overlay/base"
}

# wva_repair_immutable_rolerefs deletes this install's ClusterRoleBindings whose
# roleRef no longer matches what will be applied, so the apply can recreate them.
#
# `roleRef` is IMMUTABLE. An upgrade that changes which ClusterRole a binding
# points at therefore fails with
#
#   ClusterRoleBinding ... is invalid: roleRef: ... cannot change roleRef
#
# and the install stops halfway — which is exactly what happened the first time
# the ClusterRoles were given per-namespace names, on a cluster with an install
# already on it. Found by running the upgrade, not by reading it.
#
# It only ever deletes a binding whose name matches what THIS install renders,
# i.e. one already carrying this namespace's suffix. Another install's binding
# has a different suffix and cannot be selected here. The controller is without
# that binding for the moment between the delete and the apply below.
wva_repair_immutable_rolerefs() {
    local name want have
    while read -r name want; do
        [ -n "$name" ] && [ -n "$want" ] || continue
        have="$(kubectl get clusterrolebinding "$name" -o jsonpath='{.roleRef.name}' 2>/dev/null || true)"
        [ -n "$have" ] || continue          # absent: the apply just creates it
        [ "$have" = "$want" ] && continue   # already right
        log_info "Recreating ClusterRoleBinding $name — roleRef is immutable and must change ($have -> $want)"
        kubectl delete clusterrolebinding "$name" --ignore-not-found >/dev/null 2>&1 || true
    done < <(wva_render_manifests \
        | yq 'select(.kind == "ClusterRoleBinding") | .metadata.name + " " + .roleRef.name' 2>/dev/null \
        | grep -v '^null' || true)
}

# wva_render_manifests prints the manifests this install would apply, with the
# namespace transform and the per-namespace ClusterRoleBinding renames applied —
# so the NAMES it prints are the names that will exist on the cluster. The image
# pin is not applied: no caller of this asks about the image.
#
# Empty output means the overlay could not be rendered. Callers must treat that as
# "unknown", never as "nothing".
wva_render_manifests() {
    local tmp ns="${WVA_NS:-wva-system}"
    tmp="$(mktemp -d)" || return 0
    if wva_prepare_overlay_base "$tmp" >/dev/null 2>&1; then
        printf 'namespace: %s\nresources:\n- ./base\n' "$ns" > "$tmp/kustomization.yaml"
        wva_append_crb_name_patches "$tmp/kustomization.yaml" "$ns"
        # `|| true` because the callers run under `set -e` with pipefail, and a
        # render that fails is the case they exist to handle: it must reach them
        # as empty output, not as the whole install script exiting.
        kubectl kustomize "$tmp" 2>/dev/null || true
    fi
    rm -rf "$tmp"
}

# wva_rendered_kinds echoes, one per line, each distinct Kind this install would
# create.
wva_rendered_kinds() {
    # Only column 0 — `kind:` also appears indented under roleRef, and with a
    # leading dash under subjects.
    wva_render_manifests | awk '/^kind: /{print $2}' | sort -u || true
}

# wva_launcher_scrapers echoes the name of every PodMonitor in $1 that would
# actually generate a scrape target for an FMA launcher pod. $2, if given, is a
# PodMonitor name to ignore.
#
# "Would generate a target" is the whole point, and it is NOT the same as
# "selects". llm-d's own vllm-<model> PodMonitor selects a launcher the moment it
# binds -- the serving labels are stamped on it then -- but names its endpoint by
# port NAME, and a launcher declares no container ports, so the operator resolves
# nothing and produces no target. A caller asking "is anything scraping these?"
# would get the wrong answer from a selector test, in both directions: it would
# report scraping where there is none, and refuse to add scraping where it is
# needed.
#
# So a monitor counts only if it could reach the pod:
#   - it sets targetPort (a number bypasses the named-port lookup), or
#   - it rewrites __address__ in a relabeling, or
#   - it names a port that a launcher pod actually declares.
#
# Selectors are resolved by asking the API server rather than by comparing label
# maps, so a monitor that reaches launchers by any combination is caught.
# matchExpressions are not evaluated -- kubectl takes no expression selector -- so
# an expression-only monitor can slip through. This is a guard for the realistic
# case, not a proof, and both callers treat it as advisory.
#
# Echoes nothing when there are no launcher pods: with nothing to resolve against,
# "no monitor reaches them" is the only answer that can be defended.
wva_launcher_scrapers() {
    local ns="$1" exclude="${2:-}" launchers launcher_ports pm sel rewrites ports matched p
    launchers=$(kubectl get pods -n "$ns" -l app.kubernetes.io/component=launcher \
        -o name 2>/dev/null)
    [ -n "$launchers" ] || return 0

    launcher_ports=$(kubectl get pods -n "$ns" -l app.kubernetes.io/component=launcher \
        -o jsonpath='{range .items[*].spec.containers[*].ports[*]}{.name}{"\n"}{end}' 2>/dev/null \
        | grep -v '^$' | sort -u)

    kubectl get podmonitor -n "$ns" -o json 2>/dev/null \
      | jq -r --arg ex "$exclude" '
          .items[]
          | select($ex == "" or .metadata.name != $ex)
          | [ .metadata.name,
              ((.spec.selector.matchLabels // {}) | to_entries
                | map("\(.key)=\(.value)") | join(",")),
              ( [ (.spec.podMetricsEndpoints // [])[]
                  | (.targetPort != null)
                    or ((.relabelings // []) | any(.targetLabel == "__address__")) ]
                | any ),
              ( [ (.spec.podMetricsEndpoints // [])[] | .port // empty ] | join(" ") ) ]
          | @tsv' 2>/dev/null \
      | while IFS=$'\t' read -r pm sel rewrites ports; do
            [ -n "$sel" ] || continue
            matched=$(kubectl get pods -n "$ns" -l "$sel" -o name 2>/dev/null)
            [ -n "$matched" ] || continue
            printf '%s\n' "$matched" | grep -Fxq -f <(printf '%s\n' "$launchers") || continue
            if [ "$rewrites" = "true" ]; then
                printf '%s\n' "$pm"
                continue
            fi
            for p in $ports; do
                if printf '%s\n' "$launcher_ports" | grep -Fxq -- "$p"; then
                    printf '%s\n' "$pm"
                    break
                fi
            done
        done
}

# wva_rendered_prereq_objects echoes "<Kind> <name>" for every admin-owned object
# this install needs, at the names it will really have.
wva_rendered_prereq_objects() {
    # yq writes a `---` between documents even when each renders to one scalar.
    wva_render_manifests \
        | yq "$(wva_prereq_kind_filter select) | .kind + \" \" + .metadata.name" 2>/dev/null \
        | grep -Ev '^(---|null)' || true
}
