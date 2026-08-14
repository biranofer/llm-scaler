#!/usr/bin/env python3
"""Capture a benchmark run's dashboard data as JSON.

This is the data half of a run snapshot. The image half is
hack/benchmark/snapshot-images/, which renders the REAL Grafana dashboard from
this JSON -- so the pictures are the dashboard, not a redrawing of it.

Stdlib only, on purpose: capture has to run wherever the cluster is reachable,
and that is often a shell whose python has no pip (the benchmark venv has no
matplotlib either). Rendering happens later, from the JSON, and needs nothing
but docker.

The queries come from the Grafana dashboard rather than being restated here, so
captured numbers cannot drift from what the dashboard shows.

Usage:
  snapshot.py --namespace NS --prometheus-url URL --token-file F \\
      --since 30m --out RUNDIR/snapshot
"""

import argparse
import json
import os
import re
import ssl
import sys
import time
import urllib.parse
import urllib.request
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
DEFAULT_DASHBOARD = REPO_ROOT / "deploy" / "grafana" / "benchmark-dashboard.json"


def load_panels(dashboard_path):
    """Return [{id, title, queries:[expr, ...]}] from a Grafana dashboard.

    The panel id is kept because Grafana renders a single panel by id
    (/render/d-solo/...?panelId=N), so the image step needs it.
    """
    with open(dashboard_path, encoding="utf-8") as handle:
        dashboard = json.load(handle)

    panels = []
    for index, panel in enumerate(dashboard.get("panels", []), start=1):
        exprs = [
            target["expr"].strip()
            for target in panel.get("targets", [])
            if target.get("expr", "").strip()
        ]
        if exprs:
            panels.append({
                "id": panel.get("id", index),
                "title": panel.get("title", "untitled"),
                "queries": exprs,
            })
    return panels


def retarget_namespace(expr, namespace):
    """Point a query at the namespace actually being benchmarked.

    The shipped dashboard hardcodes namespace=~"llm-d.*", which matches nothing
    when the benchmark runs anywhere else -- the KV-cache and queue-depth panels
    come back empty and read as a broken exporter rather than a mismatched
    matcher. Rewritten here so the dashboard itself stays untouched.
    """
    if not namespace:
        return expr
    return re.sub(r'namespace\s*=~?\s*"[^"]*"', 'namespace="%s"' % namespace, expr)


def query_range(base_url, token, expr, start, end, step, insecure):
    params = urllib.parse.urlencode(
        {"query": expr, "start": str(start), "end": str(end), "step": str(step)}
    )
    request = urllib.request.Request(base_url.rstrip("/") + "/api/v1/query_range?" + params)
    if token:
        request.add_header("Authorization", "Bearer " + token)

    context = None
    if insecure:
        context = ssl.create_default_context()
        context.check_hostname = False
        context.verify_mode = ssl.CERT_NONE

    with urllib.request.urlopen(request, timeout=60, context=context) as response:
        payload = json.load(response)

    if payload.get("status") != "success":
        raise RuntimeError(payload.get("error", "query failed"))
    return payload["data"]["result"]


def parse_duration(text):
    match = re.fullmatch(r"(\d+)([smhd])", text.strip())
    if not match:
        raise ValueError("duration must look like 90s, 30m, 2h, 1d: %r" % text)
    value, unit = int(match.group(1)), match.group(2)
    return value * {"s": 1, "m": 60, "h": 3600, "d": 86400}[unit]


def main():
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--dashboard", default=str(DEFAULT_DASHBOARD))
    parser.add_argument("--namespace", default=os.environ.get("BENCHMARK_NAMESPACE", ""))
    parser.add_argument("--prometheus-url", required=True)
    parser.add_argument("--token")
    parser.add_argument("--token-file")
    parser.add_argument("--since", default="30m")
    parser.add_argument("--start")
    parser.add_argument("--end")
    parser.add_argument("--step", default="15")
    parser.add_argument("--insecure", action="store_true",
                        help="skip TLS verification (in-cluster CAs are private)")
    parser.add_argument("--out", required=True)
    args = parser.parse_args()

    panels = load_panels(args.dashboard)
    if not panels:
        sys.exit("no panels with queries in %s" % args.dashboard)

    token = None
    if args.token_file:
        token = Path(args.token_file).read_text(encoding="utf-8").strip()
    elif args.token:
        token = args.token

    end = int(args.end or time.time())
    start = int(args.start) if args.start else end - parse_duration(args.since)

    captured, failures = [], 0
    for panel in panels:
        series = []
        for expr in panel["queries"]:
            targeted = retarget_namespace(expr, args.namespace)
            try:
                result = query_range(args.prometheus_url, token, targeted,
                                     start, end, args.step, args.insecure)
            except Exception as exc:  # noqa: BLE001 - reported, never swallowed
                print("  ! %s: %s" % (panel["title"], exc), file=sys.stderr)
                failures += 1
                result = []
            series.append({"expr": targeted, "original_expr": expr, "result": result})
            points = sum(len(s.get("values", [])) for s in result)
            print("  %-32s %-54s %2d series %5d points"
                  % (panel["title"][:32], targeted[:54], len(result), points))
        captured.append({"id": panel["id"], "title": panel["title"], "series": series})

    out_dir = Path(args.out)
    out_dir.mkdir(parents=True, exist_ok=True)
    out_file = out_dir / "panels.json"
    out_file.write_text(json.dumps({
        "captured_at": end,
        "window": {"start": start, "end": end, "step": int(args.step)},
        "namespace": args.namespace,
        "prometheus_url": args.prometheus_url,
        "dashboard": str(args.dashboard),
        "panels": captured,
    }, indent=2), encoding="utf-8")

    total = sum(len(s.get("values", []))
                for panel in captured for entry in panel["series"]
                for s in entry["result"])
    print("\nwrote %s (%d panels, %d query failures, %d points)"
          % (out_file, len(captured), failures, total))

    # An empty snapshot is not a success -- it renders as a blank dashboard and
    # reads like a quiet run rather than a bad window or namespace.
    if total == 0:
        print("NO DATA in the window. Check --namespace, --since and the "
              "Prometheus URL before trusting an empty chart.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
