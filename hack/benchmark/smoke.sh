#!/usr/bin/env bash
# The benchmark to run once, right after installing WVA, to see it work.
#
#   hack/benchmark/smoke.sh <namespace>
#
# Decode-heavy at 10 req/s for five minutes, then a snapshot of the dashboard
# over exactly that window, and a line telling you where to look. It answers one
# question -- "is WVA actually scaling this?" -- and answers it with the same
# dashboard the operational docs describe, so what you see here is what you will
# see in production.
#
# NOT a measurement of the model. A 0.6B model's TTFT says nothing about a 32B
# one, and five minutes of Poisson arrivals is not a latency study. It is a
# check that load arrives, metrics arrive, and the replica count moves. For real
# numbers use `make benchmark-run`, which drives the full scenario.
#
# Decode-heavy on purpose: long outputs hold KV cache and keep requests running,
# which is what saturation is computed from. A prefill-heavy shape at the same
# rate finishes each request sooner and can leave the utilization signal too
# brief to see move.
set -o pipefail

case "${1:-}" in
    -h|--help)
        sed -n '2,/^[^#]/p' "$0" | sed 's/^# \{0,1\}//; $d'
        exit 0
        ;;
esac

NS="${1:-${BENCHMARK_NAMESPACE:-${NAMESPACE:-}}}"
[ -n "$NS" ] || { echo "usage: smoke.sh <namespace>   (or set BENCHMARK_NAMESPACE)" >&2; exit 2; }
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RATE="${REQUEST_RATE:-10}"
MINUTES="${SMOKE_MINUTES:-5}"
OUT="${SMOKE_OUT:-$REPO/benchmark-smoke-$NS}"

say() { printf '\n\033[0;34m==>\033[0m %s\n' "$*"; }

# ---------------------------------------------------------------- preconditions
# Checked before the load, not after: five minutes of traffic against a stack
# that cannot report is five minutes spent proving nothing.
say "Checking $NS can be measured"
replicas="$(kubectl get scaledobject -n "$NS" -o name 2>/dev/null | wc -l)"
if [ "$replicas" -eq 0 ]; then
    echo "  No ScaledObject in $NS, so WVA is not scaling anything there." >&2
    echo "  Register the workloads first:  make scaledobjects-plan NAMESPACE=$NS" >&2
    exit 1
fi
echo "  $replicas ScaledObject(s) registered"

PROM="${PROMETHEUS_URL:-}"
if [ -z "$PROM" ]; then
    # Same resolution the installer uses, so this needs no argument on the
    # platform where the installer needed none either.
    # shellcheck source=/dev/null
    . "$REPO/deploy/lib/common.sh" 2>/dev/null || true
    # shellcheck source=/dev/null
    . "$REPO/deploy/lib/infra_monitoring.sh" 2>/dev/null || true
    command -v wva_detect_prometheus_url >/dev/null 2>&1 && PROM="$(wva_detect_prometheus_url 2>/dev/null || true)"
fi
[ -n "$PROM" ] || { echo "  Could not work out the Prometheus URL. Pass PROMETHEUS_URL=<url>." >&2; exit 1; }
echo "  Prometheus: $PROM"

# ------------------------------------------------------------------------ load
say "Driving decode-heavy load at ${RATE} req/s for ${MINUTES}m"
start_epoch="$(date -u +%s)"
if ! make -C "$REPO" benchmark-run \
        BENCHMARK_NAMESPACE="$NS" \
        BENCHMARK_WORKLOAD=decode_heavy \
        REQUEST_RATE="$RATE" \
        MAX_DURATION="$((MINUTES * 60))"; then
    echo "  The load run failed. The snapshot below covers whatever did happen," >&2
    echo "  which is usually enough to see why." >&2
fi

# -------------------------------------------------------------------- snapshot
# Captured over the run's own window rather than a fixed --since, so a slow
# standup does not push the interesting minutes off the left of every chart.
say "Capturing the dashboard over the run's window"
mkdir -p "$OUT"
since="$(( ($(date -u +%s) - start_epoch) / 60 + 2 ))m"
tok=""
command -v oc >/dev/null 2>&1 && tok="$(oc whoami -t 2>/dev/null || true)"
[ -n "$tok" ] || tok="$(kubectl create token default -n "$NS" 2>/dev/null || true)"

"$REPO/hack/benchmark/snapshot.py" \
    --namespace "$NS" \
    --prometheus-url "$PROM" \
    ${tok:+--token "$tok"} \
    --insecure \
    --since "$since" \
    --out "$OUT/snapshot" || {
        echo "  Capture failed. The run itself is unaffected." >&2; exit 1; }

# --------------------------------------------------------------------- render
# Optional: the JSON is the snapshot, the PNGs are a convenience. Docker is not
# a prerequisite for having captured the run.
rendered=""
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    say "Rendering it through the real dashboard"
    if "$REPO/hack/benchmark/snapshot-images/render.sh" "$OUT/snapshot" >/dev/null 2>&1; then
        rendered="$(ls "$OUT/snapshot"/*dashboard*.png 2>/dev/null | head -1)"
    else
        echo "  Render failed; the captured JSON is still complete." >&2
    fi
fi

# ---------------------------------------------------------------------- report
cat <<EOF

=================================================================
 Snapshot of $NS
=================================================================

  Data:    $OUT/snapshot/panels.json
EOF
[ -n "$rendered" ] && echo "  Picture: $rendered"
[ -n "$rendered" ] || cat <<EOF
  Picture: not rendered (docker unavailable). To render it later, anywhere:
             hack/benchmark/snapshot-images/render.sh $OUT/snapshot
EOF
route="$(kubectl get route wva-grafana-route -n "$NS" -o jsonpath='{.spec.host}' 2>/dev/null || true)"
if [ -n "$route" ]; then
    echo "  Live:    https://${route}/d/wva-op-dash-v2/wva-operational-dashboard?var-namespace=${NS}"
else
    echo "  Live:    make dashboard NAMESPACE=$NS   (OpenShift; stands up a private Grafana)"
fi
cat <<EOF

  What to look for, in this order:
    1. replicas MOVED. Flat at one through a deep queue means WVA never saw the
       load, which is a metrics problem, not a scaling decision.
    2. error count 0. Anything else and the latency panels are measuring retries.
    3. queue depth and KV cache non-empty. Empty means the EPP or the model
       servers are not scraped -- see 'make check-prereqs'.

EOF
