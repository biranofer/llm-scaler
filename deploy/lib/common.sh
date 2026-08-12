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
#   cluster     manages every namespace. Creates ClusterRoles and
#               ClusterRoleBindings, so it needs a cluster admin.
#   namespace   manages ONE namespace and creates NO cluster-scoped object, so a
#               NAMESPACE ADMIN can install it themselves. It gives up what
#               genuinely requires cluster scope: the gpu-inventory limiter
#               (reads nodes), authenticated metrics (TokenReview), and EPP
#               metrics (nonResourceURLs).
#
# Namespace scope used to create the same cluster-scoped RBAC as cluster scope,
# which made it blast-radius reduction and nothing more — a tenant still could not
# run it. Now the name means what it says.
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

# wva_scope_is_tenant reports whether this install creates NO cluster-scoped
# objects — i.e. whether a namespace admin could have run it themselves.
wva_scope_is_tenant() {
    [ "$(wva_install_scope)" = "namespace" ]
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
    # Created only under WVA_ADMIN_GRANTS=true. Listed unconditionally anyway:
    # the rename patch is a no-op when the object is absent, and leaving it out
    # meant two admin-granted installs shared one binding — the second apply
    # replaced its subject list, so the first controller lost node access and its
    # readiness gate turned that into a NotReady pod.
    wva-node-reader-rolebinding
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
    local kustomization="$1" ns="$2" suffix crb
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
