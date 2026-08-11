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
