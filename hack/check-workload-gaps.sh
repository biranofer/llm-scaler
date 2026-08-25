#!/usr/bin/env bash
#
# Executes the workload-readiness path in deploy/lib/scaledobject.sh against
# fixture pod specs.
#
# It exists because `bash -n` is the only check these libraries had, and every
# defect this file pins down parsed cleanly: an engine chosen by name regex that
# landed on a routing proxy, a preStop check that counted hooks pod-wide, a
# "weights persist" test satisfied by a PVC mounted anywhere, a local-path test
# that matched `/bin/sh`, and an unreadable object reported as healthy. None of
# them are syntax errors and all of them silently produce the wrong answer about
# a running workload.
#
# kubectl is stubbed: each case is a pod spec on stdin, so the assertions are
# about what the functions CONCLUDE, not about a cluster.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Sourced through a CR-stripped copy. These files are LF in git, but a Windows
# checkout with core.autocrlf writes them CRLF, and bash then reports
# `$'\r': command not found` and stops at the first function definition -- which
# looks exactly like every function being missing. Same reason the repo verifies
# gofmt against an LF-normalised copy rather than the working tree.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
for lib in common.sh scaledobject.sh; do
    tr -d '\r' < "$ROOT/deploy/lib/$lib" > "$WORK/$lib"
done
# common.sh's header says it plainly: "Requires vars: BLUE, GREEN, YELLOW, RED,
# NC." install.sh defines them; a harness that sources the library directly has
# to as well, or the first log_warning dies on an unbound variable under `set -u`
# -- which inside a command substitution kills only the subshell, so it surfaces
# as one test silently returning nothing rather than as an error.
BLUE='' GREEN='' YELLOW='' RED='' NC=''
# shellcheck source=/dev/null
source "$WORK/common.sh"
# shellcheck source=/dev/null
source "$WORK/scaledobject.sh"

FIXTURE=""      # the JSON `kubectl get` returns
FIXTURE_RC=0    # or the failure it returns instead

# Stubbed in place of the real client. `command kubectl` is never reached.
kubectl() {
    case "$1" in
        get)
            [ "$FIXTURE_RC" -eq 0 ] || return "$FIXTURE_RC"
            printf '%s' "$FIXTURE"
            ;;
        *) return 0 ;;
    esac
}

POD_DEPLOY='["spec","template"]'
FAILED=0
CASE=""

fail() {
    printf 'FAIL  %s\n      %s\n' "$CASE" "$1" >&2
    FAILED=$((FAILED + 1))
}

ok() { printf 'ok    %s\n' "$CASE"; }

# assert_field checks one \037-separated field of so_workload_gaps by index.
assert_field() {
    local idx="$1" want="$2" gaps got
    gaps="$(so_workload_gaps ns deployments w "$POD_DEPLOY")" || {
        fail "so_workload_gaps returned non-zero"
        return
    }
    got="$(printf '%s' "$gaps" | cut -d$'\037' -f"$idx")"
    [ "$got" = "$want" ] || fail "field $idx: want '$want', got '$got'"
}

assert_contains() {
    case "$1" in
        *"$2"*) : ;;
        *) fail "expected to contain: $2" ;;
    esac
}

assert_not_contains() {
    case "$1" in
        *"$2"*) fail "expected NOT to contain: $2" ;;
        *) : ;;
    esac
}

# --- fixtures ---------------------------------------------------------------
#
# Shapes taken from what llm-d actually deploys, not invented: the modelservice
# chart's `/bin/sh -c "vllm serve ..."` form, and the proxy+server pair an
# SGLang deployment produces.

f_vllm_plain() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
}

f_sglang_proxy_has_hook() {
    # The engine is called `server` and the container that HAS a preStop hook is
    # the proxy. A name regex picks the proxy; counting hooks pod-wide reports
    # this pod as already draining.
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "containers":[
          {"name":"routing-proxy","image":"quay.io/x/proxy:1",
           "lifecycle":{"preStop":{"exec":{"command":["sleep","10"]}}}},
          {"name":"server","image":"lmsysorg/sglang:latest",
           "args":["python3","-m","sglang.launch_server","--model-path","meta-llama/Llama-3.1-8B"]}]}}}}'
}

f_mount_without_hf_home() {
    # A PVC is mounted and the engine still downloads into the image layer.
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "volumes":[{"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"}],
                       "command":["/bin/sh","-c"],
                       "args":["vllm serve Qwen/Qwen3-0.6B --port 8000"]}]}}}}'
}

f_cached_properly() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":120,
        "volumes":[{"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}}],
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "env":[{"name":"HF_HOME","value":"/model-cache/huggingface"}],
                       "volumeMounts":[{"name":"model-storage","mountPath":"/model-cache"}],
                       "lifecycle":{"preStop":{"exec":{"command":["sleep","45"]}}},
                       "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
}

f_local_path() {
    FIXTURE='{"spec":{"template":{"spec":{
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "command":["/bin/sh","-c"],
                       "args":["vllm serve /models/qwen3-0.6b"]}]}}}}'
}

f_long_grace() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":300,
        "containers":[{"name":"vllm","image":"quay.io/x/vllm-openai:v0.11",
                       "args":["vllm","serve","Qwen/Qwen3-0.6B"]}]}}}}'
}

f_no_engine() {
    FIXTURE='{"spec":{"template":{"spec":{
        "containers":[{"name":"sidecar","image":"quay.io/x/proxy:1","args":["--listen",":80"]}]}}}}'
}

# Copied from a running llm-d deployment on an OpenShift cluster
# (qwen-...-decode, modelservice chart, uriProtocol: pvc). Two things about it
# broke assumptions the invented fixtures could not:
#   * the command is a multi-line block scalar, so a `read` on an argv field that
#     still contains newlines is truncated at the first one;
#   * the weights are served FROM the volume by local path, while
#     --served-model-name advertises a repository id -- so the advertised name
#     says "downloads from the Hub" and the source says "already on disk".
f_llmd_real() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "volumes":[{"name":"model-storage","persistentVolumeClaim":{"claimName":"model-pvc"}},
                   {"name":"dshm"},{"name":"shared-config"}],
        "containers":[{"name":"vllm","image":"docker.io/vllm/vllm-openai:v0.26.0",
                       "env":[{"name":"HF_HOME","value":"/tmp/huggingface"}],
                       "volumeMounts":[{"name":"dshm","mountPath":"/dev/shm"},
                                       {"name":"shared-config","mountPath":"/shared-config"},
                                       {"name":"model-storage","mountPath":"/model-cache"}],
                       "command":["/bin/bash","-c"],
                       "args":["/bin/true ; vllm serve /model-cache/models/Qwen/Qwen3-0.6B \\\n  --host 0.0.0.0 \\\n  --served-model-name Qwen/Qwen3-0.6B \\\n  --port 8000\n"]}]}}}}'
}

# The same block-scalar shape, but genuinely fetching from the Hub: the source is
# a repository id and HF_HOME points outside every mount.
f_llmd_real_downloads() {
    FIXTURE='{"spec":{"template":{"spec":{
        "terminationGracePeriodSeconds":30,
        "containers":[{"name":"vllm","image":"docker.io/vllm/vllm-openai:v0.26.0",
                       "env":[{"name":"HF_HOME","value":"/tmp/huggingface"}],
                       "command":["/bin/bash","-c"],
                       "args":["/bin/true ; vllm serve Qwen/Qwen3-0.6B \\\n  --host 0.0.0.0 \\\n  --served-model-name my-alias \\\n  --port 8000\n"]}]}}}}'
}

f_unreadable() { FIXTURE=""; FIXTURE_RC=1; }

reset() { FIXTURE=""; FIXTURE_RC=0; }

# --- detection --------------------------------------------------------------

CASE="plain vLLM: engine, no hook, default grace, nothing cached"
reset; f_vllm_plain
assert_field 1 "vllm"
assert_field 2 "0"
assert_field 3 "30"
assert_field 5 "0"
[ "$FAILED" -eq 0 ] && ok

CASE="SGLang: engine is 'server' by image, not the proxy that has the hook"
reset; f_sglang_proxy_has_hook
before=$FAILED
assert_field 1 "server"
# The engine's own hook count, which is what decides whether streams survive.
assert_field 2 "0"
[ "$FAILED" -eq "$before" ] && ok

CASE="a PVC mounted with no HF_HOME does not make weights persist"
reset; f_mount_without_hf_home
before=$FAILED
assert_field 4 "1"      # a PVC is present...
assert_field 5 "0"      # ...and the engine still downloads outside it
[ "$FAILED" -eq "$before" ] && ok

CASE="HF_HOME under a mounted volume counts as cached"
reset; f_cached_properly
before=$FAILED
assert_field 5 "1"
assert_field 2 "1"
[ "$FAILED" -eq "$before" ] && ok

CASE="unreadable object returns non-zero and prints nothing"
reset; f_unreadable
before=$FAILED
out="$(so_workload_gaps ns deployments w "$POD_DEPLOY")" && fail "expected non-zero"
[ -z "$out" ] || fail "expected no output, got: $out"
[ "$FAILED" -eq "$before" ] && ok

CASE="a multi-line block scalar keeps its later flags (read stops at newline 1)"
reset; f_llmd_real
before=$FAILED
gaps="$(so_workload_gaps ns deployments w "$POD_DEPLOY")" || fail "returned non-zero"
argv="$(printf '%s' "$gaps" | cut -d$'\037' -f6)"
assert_contains "$argv" "--served-model-name"
assert_contains "$argv" "--port"
[ "$FAILED" -eq "$before" ] && ok

CASE="weights served BY LOCAL PATH need no cache, whatever name is advertised"
reset; f_llmd_real
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
# The drain half is still owed -- this deployment has no preStop hook.
assert_contains "$doc" "preStop"
# ...but mounting a second cache into a pod already reading from one is the
# false positive that --served-model-name produces.
assert_not_contains "$doc" "claimName"
assert_not_contains "$doc" "HF_HOME"
[ "$FAILED" -eq "$before" ] && ok

CASE="the same shape genuinely downloading from the Hub does get a cache"
reset; f_llmd_real_downloads
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "claimName"
assert_contains "$doc" "HF_HOME"
[ "$FAILED" -eq "$before" ] && ok

# --- notes ------------------------------------------------------------------

CASE="drain note fires for the proxy-has-a-hook pod"
reset; f_sglang_proxy_has_hook
before=$FAILED
note="$(so_drain_note ns deployments w "$POD_DEPLOY")"
assert_contains "$note" "server declares no preStop hook"
[ "$FAILED" -eq "$before" ] && ok

CASE="drain note is silent when the engine has a hook"
reset; f_cached_properly
before=$FAILED
note="$(so_drain_note ns deployments w "$POD_DEPLOY")"
[ -z "$note" ] || fail "expected silence, got: $note"
[ "$FAILED" -eq "$before" ] && ok

CASE="weights note fires for the sh -c form (used to match on /bin/sh)"
reset; f_mount_without_hf_home
before=$FAILED
note="$(so_weights_note ns deployments w "$POD_DEPLOY")"
assert_contains "$note" "Weights do not persist"
[ "$FAILED" -eq "$before" ] && ok

CASE="weights note is silent for a local model path"
reset; f_local_path
before=$FAILED
note="$(so_weights_note ns deployments w "$POD_DEPLOY")"
[ -z "$note" ] || fail "expected silence, got: $note"
[ "$FAILED" -eq "$before" ] && ok

CASE="an unreadable object is reported as unknown, never as healthy"
reset; f_unreadable
before=$FAILED
note="$(so_drain_note ns deployments w "$POD_DEPLOY")"
assert_contains "$note" "unknown"
assert_not_contains "$note" "no preStop hook"
[ "$FAILED" -eq "$before" ] && ok

# --- emitted patch ----------------------------------------------------------

CASE="patch names the engine container, both halves, HF_HOME with the mount"
reset; f_mount_without_hf_home
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "- name: vllm"
assert_contains "$doc" "preStop"
assert_contains "$doc" "HF_HOME"
assert_contains "$doc" "claimName: model-pvc"
[ "$FAILED" -eq "$before" ] && ok

CASE="patch never lowers an existing longer grace period"
reset; f_long_grace
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_contains "$doc" "terminationGracePeriodSeconds: 300"
[ "$FAILED" -eq "$before" ] && ok

CASE="patch emits no cache half for a local model path"
reset; f_local_path
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
assert_not_contains "$doc" "claimName"
assert_not_contains "$doc" "HF_HOME"
[ "$FAILED" -eq "$before" ] && ok

CASE="nothing is emitted for a workload that is already fine"
reset; f_cached_properly
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
[ -z "$doc" ] || fail "expected no document, got: $doc"
[ "$FAILED" -eq "$before" ] && ok

CASE="no identifiable engine is skipped rather than patched at container 0"
reset; f_no_engine
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
[ -z "$doc" ] || fail "expected no document, got: $doc"
[ "$FAILED" -eq "$before" ] && ok

CASE="LeaderWorkerSet is skipped, not patched with a Deployment-shaped document"
reset; f_vllm_plain
before=$FAILED
doc="$(so_workload_patch_doc ns leaderworkersets w '["spec","leaderWorkerTemplate","leaderTemplate"]' 2>/dev/null)"
[ -z "$doc" ] || fail "expected no document, got: $doc"
[ "$FAILED" -eq "$before" ] && ok

CASE="an unreadable object emits nothing"
reset; f_unreadable
before=$FAILED
doc="$(so_workload_patch_doc ns deployments w "$POD_DEPLOY" 2>/dev/null)"
[ -z "$doc" ] || fail "expected no document, got: $doc"
[ "$FAILED" -eq "$before" ] && ok

# --- the patch index --------------------------------------------------------

CASE="the index lists EVERY document, not the first"
before=$FAILED
if command -v yq >/dev/null 2>&1; then
    multi="$(mktemp)"
    cat > "$multi" <<'YAML'
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: a
  namespace: ns1
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: b
  namespace: ns2
YAML
    lines="$(so_workload_patch_index "$multi" | wc -l | tr -d ' ')"
    [ "$lines" = "2" ] || fail "want 2 index lines, got $lines (yq eval-all collapses the stream onto one)"
    rm -f "$multi"
    [ "$FAILED" -eq "$before" ] && ok
else
    printf 'skip  %s (yq not installed)\n' "$CASE"
fi

# --- dropping the weights half ----------------------------------------------
#
# A cluster with no `model-pvc` is the common case, and refusing the whole
# document there would skip the drain fix too -- including in benchmark-standup,
# where draining is the entire reason the patch runs.

CASE="a missing claim drops the weights half and keeps the drain half"
before=$FAILED
if command -v yq >/dev/null 2>&1; then
    d="$(mktemp)"
    cat > "$d" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: w
  namespace: ns1
spec:
  template:
    spec:
      terminationGracePeriodSeconds: 120
      containers:
      - name: vllm
        lifecycle:
          preStop:
            exec:
              command: ["/bin/sh", "-c", "sleep 45"]
        env:
        - name: HF_HOME
          value: /model-cache/huggingface
        volumeMounts:
        - name: model-storage
          mountPath: /model-cache
      volumes:
      - name: model-storage
        persistentVolumeClaim:
          claimName: model-pvc
YAML
    so_workload_patch_drop_cache "$d" || fail "expected the drain half to survive"
    left="$(cat "$d")"
    assert_contains "$left" "terminationGracePeriodSeconds: 120"
    assert_contains "$left" "preStop"
    assert_not_contains "$left" "claimName"
    assert_not_contains "$left" "HF_HOME"
    assert_not_contains "$left" "volumeMounts"
    rm -f "$d"
    [ "$FAILED" -eq "$before" ] && ok

    CASE="a cache-only document is skipped, not applied as an empty patch"
    before=$FAILED
    d="$(mktemp)"
    cat > "$d" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: w
  namespace: ns1
spec:
  template:
    spec:
      containers:
      - name: vllm
        env:
        - name: HF_HOME
          value: /model-cache/huggingface
      volumes:
      - name: model-storage
        persistentVolumeClaim:
          claimName: model-pvc
YAML
    so_workload_patch_drop_cache "$d" && fail "expected non-zero: nothing actionable is left"
    rm -f "$d"
    [ "$FAILED" -eq "$before" ] && ok
else
    printf 'skip  %s (yq not installed)\n' "$CASE"
fi

# --- the driver -------------------------------------------------------------
#
# wva_workload_patch's return code is load-bearing: benchmark-standup runs it as
# `... || echo WARNING`, and the previous version returned 0 on every path, so
# the warning could not fire for the one failure it named.

DRIVER_MODE=""

driver_kubectl() {
    # $1=get $2=<resource|crd> ...
    case "$2" in
        crd) return 1 ;;                     # no LWS CRD, so that arm is skipped
    esac
    case "$DRIVER_MODE" in
        listfail) return 1 ;;
        onegap)
            # A listing is `get deployments -n ns -o json`; a single read has the
            # name in $3.
            if [ "$3" = "-n" ]; then
                printf '%s' '{"items":[{"metadata":{"name":"w","labels":{"llm-d.ai/role":"decode"}}}]}'
            else
                f_vllm_plain; printf '%s' "$FIXTURE"
            fi
            ;;
    esac
}

run_driver() {
    DRIVER_MODE="$1"
    local saved_out
    saved_out="$(mktemp -d)/patch.yaml"
    kubectl() { driver_kubectl "$@"; }
    WVA_DEFAULT_SO_NS=ns1 WVA_WORKLOAD_PATCH_FILE="$saved_out" wva_workload_patch >/dev/null 2>&1
    DRIVER_RC=$?
    DRIVER_OUT="$saved_out"
    # Restore the fixture stub for anything after this.
    kubectl() {
        case "$1" in
            get)
                [ "$FIXTURE_RC" -eq 0 ] || return "$FIXTURE_RC"
                printf '%s' "$FIXTURE"
                ;;
            *) return 0 ;;
        esac
    }
}

CASE="a namespace that cannot be listed returns non-zero, not 'all clean'"
before=$FAILED
run_driver listfail
[ "$DRIVER_RC" -ne 0 ] || fail "want non-zero so the caller's warning can fire, got 0"
[ -f "$DRIVER_OUT" ] && fail "wrote a patch file for a namespace it could not read"
[ "$FAILED" -eq "$before" ] && ok

CASE="one workload with gaps is written, and the run succeeds"
before=$FAILED
run_driver onegap
[ "$DRIVER_RC" -eq 0 ] || fail "want 0, got $DRIVER_RC"
[ -s "$DRIVER_OUT" ] || fail "expected a patch file"
assert_contains "$(cat "$DRIVER_OUT" 2>/dev/null)" "name: w"
[ "$FAILED" -eq "$before" ] && ok

# ----------------------------------------------------------------------------

if [ "$FAILED" -gt 0 ]; then
    printf '\n%d workload-readiness check(s) failed.\n' "$FAILED" >&2
    exit 1
fi
printf '\nWorkload readiness checks passed.\n'
