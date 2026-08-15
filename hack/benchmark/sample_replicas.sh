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

case "$CMD" in
  start)
    NS="${2:?namespace required}"; OUT="${3:?outfile required}"
    mkdir -p "$(dirname "$OUT")"
    printf '{"snapshots":[' > "$OUT"
    (
      first=1
      while :; do
        snap=$(_snapshot "$NS")
        if [ -n "$snap" ]; then
          [ $first -eq 1 ] || printf ',' >> "$OUT"
          printf '%s' "$snap" >> "$OUT"
          first=0
        fi
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
