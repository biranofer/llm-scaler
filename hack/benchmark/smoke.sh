#!/usr/bin/env bash
# The benchmark to run once, right after installing WVA, to see it work.
#
#   hack/benchmark/smoke.sh <namespace>
#   make benchmark-smoke NAMESPACE=<namespace>
#
# Decode-heavy load at 10 req/s against WHAT IS ALREADY DEPLOYED, then a
# dashboard snapshot if there is a dashboard to snapshot. It answers one
# question -- is WVA actually scaling this? -- and answers it in five minutes.
#
# Deliberately NOT llm-d-benchmark. That suite stands up its own stack and needs
# a CLI, uv, helm and helmfile; its numbers are for comparing runs. This needs a
# cluster you can already reach and nothing else: it finds the service that is
# serving, asks it which model it has, and sends requests. Plain Kubernetes and
# OpenShift alike -- no Route, no oc, no platform assumption anywhere.
#
# NOT a measurement of the model. Five minutes of arrivals is not a latency
# study, and a small model's TTFT says nothing about a large one's. For numbers
# worth comparing, use `make benchmark-run`.
#
# Decode-heavy on purpose: long outputs hold KV cache and keep requests running,
# and that is what saturation is computed from. The same rate with short outputs
# finishes each request sooner and can leave the signal too brief to see move.
set -o pipefail

case "${1:-}" in
    -h|--help)
        sed -n '2,/^[^#]/p' "$0" | sed 's/^# \{0,1\}//; $d'
        exit 0
        ;;
esac

NS="${1:-${BENCHMARK_NAMESPACE:-${NAMESPACE:-}}}"
[ -n "$NS" ] || { echo "usage: smoke.sh <namespace>   (or set NAMESPACE)" >&2; exit 2; }
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RATE="${REQUEST_RATE:-10}"
MINUTES="${SMOKE_MINUTES:-5}"
OUT="${SMOKE_OUT:-$REPO/benchmark-smoke-$NS}"
# A Kubernetes test image, not a Docker Hub one: registry.k8s.io is mirrored
# where docker.io often is not, and this has to pull on the cluster under test.
LOADER_IMAGE="${SMOKE_IMAGE:-registry.k8s.io/e2e-test-images/agnhost:2.47}"

say()  { printf '\n\033[0;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m    %s\033[0m\n' "$*" >&2; }

# --------------------------------------------------------- is the stack there
# Every piece is checked BEFORE any load is sent, and all of them are reported
# before exiting. Five minutes of traffic against a stack that cannot report is
# five minutes spent proving nothing -- and being told about one missing piece at
# a time, five minutes apart, is worse than being told about all of them now.
#
# The chain is: model servers serve, the EPP schedules and publishes the queue,
# WVA reads both and decides, KEDA owns the HPA and actuates. A gap anywhere and
# the replica count does not move, which is exactly what this run is looking at.
say "Checking the stack in $NS"
MISSING=0
ok()   { printf '  \033[0;32m+\033[0m %s\n' "$*"; }
gone() { printf '  \033[0;31m-\033[0m %s\n' "$*"; MISSING=$((MISSING + 1)); }
fix()  { printf '      %s\n' "$*"; }

# KEDA: cluster-scoped, and the CRD is the thing that matters -- a ScaledObject
# cannot exist without it, and WVA publishes decisions THROUGH it.
if kubectl get crd scaledobjects.keda.sh >/dev/null 2>&1; then
    ok "KEDA (scaledobjects.keda.sh)"
else
    gone "KEDA is not installed (no scaledobjects.keda.sh CRD)"
    fix "the installer adds it when the cluster has none: make deploy-wva NAMESPACE=$NS"
fi

# WVA: a controller that manages THIS namespace. One that watches another is not
# the same as one watching yours, and reads identically in `kubectl get deploy -A`.
WVA_DEPLOY="$(kubectl get deploy -A -l app.kubernetes.io/name=workload-variant-autoscaler \
    -o jsonpath='{range .items[*]}{.metadata.namespace}{"|"}{.spec.template.spec.containers[0].env[?(@.name=="WVA_WATCH_NAMESPACE")].value}{"|"}{.status.readyReplicas}{"\n"}{end}' 2>/dev/null)"
WVA_FOR_NS=""
while IFS='|' read -r wns watch ready; do
    [ -n "$wns" ] || continue
    case "$watch" in
        # The literal is the base default and means "its own namespace"; only an
        # install given WVA_WATCH_NS patches a real name in.
        ''|'$(POD_NAMESPACE)') watch="$wns" ;;
    esac
    if [ "$watch" = "$NS" ] && [ "${ready:-0}" -ge 1 ]; then WVA_FOR_NS="$wns"; fi
done <<EOF
$WVA_DEPLOY
EOF
if [ -n "$WVA_FOR_NS" ]; then
    ok "WVA controller in $WVA_FOR_NS, managing $NS"
else
    gone "No ready WVA controller manages $NS"
    fix "make deploy-wva NAMESPACE=$NS"
fi

# Model servers: what there is to scale. Read from the pod template, so a
# workload parked at zero still counts.
. "$REPO/deploy/lib/common.sh" 2>/dev/null || true
. "$REPO/deploy/lib/scaledobject.sh" 2>/dev/null || true
. "$REPO/deploy/lib/infra_monitoring.sh" 2>/dev/null || true
SERVERS=0
if command -v wva_serving_workload_count >/dev/null 2>&1; then
    SERVERS="$(wva_serving_workload_count "$NS" 2>/dev/null || echo 0)"
fi
if [ "${SERVERS:-0}" -gt 0 ]; then
    ok "$SERVERS llm-d model server(s)"
else
    gone "No llm-d model servers in $NS"
    fix "install llm-d there first; this drives load, it does not deploy models"
fi

# EPP: the scheduler. Without it there is no queue signal, and queued demand is
# the input that makes a decode-heavy run scale at all.
EPPS=""
command -v wva_epp_services >/dev/null 2>&1 && EPPS="$(wva_epp_services "$NS" 2>/dev/null)"
[ -n "$EPPS" ] || EPPS="$(kubectl get svc -n "$NS" -o name 2>/dev/null \
    | grep -E "router-epp|epp$" | cut -d/ -f2)"
if [ -n "$EPPS" ]; then
    ok "EPP: $(printf '%s' "$EPPS" | paste -sd, -)"
else
    gone "No EPP (InferencePool endpoint picker) in $NS"
    fix "see docs/guides/testing-with-llm-d/ for a stack with one"
fi

# Registration: WVA scales what a ScaledObject names, and nothing else.
SO_COUNT="$(kubectl get scaledobject -n "$NS" --no-headers 2>/dev/null | wc -l | tr -d ' ')"
if [ "${SO_COUNT:-0}" -gt 0 ]; then
    ok "$SO_COUNT ScaledObject(s) registered"
else
    gone "No ScaledObject in $NS, so WVA is scaling nothing there"
    fix "make scaledobjects-plan NAMESPACE=$NS   then  make scaledobjects-apply"
fi

if [ "$MISSING" -gt 0 ]; then
    echo
    echo "  $MISSING piece(s) missing. Nothing was run." >&2
    echo "  A fuller check, with the metrics side too:  make check-prereqs NAMESPACE=$NS" >&2
    exit 1
fi

# The service already serving. Same detection wait_serving.sh uses, and the port
# is chosen BY NAME: a router-epp service exposes grpc-ext-proc first, so
# ports[0] is gRPC and sending HTTP at it gets nothing back at all.
SVC="${SMOKE_ENDPOINT_SVC:-}"
[ -n "$SVC" ] || SVC="$(kubectl get svc -n "$NS" -o name 2>/dev/null \
    | grep -E "router-epp|inference-gateway|epp$" | head -1 | cut -d/ -f2)"
[ -n "$SVC" ] || { echo "  No router/EPP service in $NS. Set SMOKE_ENDPOINT_SVC=<service>." >&2; exit 1; }
PORT="$(kubectl get svc "$SVC" -n "$NS" -o json 2>/dev/null | python3 -c '
import json, sys
ports = json.load(sys.stdin)["spec"].get("ports", [])
for p in ports:
    if p.get("name") == "http":
        print(p["port"]); sys.exit(0)
for p in ports:
    if p.get("port") in (80, 8000, 8080):
        print(p["port"]); sys.exit(0)
if ports:
    print(ports[0]["port"])')"
[ -n "$PORT" ] || { echo "  Could not resolve an HTTP port on $SVC." >&2; exit 1; }
BASE="http://${SVC}.${NS}.svc.cluster.local:${PORT}"
echo "  Endpoint: $BASE"

# Ask the endpoint what it serves rather than being told. A model name that does
# not match what is loaded returns an error per request, which looks like load
# being driven and produces no saturation at all.
MODEL="${MODEL_ID:-}"
if [ -z "$MODEL" ]; then
    MODEL="$(kubectl run smoke-probe-$$ -n "$NS" --rm -i --restart=Never --quiet \
        --image="$LOADER_IMAGE" --command -- \
        /bin/sh -c "curl -s --max-time 10 ${BASE}/v1/models" 2>/dev/null \
        | python3 -c 'import json,sys
try:
    print(json.load(sys.stdin)["data"][0]["id"])
except Exception:
    pass' 2>/dev/null)"
fi
[ -n "$MODEL" ] || { echo "  $BASE/v1/models named no model. Pass MODEL_ID=<id>." >&2; exit 1; }
echo "  Model:    $MODEL"

# ------------------------------------------------------------------------ load
say "Driving decode-heavy load at ${RATE} req/s for ${MINUTES}m"
START_EPOCH="$(date -u +%s)"
JOB="wva-smoke-$(date -u +%H%M%S)"
DURATION="$((MINUTES * 60))"
GAP="$(awk "BEGIN{printf \"%.3f\", 1/${RATE}}")"
BODY="{\"model\":\"${MODEL}\",\"prompt\":\"Summarize the history of computing in as much detail as you can.\",\"max_tokens\":1024,\"temperature\":0.7}"

# One pod, open loop: a request every 1/RATE seconds whether or not the previous
# one finished. A closed loop (N workers in lockstep) throttles itself exactly
# when the server slows down, which is the moment worth measuring. In-flight is
# capped so a stalled server cannot fan out without bound.
LOAD_SCRIPT="end=\$(( \$(date +%s) + ${DURATION} ))
sent=0
while [ \$(date +%s) -lt \$end ]; do
  if [ \$(jobs -p | wc -l) -lt 200 ]; then
    curl -s -o /dev/null --max-time 120 -H 'Content-Type: application/json' \\
         -d '${BODY}' '${BASE}/v1/completions' &
    sent=\$((sent+1))
  fi
  sleep ${GAP}
done
wait
echo \"dispatched \$sent requests\""

python3 - "$NS" "$JOB" "$LOADER_IMAGE" "$LOAD_SCRIPT" <<'PYEOF' | kubectl apply -n "$NS" -f - >/dev/null
import json, sys
ns, job, image, script = sys.argv[1:5]
print(json.dumps({
    "apiVersion": "batch/v1", "kind": "Job",
    "metadata": {"name": job, "namespace": ns,
                 "labels": {"app.kubernetes.io/name": "wva-smoke"}},
    "spec": {"backoffLimit": 0, "template": {
        "metadata": {"labels": {"app.kubernetes.io/name": "wva-smoke"}},
        "spec": {"restartPolicy": "Never", "containers": [{
            "name": "load", "image": image,
            "command": ["/bin/sh", "-c"], "args": [script],
            "resources": {"requests": {"cpu": "100m", "memory": "128Mi"}}}]}}}}))
PYEOF
echo "  Job/${JOB} started"
kubectl wait --for=condition=complete --timeout=$((DURATION + 180))s \
    "job/${JOB}" -n "$NS" >/dev/null 2>&1 \
    || warn "the load job did not report complete; reporting on what did happen"
kubectl logs -n "$NS" "job/${JOB}" --tail=1 2>/dev/null | sed 's/^/  /'
kubectl delete job "${JOB}" -n "$NS" --wait=false >/dev/null 2>&1

# -------------------------------------------------------------------- snapshot
# GATED on a dashboard existing. The snapshot's value is that it is the
# dashboard's own queries over the run's window; with no dashboard deployed there
# is nothing to point the reader at afterwards, and a panels.json nobody can
# render is not a result. So look for one, and if there is none, say how to get
# one rather than writing a file and calling it a snapshot.
say "Looking for a dashboard to snapshot"
DASH_KIND=""
DASH_WHERE=""
if [ "$(kubectl get grafana -n "$NS" --no-headers 2>/dev/null | wc -l | tr -d ' ')" != "0" ]; then
    DASH_KIND="grafana-operator"; DASH_WHERE="$NS"
else
    for cm_ns in "$NS" "${MONITORING_NAMESPACE:-workload-variant-autoscaler-monitoring}" \
                 openshift-user-workload-monitoring; do
        if kubectl get configmap wva-operation-dashboard -n "$cm_ns" >/dev/null 2>&1; then
            DASH_KIND="configmap"; DASH_WHERE="$cm_ns"; break
        fi
    done
fi

SNAP=""
if [ -z "$DASH_KIND" ]; then
    warn "No dashboard in reach, so nothing was snapshotted. The load still ran."
    warn "  OpenShift:  make dashboard NAMESPACE=$NS   (private Grafana, dashboard imported)"
    warn "  Kubernetes: import deploy/grafana/operational-dashboard.json into your Grafana"
else
    echo "  Found: $DASH_KIND in $DASH_WHERE"
    PROM="${PROMETHEUS_URL:-}"
    if [ -z "$PROM" ]; then
        # The installer's own detection: Thanos on OpenShift, prometheus-operated
        # elsewhere. Sourcing it keeps one answer for "which Prometheus".
        # shellcheck source=/dev/null
        . "$REPO/deploy/lib/common.sh" 2>/dev/null || true
        # shellcheck source=/dev/null
        . "$REPO/deploy/lib/infra_monitoring.sh" 2>/dev/null || true
        if command -v wva_detect_prometheus_url >/dev/null 2>&1; then
            PROM="$(wva_detect_prometheus_url 2>/dev/null || true)"
        fi
    fi
    if [ -z "$PROM" ]; then
        warn "Could not work out the Prometheus URL; pass PROMETHEUS_URL=<url> to snapshot."
    else
        echo "  Prometheus: $PROM"
        mkdir -p "$OUT"
        # The run's own elapsed window, not a fixed --since: a slow start would
        # otherwise push the interesting minutes off the left of every chart.
        SINCE="$(( ($(date -u +%s) - START_EPOCH) / 60 + 2 ))m"
        TOK="$(kubectl create token default -n "$NS" 2>/dev/null || true)"
        if [ -z "$TOK" ] && command -v oc >/dev/null 2>&1; then
            TOK="$(oc whoami -t 2>/dev/null || true)"
        fi
        if "$REPO/hack/benchmark/snapshot.py" --namespace "$NS" \
                --prometheus-url "$PROM" ${TOK:+--token "$TOK"} --insecure \
                --since "$SINCE" --out "$OUT/snapshot" >/dev/null 2>&1; then
            SNAP="$OUT/snapshot"
            echo "  Captured $SINCE of dashboard data"
        else
            warn "Capture failed. The run itself is unaffected."
        fi
    fi
fi

# ---------------------------------------------------------------------- report
echo
echo "================================================================="
echo " WVA smoke: $NS"
echo "================================================================="
echo
if [ -n "$SNAP" ]; then
    echo "  Snapshot: $SNAP/panels.json"
    echo "  Render:   hack/benchmark/snapshot-images/render.sh $SNAP"
fi
ROUTE="$(kubectl get route -n "$NS" \
    -o jsonpath='{.items[?(@.metadata.name=="wva-grafana-route")].spec.host}' 2>/dev/null || true)"
if [ -n "$ROUTE" ]; then
    echo "  Live:     https://${ROUTE}/d/wva-op-dash-v2/wva-operational-dashboard?var-namespace=${NS}"
fi
cat <<REPORT

  What to look for, in this order:
    1. replicas MOVED. Flat at one through a deep queue means WVA never saw the
       load -- a metrics problem wearing a scaling decision's clothes.
    2. error count 0. Anything else and the latency panels measure retries.
    3. queue depth and KV cache non-empty. Empty means the EPP or the model
       servers are not scraped: make check-prereqs NAMESPACE=$NS

  Replica history, with no dashboard at all:
    kubectl get hpa -n $NS -w

REPORT
