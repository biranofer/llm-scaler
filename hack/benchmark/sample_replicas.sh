#!/usr/bin/env bash
# Sample serving replica counts for the duration of a benchmark run.
#
# The harness already writes metrics/processed/replica_status_timeseries.json,
# and on every FMA run measured it came back with snapshots but no controllers:
# collect_metrics.sh filters them with
#
#     model_filter="${LLMDBENCH_HARNESS_STACK_NAME:-}"
#     if model_filter and model != model_filter: continue
#
# comparing a STACK name against the llm-d.ai/model LABEL. Ours are
# "inference-scheduling-wva" and "qwen-qwe-...", which never match, so every
# controller is dropped. The variable cannot be overridden from here either:
# run_only.sh writes it into the harness pod spec from endpoint_stack_name,
# which is also used as --stack, so it cannot simply be set to the model.
#
# Rather than depend on that being fixed upstream, sample it ourselves. The
# result is the same shape the harness produces, so postprocess reads it with
# the same code.
#
# Usage:
#   sample_replicas.sh start <namespace> <outfile>   # backgrounds, writes a pidfile
#   sample_replicas.sh stop  <outfile>               # stops and finalises
set -u

CMD="${1:?usage: $0 start <namespace> <outfile> | stop <outfile>}"
KUBECTL="${KUBECTL_CMD:-kubectl}"
INTERVAL="${REPLICA_SAMPLE_INTERVAL:-10}"

_snapshot() {
    local ns="$1"
    $KUBECTL --namespace "$ns" get deployments,statefulsets -o json 2>/dev/null \
      | python3 -c '
import json, sys
from datetime import datetime, timezone
try:
    data = json.load(sys.stdin)
except Exception:
    data = {"items": []}
snap = {"timestamp": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "controllers": []}
for item in data.get("items", []):
    tmpl = item.get("spec", {}).get("template", {}).get("metadata", {}).get("labels", {})
    # Same predicate as the harness, WITHOUT the model filter that empties it:
    # a serving pod template, or the FMA requester.
    if tmpl.get("llm-d.ai/inferenceServing") != "true" and tmpl.get("llm-d.ai/role") != "requester":
        continue
    st = item.get("status", {})
    snap["controllers"].append({
        "name": item.get("metadata", {}).get("name", ""),
        "kind": item.get("kind", "Deployment"),
        "desired_replicas": item.get("spec", {}).get("replicas", 0),
        "ready_replicas": st.get("readyReplicas", 0) or 0,
        "available_replicas": st.get("availableReplicas", 0) or 0,
    })
print(json.dumps(snap))
'
}

# Per-pod timings, so a run can say whether FMA WOKE a sleeping instance or
# rebuilt one. That distinction is invisible in replica counts -- both look like
# "a replica arrived" -- and it is the difference between 3s and ~50-80s. We
# only found it by reading controller logs by hand; measuring it makes it a
# number in the results table instead.
#
# Emitted as JSON lines and deduped at stop, because pods vanish on scale-down:
# a single collection at the end would miss exactly the replicas we care about.
_pods_snapshot() {
    local ns="$1"
    $KUBECTL --namespace "$ns" get pods -o json 2>/dev/null \
      | python3 -c '
import json, sys
try:
    data = json.load(sys.stdin)
except Exception:
    sys.exit(0)

# Launcher creation times, so we can tell a pre-existing launcher (which COULD
# have been woken) from one built for this bind (which certainly was not).
launchers = {}
for p in data.get("items", []):
    lb = (p["metadata"].get("labels") or {})
    if lb.get("app.kubernetes.io/component") == "launcher":
        launchers[p["metadata"]["name"]] = p["metadata"].get("creationTimestamp")

for p in data.get("items", []):
    m = p["metadata"]
    lb = m.get("labels") or {}
    # Launchers also carry an inference-serving label, but they are not scale
    # -target replicas -- they are the pool a replica binds INTO. Counting them
    # here would mix the thing being measured with the thing it waits for.
    if lb.get("app.kubernetes.io/component") == "launcher":
        continue
    serving = lb.get("llm-d.ai/inference-serving") == "true" or lb.get("llm-d.ai/inferenceServing") == "true"
    requester = lb.get("llm-d.ai/role") == "requester" or lb.get("app") == "dp-app"
    if not (serving or requester):
        continue
    ready = None
    for c in p.get("status", {}).get("conditions", []):
        if c.get("type") == "Ready" and c.get("status") == "True":
            ready = c.get("lastTransitionTime")
    dual = lb.get("dual-pods.llm-d.ai/dual")
    print(json.dumps({
        "name": m.get("name"),
        "node": p.get("spec", {}).get("nodeName"),
        "created": m.get("creationTimestamp"),
        "ready_at": ready,
        "bound_launcher": dual,
        "launcher_created": launchers.get(dual) if dual else None,
        "is_requester": bool(requester),
    }))
'
}

case "$CMD" in
  start)
    NS="${2:?namespace required}"; OUT="${3:?outfile required}"
    mkdir -p "$(dirname "$OUT")"
    printf '{"snapshots":[' > "$OUT"
    : > "$OUT.pods.jsonl"
    (
      first=1
      while :; do
        snap=$(_snapshot "$NS")
        if [ -n "$snap" ]; then
          [ $first -eq 1 ] || printf ',' >> "$OUT"
          printf '%s' "$snap" >> "$OUT"
          first=0
        fi
        _pods_snapshot "$NS" >> "$OUT.pods.jsonl" 2>/dev/null || true
        sleep "$INTERVAL"
      done
    ) &
    echo $! > "$OUT.pid"
    echo "replica sampler started (pid $(cat "$OUT.pid"), every ${INTERVAL}s) -> $OUT"
    ;;
  stop)
    OUT="${2:?outfile required}"
    if [ -f "$OUT.pid" ]; then
      kill "$(cat "$OUT.pid")" 2>/dev/null || true
      rm -f "$OUT.pid"
    fi
    # Close the array even if no snapshot was written, so the file is always
    # valid JSON. An empty snapshots list reads as "not measured" downstream,
    # which is the honest answer -- unlike a zero replica count.
    printf ']}' >> "$OUT"
    # Dedupe the pod observations into one record per pod. Keep the observation
    # that has a Ready time: a pod is seen several times, and only later samples
    # carry the transition we want.
    TIMINGS="$(dirname "$OUT")/wva_pod_timings.json"
    python3 - "$OUT.pods.jsonl" "$TIMINGS" <<'PY' 2>/dev/null || true
import json, sys
src, dst = sys.argv[1], sys.argv[2]
best = {}
try:
    for line in open(src, encoding="utf-8"):
        line = line.strip()
        if not line:
            continue
        try:
            r = json.loads(line)
        except Exception:
            continue
        n = r.get("name")
        if not n:
            continue
        prev = best.get(n)
        # Prefer a record that knows when the pod became Ready, and one that
        # knows which launcher it bound to -- both appear only after the fact.
        if (prev is None
                or (r.get("ready_at") and not prev.get("ready_at"))
                or (r.get("bound_launcher") and not prev.get("bound_launcher"))):
            best[n] = r
except FileNotFoundError:
    pass
json.dump({"pods": list(best.values())}, open(dst, "w", encoding="utf-8"))
print("  pod timings: %d pod(s) -> %s" % (len(best), dst))
PY
    rm -f "$OUT.pods.jsonl"
    n=$(python3 -c "
import json,sys
try:
    d=json.load(open('$OUT'))
    s=d.get('snapshots',[])
    c=sum(len(x.get('controllers',[])) for x in s)
    print(f'{len(s)} snapshot(s), {c} controller sample(s)')
except Exception as e:
    print('unreadable:', e)
" 2>/dev/null)
    echo "replica sampler stopped: $n -> $OUT"
    ;;
  *)
    echo "unknown command: $CMD" >&2; exit 2 ;;
esac
