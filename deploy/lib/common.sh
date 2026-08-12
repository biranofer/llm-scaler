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

# wva_scope_is_tenant reports whether this install manages ONE namespace.
#
# It says nothing about whether a namespace admin can install it. That is a
# question about the RENDERED overlay, and the answer differs by platform: on
# Kubernetes the namespace-scoped overlay creates no cluster-scoped object, but
# on OpenShift it creates eight — the platform's monitoring wiring is
# cluster-scoped, and components/tenant-installable does not subtract it.
# Ask wva_rendered_kinds, never this.
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
    # WVA_ADMIN_GRANTS only; the patches are no-ops when the objects are absent.
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
#                       where it was the single denial standing between a
#                       namespace admin and a working "self-service" install.
WVA_PREREQ_KINDS=(Namespace ClusterRole ClusterRoleBinding ServiceMonitor)

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

# wva_admin_grants answers whether this namespace-scoped install keeps the
# cluster-scoped pieces: authenticated metrics (TokenReview) and the node read the
# gpu-inventory limiter needs.
#
# Unset means DETECT, because the question is not what the operator meant, it is
# what this installer can actually create. It defaulted to false, so a cluster
# admin running the ordinary command on Kubernetes silently got the self-service
# shape — metrics served over plain HTTP — and nothing said so. Detecting instead
# means the flag is only needed to turn the capability OFF.
#
# The detection is one-directional and cannot loosen anything: it can only enable
# the extra objects for someone who may create them anyway. A tenant is answered
# "no" and gets the shape that installs.
wva_admin_grants() {
    case "${WVA_ADMIN_GRANTS:-}" in
        true)  echo true;  return ;;
        false) echo false; return ;;
    esac
    if kubectl auth can-i create clusterroles >/dev/null 2>&1 \
        && kubectl auth can-i create clusterrolebindings >/dev/null 2>&1; then
        echo true
    else
        echo false
    fi
}

# wva_prepare_overlay_base populates $1/base with the overlay this install will
# apply, WVA_ADMIN_GRANTS included.
#
# The preflight and the deploy both go through here. They must, for the same
# reason wva_append_crb_name_patches is shared: a preflight that infers the shape
# instead of reading it is how a namespace-scoped OpenShift install passed
# `--check` and then failed partway through `kubectl apply -k`, after the
# namespace, the RBAC and the ServiceAccount already existed.
wva_prepare_overlay_base() {
    local tmp_overlay="$1" kustomize_overlay
    kustomize_overlay="$(wva_overlay_dir)"

    # Symlink the base overlay so kustomization.yaml can reference it with a
    # relative path — Kustomize rejects absolute paths in resources.
    ln -s "$kustomize_overlay" "$tmp_overlay/base"

    # WVA_ADMIN_GRANTS: this namespace-scoped install is being made BY a cluster
    # admin, or by someone an admin has granted the cluster-scoped pieces to.
    #
    # Namespace scope defaults to the self-service shape on Kubernetes — no
    # cluster-scoped object at all — because that is what makes it installable by
    # a namespace admin. But the two limitations that shape carries are
    # limitations of the INSTALLER, not of the scope: authenticated metrics need
    # TokenReview and the gpu-inventory limiter needs nodes, both cluster-scoped
    # APIs. An admin installing the very same overlay has both and should not
    # lose them.
    [ "$(wva_install_scope)" = "namespace" ] || return 0
    [ "$(wva_admin_grants)" = "true" ] || return 0

    # Re-render the overlay without the unauthenticated-metrics component. The
    # overlay lists it, so it is dropped here rather than added there — the
    # default has to be the shape a tenant can actually install.
    local rendered="$tmp_overlay/admin-base"
    mkdir -p "$rendered"
    # Kustomize rejects ABSOLUTE paths in resources and components, so the copied
    # overlay's `../../../` references are rehomed onto a relative symlink rather
    # than expanded to a real path.
    ln -s "$WVA_PROJECT/config" "$rendered/config"
    grep -v 'components/unauthenticated-metrics' "$kustomize_overlay/kustomization.yaml" \
        | sed 's#\.\./\.\./\.\./#./config/#g' > "$rendered/kustomization.yaml"
    # And the node read, which no Role can provide — a Node is cluster-scoped.
    # Without it a gpu-inventory limiter resolves no accelerator, and the
    # controller's readiness gate now refuses to serve that state at all.
    printf -- '- ./config/components/node-reader/\n' >> "$rendered/kustomization.yaml"
    rm -f "$tmp_overlay/base"
    ln -s "$rendered" "$tmp_overlay/base"
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

# wva_rendered_prereq_objects echoes "<Kind> <name>" for every admin-owned object
# this install needs, at the names it will really have.
wva_rendered_prereq_objects() {
    # yq writes a `---` between documents even when each renders to one scalar.
    wva_render_manifests \
        | yq "$(wva_prereq_kind_filter select) | .kind + \" \" + .metadata.name" 2>/dev/null \
        | grep -Ev '^(---|null)' || true
}
