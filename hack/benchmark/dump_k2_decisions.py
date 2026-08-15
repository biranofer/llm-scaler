#!/usr/bin/env python3
"""Extract the V2 saturation analyzer's internal k1/k2 capacity decisions from
the controller logs within a given results dir's run window, and render a
markdown report of which fallback tier fired, when, and why.

Reads five log lines emitted by internal/engines/analyzers/saturation_v2:
  - k2-decision                    per replica, per cycle: which of the four
                                    k2 priority tiers fired (observed /
                                    historical / derived / fallback-to-k1)
  - replica-capacity-decision       per replica, per cycle: k1 vs k2, which
                                    bound won, demand/queue inputs
  - replica-capacity-no-cache-info  when vllm:cache_config_info is absent
  - variant-capacity-source         zero-replica variant: compatible-variant
                                    borrow or no-data
  - zero-replica-capacity-estimate  zero-replica variant: live / derived /
                                    stored-fallback estimate

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
from collections import Counter, defaultdict
from datetime import datetime, timezone
from pathlib import Path

try:
    import yaml
except ImportError:
    print("ERROR: PyYAML required. pip install pyyaml", file=sys.stderr)
    sys.exit(1)

# Tab-delimited zap console encoding: ts, level, caller, message, json-fields.
# Matched on message name only (not file:line) — see dump_wva_target_timeseries.py
# for why pinning the caller path is the wrong thing to do here.
LOG_LINE = re.compile(
    r'^(?P<ts>\S+)\t\S+\t\S+\t(?P<msg>[a-z0-9-]+)\t(?P<json>\{.*\})$'
)

MESSAGES = {
    "k2-decision",
    "replica-capacity-decision",
    "replica-capacity-no-cache-info",
    "variant-capacity-source",
    "zero-replica-capacity-estimate",
    "scheduler-queue-demand",
}

PRIORITY_ORDER = ["P1-obs", "P2-hist", "P3-k2", "P4-k1"]
PRIORITY_MEANING = {
    "P1-obs": "observed (queue saturated, tokensInUse used directly)",
    "P2-hist": "historical (rolling average of prior observations)",
    "P3-k2": "derived (estimated from deployment args)",
    "P4-k1": "fallback (no observed/historical/derived signal; memory-bound only)",
}


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


# Legend for the abbreviated codes in the per-iteration detail table's
# "Inputs" and "Bound" columns — kept short so each row fits on one line.
INPUTS_LEGEND = (
    "q=queueLength/queueThreshold, h=history-window sample count, "
    "in/out=avg input/output tokens this cycle, no-sig=no observed/historical/"
    "derived signal (fell all the way to k1)"
)
BOUND_LEGEND = "k1=memory-bound won, k2=compute-bound won"


def format_priority_inputs(priority, e):
    """Renders the fallback-chain inputs specific to whichever k2 priority
    tier fired, for the per-iteration detail table, as a short code (see
    INPUTS_LEGEND) rather than a sentence — keeps each row to one line."""
    if priority == "P1-obs":
        return f"q{e.get('queueLength','?')}/{e.get('queueThreshold','?')} h{e.get('historyWindowLen','?')}"
    if priority == "P2-hist":
        return f"h{e.get('historyWindowLen','?')}"
    if priority == "P3-k2":
        return f"in{e.get('avgInputTokens','?')} out{e.get('avgOutputTokens','?')}"
    if priority == "P4-k1":
        return "no-sig"
    return "?"


def format_bound(bound_by):
    return {"k1-memory": "k1", "k2-compute": "k2"}.get(bound_by, bound_by)


def build_cycles(rc_events, k2_events, sq_by_ts):
    """Aggregates k2-decision + replica-capacity-decision + scheduler-queue-demand
    into ONE row per optimize cycle (timestamp), totalled across every replica
    that reported that cycle. Used for both the tier-transitions table and the
    per-iteration detail table, so a cycle with several replicas at different
    priorities is one aggregate point in both — not several same-timestamp rows
    read as a sequence of transitions that never happened in time."""
    rc_by_ts = defaultdict(list)
    for e in rc_events:
        rc_by_ts[e["_ts"]].append(e)
    k2_by_ts = defaultdict(list)
    for e in k2_events:
        k2_by_ts[e["_ts"]].append(e)

    cycles = []
    for ts in sorted(set(rc_by_ts) | set(k2_by_ts)):
        rcs = rc_by_ts.get(ts, [])
        k2s = k2_by_ts.get(ts, [])

        priorities = [k.get("priority", "?") for k in k2s]
        priority_label = ",".join(sorted(set(priorities))) if priorities else "?"
        # k1 and k2 are shared across replicas of one variant unless their
        # history-bucket key differs (rare within one cycle) — show the most
        # common value rather than every replica's copy.
        k1_common = Counter(r.get("k1MemoryBound") for r in rcs).most_common(1)
        k2_common = Counter(r.get("k2ComputeBound") for r in rcs).most_common(1)
        bound_counts = Counter(format_bound(r.get("boundBy", "?")) for r in rcs)

        tokens_in_use = sum(r.get("tokensInUse", 0) or 0 for r in rcs)
        local_queue = sum(r.get("localQueueDemand", 0) or 0 for r in rcs)
        replica_demand_total = sum(r.get("replicaDemand", 0) or 0 for r in rcs)

        sq = sq_by_ts.get(ts)
        epp_queue = (sq.get("estimatedTokens") if sq else 0) or 0

        cycles.append({
            "ts": ts,
            "time_short": ts.split("T")[1].split("+")[0] if "T" in ts else ts,
            "n": max(len(rcs), len(k2s)),
            "priority_label": priority_label,
            "k1": k1_common[0][0] if k1_common else "?",
            "k2": k2_common[0][0] if k2_common else "?",
            "bound_label": ",".join(sorted(bound_counts)) if bound_counts else "?",
            "tokens_in_use": tokens_in_use,
            "local_queue": local_queue,
            "epp_queue": epp_queue,
            "total_demand": replica_demand_total + epp_queue,
        })
    return cycles


def parse_iso(s):
    return datetime.fromisoformat(s.replace("Z", "+00:00"))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("results_dir", help="Path to .../results/<treatment>_<i>")
    ap.add_argument("-n", "--namespace", required=True)
    args = ap.parse_args()

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

    logs = subprocess.run(
        ["kubectl", "logs", "-n", args.namespace,
         "-l", "app.kubernetes.io/name=workload-variant-autoscaler",
         f"--since={since_seconds}s", "--tail=200000"],
        capture_output=True, text=True,
    ).stdout

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

    report = render_report(events, start, stop)
    reports_dir = rd / "metrics" / "reports"
    reports_dir.mkdir(parents=True, exist_ok=True)
    md_out = reports_dir / "k2_decision_report.md"
    md_out.write_text(report)

    print(f"Wrote {json_out} ({len(events)} events)")
    print(f"Wrote {md_out}")


def render_report(events, start, stop):
    lines = []
    lines.append("# K1/K2 Capacity Decision Report")
    lines.append("")
    lines.append(f"Window: {start.isoformat()} -> {stop.isoformat()}")
    lines.append(f"Total events captured: {len(events)}")
    lines.append("")

    k2_events = [e for e in events if e["_msg"] == "k2-decision"]
    variants = sorted({e.get("variant", "?") for e in k2_events})

    # scheduler-queue-demand is model-level (one per cycle, not per variant or
    # per replica) — joined into each variant's per-iteration table by
    # timestamp only. Fine for the common single-model-per-report case; with
    # several models sharing a log window this would need a modelID filter too.
    sq_by_ts = {e["_ts"]: e for e in events if e["_msg"] == "scheduler-queue-demand"}

    for variant in variants:
        v_events = [e for e in k2_events if e.get("variant") == variant]
        if not v_events:
            continue
        lines.append(f"## Variant: {variant}")
        lines.append("")

        counts = Counter(e.get("priority", "?") for e in v_events)
        total = sum(counts.values())
        lines.append("### K2 fallback-tier distribution")
        lines.append("")
        dist_rows = []
        for p in PRIORITY_ORDER:
            if p not in counts:
                continue
            pct = 100 * counts[p] / total if total else 0
            dist_rows.append([p, PRIORITY_MEANING[p], counts[p], f"{pct:.1f}%"])
        lines.extend(md_table(["Priority", "Meaning", "Count", "% of cycles"], dist_rows))
        lines.append("")

        # k1 vs k2 range, from replica-capacity-decision.
        rc_events = [e for e in events
                     if e["_msg"] == "replica-capacity-decision" and e.get("variant") == variant]

        # Per-cycle aggregation, shared by transitions and the detail table
        # below — see build_cycles for why this must be per-cycle, not per-event.
        cycles = build_cycles(rc_events, v_events, sq_by_ts)

        # Transitions: consecutive CYCLES (not raw events) where the aggregate
        # priority label changed.
        lines.append("### Tier transitions")
        lines.append("")
        transitions = []
        prev = None
        for c in cycles:
            cur = c["priority_label"]
            if cur != prev:
                transitions.append((c["time_short"], prev, cur, c["k2"]))
                prev = cur
        if not transitions:
            lines.append("_No transitions recorded._")
        else:
            trans_rows = [[ts, frm or "(start)", to, k2] for ts, frm, to, k2 in transitions]
            lines.extend(md_table(["Time", "From", "To", "k2"], trans_rows))
        lines.append("")

        if rc_events:
            k1_vals = [e["k1MemoryBound"] for e in rc_events if "k1MemoryBound" in e]
            k2_vals = [e["k2ComputeBound"] for e in rc_events if "k2ComputeBound" in e]
            bound_counts = Counter(e.get("boundBy", "?") for e in rc_events)
            lines.append("### Which bound governed (k1-memory vs k2-compute)")
            lines.append("")
            lines.extend(md_table(["Bound", "Cycles"],
                                   [[b, c] for b, c in bound_counts.most_common()]))
            lines.append("")
            if k1_vals and k2_vals:
                lines.append(f"k1 range: {min(k1_vals)} - {max(k1_vals)} tokens  ")
                lines.append(f"k2 range: {min(k2_vals)} - {max(k2_vals)} tokens")
            lines.append("")

        # Per-iteration detail: the same per-cycle aggregation used for
        # transitions above, one row per cycle (not per replica).
        lines.append("### Per-iteration detail")
        lines.append("")
        lines.append("One row per optimize cycle, totalled across every ready replica of this "
                      "variant that cycle (N). KVinUse/LocalQ/EPPq/TotalDemand are all in tokens; "
                      "Priority lists every tier that fired across N replicas this cycle (see the "
                      "fallback-tier distribution above for what each code means). Time is "
                      "HH:MM:SS on the run date above.")
        lines.append("")
        lines.append(f"Legend — Bound: {BOUND_LEGEND}.")
        lines.append("")

        detail_rows = [[
            c["time_short"], c["n"], c["priority_label"], c["k2"], c["k1"], c["bound_label"],
            c["tokens_in_use"], c["local_queue"], c["epp_queue"], c["total_demand"],
        ] for c in cycles]
        lines.extend(md_table(
            ["Time", "N", "Priority", "k2", "k1", "Bound", "KVinUse", "LocalQ", "EPPq", "TotalDemand"],
            detail_rows))
        lines.append("")

    # No-cache-info fallback events (missing vllm:cache_config_info).
    nci_events = [e for e in events if e["_msg"] == "replica-capacity-no-cache-info"]
    if nci_events:
        lines.append("## No-cache-info fallback events")
        lines.append("")
        lines.append("These pods never reported `vllm:cache_config_info`; capacity came from "
                      "the capacity store instead of live KV-cache metrics.")
        lines.append("")
        lines.extend(md_table(
            ["Time", "Variant", "Pod", "Reason"],
            [[e["_ts"], e.get("variant", "?"), e.get("pod", "?"), e.get("reason", "?")]
             for e in nci_events]))
        lines.append("")

    # Zero-replica capacity estimates.
    zr_events = [e for e in events if e["_msg"] == "zero-replica-capacity-estimate"]
    vs_events = [e for e in events if e["_msg"] == "variant-capacity-source"]
    if zr_events or vs_events:
        lines.append("## Zero-replica capacity estimation")
        lines.append("")
        lines.append("How a variant's capacity was estimated while it had no ready replicas.")
        lines.append("")
        zr_rows = []
        for e in zr_events:
            detail = f"perReplicaCapacity={e.get('perReplicaCapacity','?')}"
            if e.get("boundedBy"):
                detail += f", boundedBy={e['boundedBy']}"
            zr_rows.append([e["_ts"], e.get("variant", "?"), e.get("source", "?"), detail])
        for e in vs_events:
            zr_rows.append([e["_ts"], e.get("variant", "?"), "(aggregation)", e.get("reason", "?")])
        lines.extend(md_table(["Time", "Variant", "Source", "Detail"], zr_rows))
        lines.append("")

    if not k2_events and not nci_events and not zr_events and not vs_events:
        lines.append("_No k1/k2 decision events found in the run window. "
                      "Check that the controller image includes the k1/k2 logging "
                      "and that LOG_LEVEL permits INFO output._")
        lines.append("")

    return "\n".join(lines)


if __name__ == "__main__":
    main()
