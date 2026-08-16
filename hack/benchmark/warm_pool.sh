#!/usr/bin/env bash
# Manage the FMA warm pool: the set of sleeping vLLM instances a scale-up can
# wake instead of rebuilding.
#
# WHY THIS EXISTS, AND WHY IT TAKES REPLICAS
# -----------------------------------------
# FMA reuses a sleeping instance only on an exact instance-ID match, and that ID
# is a hash that INCLUDES the GPU UUIDs (pkg/controller/dual-pods). So a sleeper
# is reusable only by a requester that reserved that same GPU. Two consequences:
#
#   * Sleepers made by the launcher-populator are keyed to GPUs it picked, which
#     no requester will reserve except by chance. They are never woken. Measured:
#     every such bind rebuilt from scratch, ~50s of model load.
#   * Sleepers made by scaling requesters UP then DOWN are keyed to GPUs the
#     scheduler actually hands out, so a later scale-up hits. Measured: 3s.
#
# The durable size of that pool is LauncherPopulationPolicy.launcherCount, which
# is PER NODE -- the populator prints `node=... desired=N` per node key and
# deletes anything above it as "excess launcher pod", warm instances included.
# That is what makes warmth fragile: exceed the count and the populator reaps
# your warm capacity ~20s after a scale-down.
#
# This script takes the pool size in REPLICAS, not per-node counts, and owns the
# translation itself. That is deliberate. The size is the one input that may
# later come from somewhere else -- WVA already computes desired and peak
# replicas -- and a future producer should not need node topology or free-GPU
# accounting to say "keep 6 warm". Placement stays infra config; warming stays a
# separate step; only sizing changes hands. Keep it that way.
#
# Usage:
#   warm_pool.sh size <replicas>   set durable capacity to hold <replicas> warm
#   warm_pool.sh verify [replicas] gate: are that many actually asleep and ready?
#   warm_pool.sh report            what the pool looks like right now
#
# Env: WARM_POOL_NS (default $BENCHMARK_NAMESPACE), KUBECONFIG as usual.
set -u

NS="${WARM_POOL_NS:-${BENCHMARK_NAMESPACE:-}}"
[ -n "$NS" ] || { echo "warm_pool: set WARM_POOL_NS or BENCHMARK_NAMESPACE" >&2; exit 2; }

SLEEP_LABEL='dual-pods.llm-d.ai/sleeping'
LAUNCHER_SEL='app.kubernetes.io/component=launcher'

policy_name() {
    kubectl get launcherpopulationpolicy -n "$NS" \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

# Nodes the policy is allowed to place launchers on. An empty hostname list
# means "any node matching the label selector", in which case we cannot know the
# count without listing nodes -- do that only then, so the common path is cheap.
policy_nodes() {
    local n
    n=$(kubectl get launcherpopulationpolicy -n "$NS" -o json 2>/dev/null | python3 -c '
import json, sys
try:
    pol = json.load(sys.stdin)["items"][0]
except Exception:
    sys.exit(0)
sel = (pol["spec"].get("enhancedNodeSelector") or {}).get("labelSelector") or {}
for e in sel.get("matchExpressions") or []:
    if e.get("key") == "kubernetes.io/hostname" and e.get("operator") == "In":
        print(len(e.get("values") or []))
        sys.exit(0)
' 2>/dev/null)
    if [ -n "${n:-}" ] && [ "$n" -gt 0 ] 2>/dev/null; then
        echo "$n"; return
    fi
    kubectl get nodes -l nvidia.com/gpu.present=true --no-headers 2>/dev/null | grep -c . || echo 0
}

# Sleepers that are actually usable: labelled sleeping AND Running AND Ready.
# A pod that is sleeping-but-not-ready is not warm capacity, it is a pod that
# happens to be idle.
ready_sleepers() {
    kubectl get pods -n "$NS" -l "$LAUNCHER_SEL" -o json 2>/dev/null | python3 -c '
import json, sys
n = 0
for p in json.load(sys.stdin).get("items", []):
    lbl = (p["metadata"].get("labels") or {}).get("dual-pods.llm-d.ai/sleeping")
    if lbl != "true":
        continue
    if p["status"].get("phase") != "Running":
        continue
    if any(c["type"] == "Ready" and c["status"] == "True"
           for c in p["status"].get("conditions", [])):
        n += 1
print(n)
' 2>/dev/null || echo 0
}

cmd_size() {
    local want="${1:?usage: warm_pool.sh size <replicas>}"
    local pol nodes per
    pol=$(policy_name)
    [ -n "$pol" ] || { echo "warm_pool: no LauncherPopulationPolicy in $NS" >&2; exit 1; }
    nodes=$(policy_nodes)
    [ "${nodes:-0}" -gt 0 ] 2>/dev/null || { echo "warm_pool: no candidate nodes" >&2; exit 1; }

    # launcherCount is per node, so round UP: a pool one slot short of the
    # ceiling means the last scale-up step rebuilds cold, and a run is only as
    # fast as its slowest replica.
    per=$(( (want + nodes - 1) / nodes ))
    [ "$per" -ge 1 ] || per=1

    echo "  policy=$pol nodes=$nodes want=${want} replicas -> launcherCount=$per per node (capacity $((per * nodes)))"
    kubectl patch launcherpopulationpolicy "$pol" -n "$NS" --type=merge \
        -p "{\"spec\":{\"countForLauncher\":[{\"launcherConfigName\":\"$(kubectl get launcherpopulationpolicy "$pol" -n "$NS" -o jsonpath='{.spec.countForLauncher[0].launcherConfigName}' 2>/dev/null)\",\"launcherCount\":$per}]}}" >/dev/null \
        && echo "  patched" || { echo "  patch FAILED" >&2; exit 1; }
}

cmd_verify() {
    local want="${1:-}" have nodes
    have=$(ready_sleepers)
    if [ -z "$want" ]; then
        echo "  sleeping + Running + Ready launchers: $have"
        return 0
    fi
    echo "  want >= $want warm, have $have"
    if [ "$have" -ge "$want" ]; then
        echo "  OK"
        return 0
    fi
    echo "  NOT WARM: a scale-up will rebuild instead of waking (~50s per replica)" >&2
    return 1
}

cmd_report() {
    echo "  --- launchers ---"
    kubectl get pods -n "$NS" -l "$LAUNCHER_SEL" \
        -o custom-columns="NAME:.metadata.name,NODE:.spec.nodeName,SLEEPING:.metadata.labels.dual-pods\.llm-d\.ai/sleeping,BOUND:.metadata.labels.dual-pods\.llm-d\.ai/dual" \
        --no-headers 2>/dev/null | sed 's/^/    /'
    echo "  --- instances (GPU is the reuse key) ---"
    for p in $(kubectl get pods -n "$NS" -l "$LAUNCHER_SEL" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
        kubectl exec -n "$NS" "$p" -c inference-server -- \
            curl -s --max-time 6 http://localhost:8001/v2/vllm/instances 2>/dev/null \
        | POD="$p" python3 -c '
import json, os, sys
raw = sys.stdin.read().strip()
pod = os.environ["POD"][-6:]
if not raw:
    print("    %-8s <no answer>" % pod); sys.exit()
try:
    d = json.loads(raw)
except Exception:
    print("    %-8s <unparseable>" % pod); sys.exit()
inst = d.get("instances") or []
if not inst:
    print("    %-8s <no instances>" % pod)
for i in inst:
    print("    %-8s inst=%-15s gpu=%s" % (
        pod, (i.get("instance_id") or "")[:14], (i.get("gpu_uuids") or ["-"])[0][:22]))
' 2>/dev/null
    done
    echo "  --- capacity ---"
    echo "    policy nodes: $(policy_nodes)   launcherCount: $(kubectl get launcherpopulationpolicy -n "$NS" -o jsonpath='{.items[0].spec.countForLauncher[0].launcherCount}' 2>/dev/null)"
    echo "    warm now:     $(ready_sleepers)"
}

case "${1:-}" in
    size)   shift; cmd_size "$@" ;;
    verify) shift; cmd_verify "$@" ;;
    report) shift; cmd_report "$@" ;;
    *) sed -n '/^# Usage:/,/^# Env:/p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
