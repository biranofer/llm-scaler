#!/usr/bin/env python3
"""Extract the V2 saturation analyzer's internal k1/k2 capacity decisions from
the controller logs within a given results dir's run window, and render a
markdown report of which fallback tier fired, when, and why.

Reads eight log lines:
  - k2-decision                    (saturation_v2) per replica, per cycle:
                                    which of the four k2 priority tiers fired
                                    (observed / historical / derived /
                                    fallback-to-k1)
  - replica-capacity-decision      (saturation_v2) per replica, per cycle:
                                    k1 vs k2, which bound won, demand/queue
                                    inputs
  - replica-capacity-skipped       (saturation_v2) vllm:cache_config_info is
                                    absent AND no capacity-store record covers
                                    the replica: it contributes no capacity
  - replica-capacity-store-fallback
                                    (saturation_v2) vllm:cache_config_info is
                                    absent but a capacity-store record does
                                    cover the replica
  - variant-capacity-source        (saturation_v2) zero-replica variant:
                                    compatible-variant borrow or no-data
  - zero-replica-capacity-estimate (saturation_v2) zero-replica variant:
                                    live / derived / stored-fallback estimate
  - scheduler-queue-demand         (saturation_v2) per model, per cycle: the
                                    EPP flow-control queue's token demand
  - Applied saturation decision via shared cache
                                    (steadystate) the actual, post-enforcement
                                    target replica count for the variant this
                                    cycle

The two per-replica lines are logged at V(logging.DEFAULT), which is the
verbosity the shipped deployment runs at (cmd/main.go defaults -v to
logging.DEFAULT). Started with -v=1 or lower, the controller suppresses them
and this report comes back empty.

Output:
  metrics/processed/k2_decisions.json   (raw per-event records)
  metrics/reports/k2_decision_report.md (human-readable summary)

Usage
-----
  python hack/benchmark/dump_k2_decisions.py \
      <results>/<treatment>_<i> -n NAMESPACE
"""
import argparse
import json
import re
import subprocess
import sys
from collections import Counter
from datetime import datetime, timezone
from pathlib import Path

try:
    import yaml
except ImportError:
    print("ERROR: PyYAML required. pip install pyyaml", file=sys.stderr)
    sys.exit(1)

# Tab-delimited zap console encoding: ts, level, caller, message, json-fields.
# Matched on message name only (not file:line) — see dump_wva_target_timeseries.py
# for why pinning the caller path is the wrong thing to do here. The message
# itself is matched up to the next tab rather than restricted to
# lowercase-hyphen text, since "Applied saturation decision via shared cache"
# is a plain sentence, not one of this package's own kebab-case message names.
LOG_LINE = re.compile(
    r'^(?P<ts>\S+)\t\S+\t\S+\t(?P<msg>[^\t]+)\t(?P<json>\{.*\})$'
)

DECISION_MSG = "Applied saturation decision via shared cache"
K2_MSG = "k2-decision"
RC_MSG = "replica-capacity-decision"
SQ_MSG = "scheduler-queue-demand"

MESSAGES = {
    K2_MSG,
    RC_MSG,
    SQ_MSG,
    "replica-capacity-skipped",
    "replica-capacity-store-fallback",
    "variant-capacity-source",
    "zero-replica-capacity-estimate",
    DECISION_MSG,
}

# Default cycle-clustering window, in seconds. See assign_cycles(). Must sit
# comfortably below GLOBAL_OPT_INTERVAL (the gap *between* cycles) and above
# the spread of a single cycle's own log lines.
DEFAULT_CYCLE_GAP = 3.0


def md_table(headers, rows):
    """Renders a markdown table with every column padded to its widest cell,
    so the raw .md source lines up visually in a plain-text viewer, not just
    when rendered. Returns a list of lines to extend into the report."""
    str_rows = [[str(c) for c in row] for row in rows]
    widths = [len(h) for h in headers]
    for row in str_rows:
        for i, cell in enumerate(row):
            widths[i] = max(widths[i], len(cell))

    def fmt_row(cells):
        return "| " + " | ".join(cell.ljust(w) for cell, w in zip(cells, widths)) + " |"

    lines = [fmt_row(headers), "|" + "|".join("-" * (w + 2) for w in widths) + "|"]
    lines.extend(fmt_row(row) for row in str_rows)
    return lines


# Legend for the abbreviated codes in the detail table's "Bound" and "Decision"
# columns — kept short so each row fits on one line.
BOUND_LEGEND = "k1=memory-bound won, k2=compute-bound won"
DECISION_LEGEND = "DN = the controller decided N replicas (post scale-to-zero/min-replica enforcement)"


def format_bound(bound_by):
    return {"k1-memory": "k1", "k2-compute": "k2"}.get(bound_by, bound_by)


def parse_iso(s):
    return datetime.fromisoformat(s.replace("Z", "+00:00"))


def assign_cycles(events, gap_seconds):
    """Groups timestamp-sorted events into optimize cycles, returning a list of
    per-cycle event lists.

    The controller stamps logs at whole-second precision (controller-runtime's
    RFC3339 time encoder), and one optimize cycle emits all of its lines inside
    a fraction of a second. Joining on exact timestamp equality therefore
    usually works — and silently produces garbage when it does not: a cycle
    that happens to straddle a second boundary splits into two rows, each with
    N halved, k1/k2 blank, demand totals split, and no applied decision. In the
    report that is indistinguishable from a real half-idle cycle.

    Clustering on the gap between consecutive events removes the boundary
    sensitivity: inside a cycle that gap is 0-1s, between cycles it is the
    optimize interval (15s by default), so any threshold between the two
    separates them cleanly.
    """
    cycles = []
    prev = None
    for e in events:
        ts = parse_iso(e["_ts"])
        if prev is None or (ts - prev).total_seconds() > gap_seconds:
            cycles.append([])
        cycles[-1].append(e)
        prev = ts
    return cycles


def warn_if_cycles_merged(cycles, gap_seconds):
    """scheduler-queue-demand is emitted exactly once per model per optimize
    cycle, which makes it a reliable marker for whether clustering over-merged.
    Two of them for one model inside a single cluster means --cycle-gap is at
    or above the optimize interval, so each row is covering several cycles."""
    for cycle in cycles:
        per_model = Counter(e.get("modelID") for e in cycle if e["_msg"] == SQ_MSG)
        if per_model and max(per_model.values()) > 1:
            print(
                f"WARNING: --cycle-gap={gap_seconds}s merged several optimize cycles into one "
                f"row (saw {max(per_model.values())} {SQ_MSG} events for one model in a single "
                "cluster). Lower it below the controller's GLOBAL_OPT_INTERVAL.",
                file=sys.stderr,
            )
            return


def build_cycle_row(cycle, variant):
    """Aggregates one optimize cycle's events into a single row for `variant`,
    totalled across every replica of it that reported that cycle — not one row
    per replica. Returns None when this variant did not report in this cycle."""
    rcs = [e for e in cycle if e["_msg"] == RC_MSG and e.get("variant") == variant]
    k2s = [e for e in cycle if e["_msg"] == K2_MSG and e.get("variant") == variant]
    if not rcs and not k2s:
        return None

    # scheduler-queue-demand is model-level, not per-variant, so join it on the
    # modelID this variant's own events reported rather than on whichever queue
    # event happens to share the cycle. With several models in one log window
    # that is the difference between this model's number and another model's.
    model_ids = {e.get("modelID") for e in rcs + k2s}
    sq = next((e for e in cycle
               if e["_msg"] == SQ_MSG and e.get("modelID") in model_ids), None)

    # The applied-decision line's "variant" field is "namespace/name" (a
    # composite cache key — see steadystate/engine.go), while every
    # saturation_v2 event uses the bare name. Strip the namespace prefix so
    # both sides join on the same key.
    decision = next((e for e in cycle
                     if e["_msg"] == DECISION_MSG
                     and e.get("variant", "").rsplit("/", 1)[-1] == variant), None)

    priorities = [k.get("priority", "?") for k in k2s]
    # k1 and k2 are shared across replicas of one variant unless their
    # history-bucket key differs (rare within one cycle) — show the most
    # common value rather than every replica's copy.
    k1_common = Counter(r.get("k1MemoryBound") for r in rcs).most_common(1)
    k2_common = Counter(r.get("k2ComputeBound") for r in rcs).most_common(1)
    bound_counts = Counter(format_bound(r.get("boundBy", "?")) for r in rcs)

    epp_queue = (sq.get("estimatedTokens") if sq else 0) or 0
    replica_demand_total = sum(r.get("replicaDemand", 0) or 0 for r in rcs)
    ts = min(e["_ts"] for e in rcs + k2s)
    target = decision.get("target") if decision else None

    return {
        "ts": ts,
        "time_short": ts.split("T")[1].split("+")[0] if "T" in ts else ts,
        "n": max(len(rcs), len(k2s)),
        "priority_label": ",".join(sorted(set(priorities))) if priorities else "?",
        "k1": k1_common[0][0] if k1_common else "?",
        "k2": k2_common[0][0] if k2_common else "?",
        "bound_label": ",".join(sorted(bound_counts)) if bound_counts else "?",
        "tokens_in_use": sum(r.get("tokensInUse", 0) or 0 for r in rcs),
        "local_queue": sum(r.get("localQueueDemand", 0) or 0 for r in rcs),
        "epp_queue": epp_queue,
        "total_demand": replica_demand_total + epp_queue,
        "decision": f"D{target}" if target is not None else "?",
    }


def fetch_logs(namespace, since_seconds):
    """Returns the controller's logs, or exits non-zero. A kubectl failure —
    expired token, wrong namespace, no matching pods — must not reach the
    report as an empty log window; that reads as "the controller never logged
    anything" and sends whoever holds the report off to check the image."""
    cmd = ["kubectl", "logs", "-n", namespace,
           "-l", "app.kubernetes.io/name=workload-variant-autoscaler",
           f"--since={since_seconds}s", "--tail=200000"]
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0:
        print(f"ERROR: {' '.join(cmd)} failed (exit {proc.returncode}):", file=sys.stderr)
        print(proc.stderr.strip(), file=sys.stderr)
        sys.exit(1)
    if not proc.stdout.strip():
        print(f"ERROR: no controller logs in namespace {namespace!r} over the last "
              f"{since_seconds}s. Check the -n namespace and that the "
              "workload-variant-autoscaler pod is running.", file=sys.stderr)
        sys.exit(1)
    return proc.stdout


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("results_dir", help="Path to .../results/<treatment>_<i>")
    ap.add_argument("-n", "--namespace", required=True)
    ap.add_argument("--cycle-gap", type=float, default=DEFAULT_CYCLE_GAP,
                    help="Seconds of silence separating two optimize cycles. Must be below the "
                         f"controller's GLOBAL_OPT_INTERVAL. Default: {DEFAULT_CYCLE_GAP}")
    args = ap.parse_args()

    if args.cycle_gap <= 0:
        print("ERROR: --cycle-gap must be positive", file=sys.stderr)
        sys.exit(1)

    rd = Path(args.results_dir).resolve()
    meta_path = rd / "run_metadata.yaml"
    if not meta_path.is_file():
        print(f"ERROR: run_metadata.yaml not found in {rd}", file=sys.stderr)
        sys.exit(1)
    meta = yaml.safe_load(meta_path.read_text())

    start = parse_iso(meta["harness_start"])
    stop = parse_iso(meta["harness_stop"])

    now = datetime.now(timezone.utc)
    since_seconds = int((now - start).total_seconds()) + 90

    logs = fetch_logs(args.namespace, since_seconds)

    events = []
    for line in logs.splitlines():
        m = LOG_LINE.match(line)
        if not m or m.group("msg") not in MESSAGES:
            continue
        try:
            ts_dt = parse_iso(m.group("ts"))
            if ts_dt < start or ts_dt > stop:
                continue
            d = json.loads(m.group("json"))
        except (ValueError, json.JSONDecodeError):
            continue
        d["_msg"] = m.group("msg")
        d["_ts"] = ts_dt.isoformat()
        events.append(d)

    events.sort(key=lambda e: e["_ts"])

    processed_dir = rd / "metrics" / "processed"
    processed_dir.mkdir(parents=True, exist_ok=True)
    json_out = processed_dir / "k2_decisions.json"
    json_out.write_text(json.dumps({"events": events}, indent=2))

    report = render_report(events, start, stop, args.cycle_gap)
    reports_dir = rd / "metrics" / "reports"
    reports_dir.mkdir(parents=True, exist_ok=True)
    md_out = reports_dir / "k2_decision_report.md"
    md_out.write_text(report)

    print(f"Wrote {json_out} ({len(events)} events)")
    print(f"Wrote {md_out}")


def render_report(events, start, stop, cycle_gap):
    lines = []
    lines.append("# K1/K2 Capacity Decision Report")
    lines.append("")
    lines.append(f"Window: {start.isoformat()} -> {stop.isoformat()}")
    lines.append(f"Total events captured: {len(events)}")
    lines.append("")

    k2_events = [e for e in events if e["_msg"] == K2_MSG]
    variants = sorted({e.get("variant", "?") for e in k2_events})

    cycles = assign_cycles(events, cycle_gap)
    warn_if_cycles_merged(cycles, cycle_gap)

    for variant in variants:
        rows = [r for r in (build_cycle_row(c, variant) for c in cycles) if r]
        if not rows:
            continue

        lines.append(f"## Variant: {variant}")
        lines.append("")
        lines.append("One row per optimize cycle, totalled across every ready replica of this "
                     "variant that cycle (N). KVinUse/LocalQ/EPPq/TotalDemand are all in tokens; "
                     "Priority lists every k2 tier that fired across N replicas this cycle "
                     "(P1-obs=observed, P2-hist=historical average, P3-k2=derived from deployment "
                     "args, P4-k1=no signal, memory-bound only). Time is HH:MM:SS on the run date "
                     "above.")
        lines.append("")
        lines.append(f"Legend — Bound: {BOUND_LEGEND}.  Decision: {DECISION_LEGEND}.")
        lines.append("")

        detail_rows = [[
            r["time_short"], r["n"], r["priority_label"], r["k2"], r["k1"], r["bound_label"],
            r["tokens_in_use"], r["local_queue"], r["epp_queue"], r["total_demand"], r["decision"],
        ] for r in rows]
        lines.extend(md_table(
            ["Time", "N", "Priority", "k2", "k1", "Bound", "KVinUse", "LocalQ", "EPPq",
             "TotalDemand", "Decision"],
            detail_rows))
        lines.append("")

    if not k2_events:
        lines.append("_No k1/k2 decision events found in the run window. The two per-replica "
                     "lines are logged at V(logging.DEFAULT): check that the controller was not "
                     "started with -v=1 or lower, and that its image includes the k1/k2 "
                     "logging._")
        lines.append("")

    return "\n".join(lines)


if __name__ == "__main__":
    main()
