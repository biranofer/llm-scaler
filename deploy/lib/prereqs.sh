#!/usr/bin/env bash
#
# Prerequisite checks for deploy/install.sh.
# Requires vars: REQUIRED_TOOLS.
# Requires funcs: log_info/log_success/log_error.
#

# Minimum versions. Only tools whose minimum actually matters are listed; a tool
# absent from here is checked for presence alone, because inventing a floor
# nobody has verified turns a working install into a refused one.
readonly MIN_KUBECTL_VERSION="1.24"
readonly MIN_HELM_VERSION="3.8"

# version_at_least compares dotted versions without sort -V, which BSD/macOS
# lacks in the form this needs. Compares major.minor only: patch levels have
# never been the thing that breaks a deploy here.
version_at_least() {
    local have="$1" want="$2"
    local have_major="${have%%.*}" want_major="${want%%.*}"
    local have_rest="${have#*.}" want_rest="${want#*.}"
    local have_minor="${have_rest%%.*}" want_minor="${want_rest%%.*}"
    # Non-numeric (a dev build, an unparsed string) is treated as new enough:
    # refusing to install on a version we could not read is the worse failure.
    [[ "$have_major$have_minor$want_major$want_minor" =~ ^[0-9]+$ ]] || return 0
    (( have_major > want_major )) && return 0
    (( have_major < want_major )) && return 1
    (( have_minor >= want_minor ))
}

# tool_version prints a tool's major.minor, or nothing if it cannot be read.
tool_version() {
    local tool="$1" raw=""
    case "$tool" in
        kubectl) raw=$(kubectl version --client 2>/dev/null | grep -oE 'v[0-9]+\.[0-9]+' | head -1) ;;
        helm)    raw=$(helm version --short 2>/dev/null | grep -oE 'v[0-9]+\.[0-9]+' | head -1) ;;
        yq)      raw=$(yq --version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+' | head -1) ;;
        *)       return 0 ;;
    esac
    echo "${raw#v}"
}

# append_conditional_tools adds, to the caller's `tools` array, what this
# particular invocation needs on top of REQUIRED_TOOLS. They are conditional
# because requiring them always would refuse installs that never touch them —
# and, worse, NOT checking them at all lets an install fail halfway, after it has
# already created namespaces and RBAC.
#
# It appends to a caller-declared array rather than echoing into mapfile, because
# mapfile is bash 4+ and macOS still ships bash 3.2.
append_conditional_tools() {
    # yq edits the shipped ConfigMaps in place. Needed only when something is
    # actually patched: a declared limiter, or enabling scale-to-zero.
    if [ "${WVA_LIMITER:-none}" != "none" ] || [ "${ENABLE_SCALE_TO_ZERO:-true}" = "true" ]; then
        tools+=("yq")
    fi
    if [ "${ENVIRONMENT:-kubernetes}" = "openshift" ]; then
        tools+=("oc")
    fi
}

check_prerequisites() {
    log_info "Checking prerequisites..."

    local missing_tools=() outdated_tools=()
    local tools=("${REQUIRED_TOOLS[@]}")
    append_conditional_tools

    for tool in "${tools[@]}"; do
        [ -n "$tool" ] || continue
        if ! command -v "$tool" &> /dev/null; then
            missing_tools+=("$tool")
            continue
        fi
        local have want=""
        have=$(tool_version "$tool")
        case "$tool" in
            kubectl) want="$MIN_KUBECTL_VERSION" ;;
            helm)    want="$MIN_HELM_VERSION" ;;
        esac
        if [ -n "$want" ] && [ -n "$have" ] && ! version_at_least "$have" "$want"; then
            outdated_tools+=("$tool $have (need >= $want)")
        fi
    done

    # Keep deploy paths deterministic: fail fast instead of prompting for installs.
    if [ ${#missing_tools[@]} -ne 0 ]; then
        log_error "Missing required tools: ${missing_tools[*]}. Install them on PATH, or set SKIP_CHECKS=true to bypass this check (not recommended)."
    fi
    if [ ${#outdated_tools[@]} -ne 0 ]; then
        log_error "Tools too old: ${outdated_tools[*]}. Upgrade them, or set SKIP_CHECKS=true to bypass this check (not recommended)."
    fi

    # A reachable cluster is as much a prerequisite as a binary, and finding out
    # here beats finding out after the first namespace has been created.
    if ! kubectl cluster-info &> /dev/null; then
        log_error "Cannot reach a Kubernetes cluster with the current context ($(kubectl config current-context 2>/dev/null || echo 'none set')). Check your kubeconfig, or set SKIP_CHECKS=true to bypass this check."
    fi

    log_success "All prerequisites met (tools: ${tools[*]})"
}

# check_permissions verifies the caller can create what the install produces, and
# that the controller will be able to read what its configuration requires.
#
# `make check-prereqs` used to answer only "are the binaries here", and passed for a
# user who could not create a single object the install makes. A preflight that
# cannot fail on the most likely failure is not a preflight.
#
# The node rule is the interesting one, and it is conditional by design:
#
#   physical limiter ON   nodes are REQUIRED. A variant whose accelerator cannot be
#                         resolved is charged to no pool, receives no GPU budget and
#                         NEVER SCALES UP — silently, because nothing errors. An
#                         install that cannot read nodes is therefore broken, and
#                         this is an error.
#   physical limiter OFF  nodes are not read at all (see variantmeta.DeclaredOnly).
#                         The install is fine; the operator only loses accelerator
#                         attribution on metrics. A warning, not a refusal — and it
#                         says what changes if the limiter is turned on later.
check_permissions() {
    log_info "Checking permissions..."

    local ns="${WVA_NS}" denied=() sa="system:serviceaccount:${WVA_NS}:wva-controller-manager"

    # What the INSTALLER must be able to create. Cluster-scoped objects are in the
    # base at both scopes, so both scopes need them.
    local -a cluster_checks=(
        "create clusterroles"
        "create clusterrolebindings"
    )
    local check verb res
    for check in "${cluster_checks[@]}"; do
        verb="${check%% *}"; res="${check#* }"
        kubectl auth can-i "$verb" "$res" >/dev/null 2>&1 || denied+=("$verb $res (cluster-scoped)")
    done
    kubectl auth can-i create deployments -n "$ns" >/dev/null 2>&1 || denied+=("create deployments in $ns")
    kubectl auth can-i create configmaps -n "$ns" >/dev/null 2>&1 || denied+=("create configmaps in $ns")

    if [ ${#denied[@]} -ne 0 ]; then
        log_error "You cannot create everything this install produces. Denied:
$(printf '  - %s\n' "${denied[@]}")

Both install scopes create cluster-scoped RBAC — WVA_SCOPE=namespace narrows what
the CONTROLLER reads, not what it is granted — so both need a cluster admin.
Ask for the objects above, or have an admin run the install."
    fi

    # What the CONTROLLER will need once it is running.
    if [ "${WVA_LIMITER:-none}" = "gpu-inventory" ]; then
        if ! kubectl auth can-i list nodes --as "$sa" >/dev/null 2>&1; then
            log_error "WVA_LIMITER=gpu-inventory needs the controller to list nodes, and $sa cannot.

A GPU-aware limiter allocates out of per-accelerator pools. Without nodes, a
variant's accelerator never resolves, it is charged to no pool, it receives no
budget, and it NEVER SCALES UP — with nothing in the logs to say why.

Either grant the node read, or install without the limiter (WVA_LIMITER=none)."
        fi
        log_success "The controller can list nodes, which the gpu-inventory limiter requires"
    else
        if ! kubectl auth can-i list nodes --as "$sa" >/dev/null 2>&1; then
            log_warning "$sa cannot list nodes. That is FINE with no physical limiter — WVA does not read them (accelerators stay unresolved, which is permissive), and you lose only the accelerator label on metrics."
            log_warning "  It stops being fine the moment someone sets a gpu-inventory limiter: every variant would then get no GPU budget and stop scaling up. Watch wva_node_access_denied."
        fi
    fi

    log_success "Permissions look sufficient"
}
