#!/usr/bin/env bash
# Render a captured snapshot through the REAL Grafana dashboard, locally.
#
#   render.sh <snapshot-dir>
#
# <snapshot-dir> holds panels.json from hack/benchmark/snapshot.py. Output is
# one PNG per panel plus a full-dashboard PNG, written beside it.
#
# Why Grafana and not a plotting library: these are meant to BE the dashboard,
# so they must not drift from it in colour, axis or panel choice. Grafana reads
# deploy/grafana/benchmark-dashboard.json directly; the data comes from the
# snapshot through a shim that speaks the Prometheus API. Nothing here touches a
# cluster, so any past run can be re-rendered.
set -euo pipefail

SNAP_DIR="${1:?usage: render.sh <snapshot-dir>}"
SNAP_DIR="$(cd "$SNAP_DIR" && pwd)"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/../../.." && pwd)"
DASHBOARD="$REPO/deploy/grafana/benchmark-dashboard.json"

[ -f "$SNAP_DIR/panels.json" ] || { echo "no panels.json in $SNAP_DIR"; exit 1; }
[ -f "$DASHBOARD" ] || { echo "dashboard not found: $DASHBOARD"; exit 1; }

command -v docker >/dev/null || { echo "docker is required"; exit 1; }

# Ports are chosen free rather than fixed. Under WSL2 the port space is shared
# with Windows, so a fixed default collides with whatever the host happens to be
# running -- 9099 was taken by an unrelated node process, and the shim died with
# "Address already in use" after the compose stack had already been pulled.
# Set SNAPSHOT_{SHIM,GRAFANA}_PORT to pin them.
free_port() {
    python3 -c 'import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()'
}
PORT_GRAFANA="${SNAPSHOT_GRAFANA_PORT:-$(free_port)}"
PORT_SHIM="${SNAPSHOT_SHIM_PORT:-$(free_port)}"
PROJECT="wva-snapshot"
echo "grafana :$PORT_GRAFANA   shim :$PORT_SHIM"

cleanup() {
    docker compose -p "$PROJECT" -f "$HERE/docker-compose.yml" down -v >/dev/null 2>&1 || true
    [ -n "${SHIM_PID:-}" ] && kill "$SHIM_PID" 2>/dev/null || true
}
trap cleanup EXIT

# The shim runs on the HOST, not in compose: it needs the snapshot file, and
# host networking differs across platforms. Grafana reaches it through
# host.docker.internal, which the compose file maps for Linux too.
python3 "$HERE/promshim.py" --snapshot "$SNAP_DIR/panels.json" --port "$PORT_SHIM" \
    > "$SNAP_DIR/shim.log" 2>&1 &
SHIM_PID=$!
sleep 1
kill -0 "$SHIM_PID" 2>/dev/null || { echo "shim failed to start:"; cat "$SNAP_DIR/shim.log"; exit 1; }

# The window the snapshot covers -- Grafana is asked for exactly this range, so
# the images show the run and not "now", which would be empty.
FROM_MS=$(python3 -c "import json;print(int(json.load(open('$SNAP_DIR/panels.json'))['window']['start'])*1000)")
TO_MS=$(python3 -c "import json;print(int(json.load(open('$SNAP_DIR/panels.json'))['window']['end'])*1000)")

export SNAPSHOT_DIR="$SNAP_DIR" DASHBOARD_FILE="$DASHBOARD" \
       PORT_GRAFANA="$PORT_GRAFANA" PORT_SHIM="$PORT_SHIM"
docker compose -p "$PROJECT" -f "$HERE/docker-compose.yml" up -d

echo "waiting for grafana on :$PORT_GRAFANA ..."
for _ in $(seq 1 60); do
    if curl -sf "http://localhost:$PORT_GRAFANA/api/health" >/dev/null 2>&1; then break; fi
    sleep 2
done
curl -sf "http://localhost:$PORT_GRAFANA/api/health" >/dev/null || {
    echo "grafana did not come up"; docker compose -p "$PROJECT" -f "$HERE/docker-compose.yml" logs --tail=30; exit 1; }

UID_DASH=$(python3 -c "import json;d=json.load(open('$DASHBOARD'));print(d.get('uid') or 'wva-benchmark')")
SLUG=$(python3 -c "import json,re;d=json.load(open('$DASHBOARD'));print(re.sub(r'[^a-z0-9]+','-',(d.get('title') or 'dashboard').lower()).strip('-'))")

# The dashboard takes a namespace variable, so the render URL has to say which
# one -- otherwise Grafana renders the variable's default ("All") and the panel
# titles claim a scope the snapshot does not have.
NS_VAR=$(python3 -c "import json;print(json.load(open('$SNAP_DIR/panels.json')).get('namespace') or '')")
# The LABEL is a dashboard variable too, and its default (exported_namespace) is
# wrong wherever the metric was not relabelled. Pass what capture actually
# queried, or the panels render empty against a perfectly good snapshot.
NS_LABEL=$(python3 -c "import json;print(json.load(open('$SNAP_DIR/panels.json')).get('namespace_label') or 'namespace')")
echo "rendering panels from $DASHBOARD (namespace=${NS_VAR:-<all>}, label=$NS_LABEL)"
python3 - "$SNAP_DIR" "$PORT_GRAFANA" "$UID_DASH" "$SLUG" "$FROM_MS" "$TO_MS" "$NS_VAR" "$NS_LABEL" "$DASHBOARD" <<'PY'
import json, subprocess, sys, re, pathlib
snap_dir, port, uid, slug, frm, to = sys.argv[1:7]
ns_var = sys.argv[7] if len(sys.argv) > 7 else ""
ns_label = sys.argv[8] if len(sys.argv) > 8 else "namespace"
# Panels come from the DASHBOARD being rendered, not from the snapshot.
#
# The snapshot records the panels that existed when it was captured. Driving the
# loop from that list renders panel ids the dashboard may no longer have, and
# Grafana answers those with a near-empty PNG rather than an error: re-rendering
# an August snapshot after a panel was deleted produced a 5KB
# panel-wva-desired-ratio.png that looked like a successful render of a panel
# that does not exist, and silently skipped a panel added since.
#
# Reading the dashboard instead means the images always match what a viewer
# would see. Panels whose queries the snapshot lacks render empty, which is
# honest and already visible in shim.log as NO MATCH.
dash = json.load(open(sys.argv[9])) if len(sys.argv) > 9 else None
if dash:
    panels = [q for q in dash.get("panels", []) if q.get("type") != "row" and "id" in q]
else:
    panels = json.load(open(pathlib.Path(snap_dir) / "panels.json"))["panels"]
for panel in panels:
    safe = re.sub(r"[^a-z0-9]+", "-", panel["title"].lower()).strip("-")
    out = pathlib.Path(snap_dir) / f"panel-{safe}.png"
    url = (f"http://localhost:{port}/render/d-solo/{uid}/{slug}"
           f"?panelId={panel['id']}&from={frm}&to={to}&width=1000&height=400&tz=UTC&timeout=15"
           + (f"&var-namespace={ns_var}" if ns_var else "")
           + f"&var-namespace_label={ns_label}")
    rc = subprocess.run(["curl", "-sf", "-o", str(out), url]).returncode
    size = out.stat().st_size if out.exists() else 0
    print(f"  {panel['title'][:34]:34} {out.name:44} {'ok' if rc==0 and size>1000 else 'FAILED'} ({size}B)")
PY

FULL="$SNAP_DIR/dashboard.png"
curl -sf -o "$FULL" \
  "http://localhost:$PORT_GRAFANA/render/d/$UID_DASH/$SLUG?from=$FROM_MS&to=$TO_MS&width=1200&height=1400&tz=UTC&timeout=30${NS_VAR:+&var-namespace=$NS_VAR}&var-namespace_label=$NS_LABEL" \
  && echo "  full dashboard -> $(basename "$FULL") ($(wc -c < "$FULL")B)" \
  || echo "  full dashboard render FAILED"

echo
echo "images in $SNAP_DIR:"
ls -1 "$SNAP_DIR"/*.png 2>/dev/null | sed 's/^/  /' || echo "  none produced"
echo "shim log: $SNAP_DIR/shim.log (grep 'NO MATCH' for queries the snapshot lacks)"
