#!/usr/bin/env python3
"""Serve a captured panels.json back over the Prometheus HTTP API.

Grafana does not know it is talking to a file. It asks for query_range, this
answers from the snapshot, and the dashboard renders exactly as it would
against the live cluster -- which is the point: the images are the dashboard,
not a redrawing of it, and they can be produced months later with no cluster.

Stdlib only, so it runs in the same interpreters as the capture step.

Matching is by query STRING. Grafana sends the same expr the dashboard holds,
and capture stored it under the rewritten (namespace-targeted) form, so both
forms are indexed.

  promshim.py --snapshot RUNDIR/snapshot/panels.json --port 9099
"""

import argparse
import json
import re
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs, urlparse

SNAPSHOT = {}
INDEX = {}


def normalise(expr):
    """Whitespace-insensitive key. Grafana reflows newlines in an expr."""
    return re.sub(r"\s+", " ", (expr or "").strip())


def build_index(snapshot):
    index = {}
    for panel in snapshot.get("panels", []):
        for entry in panel.get("series", []):
            for key in (entry.get("expr"), entry.get("original_expr")):
                if key:
                    index[normalise(key)] = entry.get("result", [])
    return index


def say(message):
    """Print and FLUSH.

    render.sh redirects this to shim.log, and Python block-buffers a redirected
    stdout -- so without the flush the "NO MATCH" line, the one thing that
    distinguishes "the run was quiet" from "Grafana asked something the snapshot
    does not hold", never reaches the file.
    """
    print(message, flush=True)


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):  # quieter than the default
        say("  shim: " + fmt % args)

    def _json(self, payload, status=200):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):  # noqa: N802 - BaseHTTPRequestHandler's name
        parsed = urlparse(self.path)
        params = parse_qs(parsed.query)
        self._route(parsed.path, params)

    def do_POST(self):  # noqa: N802 - Grafana POSTs query_range
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length).decode() if length else ""
        parsed = urlparse(self.path)
        params = parse_qs(body) or parse_qs(parsed.query)
        self._route(parsed.path, params)

    def _route(self, path, params):
        # Grafana probes this on datasource save/health.
        if path.endswith("/api/v1/status/buildinfo"):
            return self._json({"status": "success",
                               "data": {"version": "2.53.0", "features": {}}})

        if path.endswith("/api/v1/query_range") or path.endswith("/api/v1/query"):
            expr = normalise((params.get("query") or [""])[0])
            result = INDEX.get(expr)
            if result is None:
                # Loudly, in the shim's log: a silent empty result is
                # indistinguishable from a quiet period on the chart.
                say("  shim: NO MATCH for %r" % expr[:90])
                result = []
            return self._json({"status": "success",
                               "data": {"resultType": "matrix", "result": result}})

        if path.endswith("/api/v1/labels") or path.endswith("/api/v1/metadata"):
            return self._json({"status": "success", "data": []})

        if path.endswith("/-/healthy") or path.endswith("/-/ready"):
            self.send_response(200)
            self.end_headers()
            return self.wfile.write(b"ok")

        return self._json({"status": "success", "data": []})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--snapshot", required=True)
    parser.add_argument("--port", type=int, default=9099)
    args = parser.parse_args()

    global SNAPSHOT, INDEX
    with open(args.snapshot, encoding="utf-8") as handle:
        SNAPSHOT = json.load(handle)
    INDEX = build_index(SNAPSHOT)

    window = SNAPSHOT.get("window", {})
    say("serving %d queries from %s" % (len(INDEX), args.snapshot))
    say("window: %s -> %s" % (window.get("start"), window.get("end")))
    HTTPServer(("0.0.0.0", args.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
