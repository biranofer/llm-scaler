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
    # yq and jq edit the shipped ConfigMaps in place, and the undeploy filters the
    # rendered overlay with yq. Both are effectively unconditional: the install
    # always patches PROMETHEUS_BASE_URL, so patch_manager_config always runs.
    # They were listed as conditional, which let the preflight pass and the install
    # then die after namespaces, RBAC and the Deployment already existed.
    tools+=("yq" "jq")
    # column formats the ScaledObject plan.
    if [ "${WVA_DEFAULT_SO:-false}" != "false" ]; then
        tools+=("column")
    fi
    # openssl mints the self-signed cert for a Prometheus this install deploys.
    if [ "${DEPLOY_PROMETHEUS:-true}" = "true" ]; then
        tools+=("openssl")
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
#   physical limiter OFF  the install still works, degraded: accelerators stay
#                         unresolved, so metrics lose the accelerator label and the
#                         capacity model cannot reuse learned capacity. A warning,
#                         not a refusal — and it says what changes if the limiter is
#                         turned on later.
# can_i_as answers a permission question ABOUT ANOTHER SUBJECT, distinguishing
# "no" from "could not ask".
#
# `kubectl auth can-i --as` impersonates, which the CALLER must be allowed to do.
# A cluster admin can; plenty of legitimate installers cannot. Both cases exit
# non-zero, so treating non-zero as "denied" turned "I lack impersonate" into
# "the controller cannot read nodes" — a hard, wrong refusal to install, and one
# that gets worse the less privileged the user is.
#
# Echoes: yes | no | unknown
can_i_as() {
    local subject="$1"; shift
    local out rc
    out=$(kubectl auth can-i "$@" --as "$subject" 2>&1)
    rc=$?
    case "$out" in
        yes*) echo yes ;;
        no*)  [ $rc -ne 0 ] && echo no || echo unknown ;;
        *)    echo unknown ;;
    esac
}

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
    # Every namespaced kind the overlay creates. Checking a subset let an install
    # pass preflight and then die partway through — after the namespace, the RBAC
    # and the ServiceAccount already existed, which is the state that is annoying
    # to clean up by hand.
    local -a ns_kinds=(deployments configmaps serviceaccounts services)
    for res in "${ns_kinds[@]}"; do
        kubectl auth can-i create "$res" -n "$ns" >/dev/null 2>&1 || denied+=("create $res in $ns")
    done
    # The namespace itself, only when it does not exist yet — requiring namespace
    # creation from someone installing into a namespace an admin already made for
    # them would refuse a perfectly good install.
    if ! kubectl get namespace "$ns" >/dev/null 2>&1; then
        kubectl auth can-i create namespaces >/dev/null 2>&1 || denied+=("create namespace $ns (it does not exist yet)")
    fi

    if [ ${#denied[@]} -ne 0 ]; then
        log_error "You cannot create everything this install produces. Denied:
$(printf '  - %s\n' "${denied[@]}")

Both install scopes create cluster-scoped RBAC — WVA_SCOPE=namespace narrows what
the CONTROLLER reads, not what it is granted — so both need a cluster admin.
Ask for the objects above, or have an admin run the install."
    fi

    # What the CONTROLLER will need once it is running.
    local node_read
    node_read="$(can_i_as "$sa" list nodes)"
    if [ "${WVA_LIMITER:-none}" = "gpu-inventory" ]; then
        case "$node_read" in
            yes)
                log_success "The controller can list nodes, which the gpu-inventory limiter requires"
                ;;
            no)
                log_error "WVA_LIMITER=gpu-inventory needs the controller to list nodes, and $sa cannot.

A GPU-aware limiter allocates out of per-accelerator pools. Without nodes, a
variant's accelerator never resolves, it is charged to no pool, it receives no
budget, and it NEVER SCALES UP — with nothing in the logs to say why.

Either grant the node read, or install without the limiter (WVA_LIMITER=none)."
                ;;
            *)
                # Cannot impersonate, so this is unanswerable from here. It is not a
                # reason to refuse: the install itself grants the node read, and the
                # controller publishes wva_node_access_denied if it turns out not to
                # have it. Say what to watch instead of guessing.
                log_warning "Could not verify that $sa may list nodes (checking needs impersonate permission, which you do not have)."
                log_warning "  WVA_LIMITER=gpu-inventory requires it. After the install, confirm with: kubectl get --raw /metrics | grep wva_node_access_denied — 1 means every variant is getting no GPU budget and will not scale up."
                ;;
        esac
    else
        case "$node_read" in
            no)
                log_warning "$sa cannot list nodes. Without a physical limiter this is survivable but degraded: accelerators stay unresolved, so metrics lose the accelerator label and the capacity model cannot reuse learned capacity across variants."
                log_warning "  It stops being fine the moment someone sets a gpu-inventory limiter: every variant would then get no GPU budget and stop scaling up. Watch wva_node_access_denied."
                ;;
            unknown)
                log_info "Skipped the controller's node-read check (needs impersonate permission). No limiter is configured, so nothing depends on it yet."
                ;;
        esac
    fi

    log_success "Permissions look sufficient"
}
