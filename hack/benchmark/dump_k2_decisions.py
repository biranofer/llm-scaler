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

        # Transitions: consecutive cycles where the priority changed.
        lines.append("### Tier transitions")
        lines.append("")
        transitions = []
        prev = None
        for e in v_events:
            cur = e.get("priority")
            if cur != prev:
                transitions.append((e["_ts"], prev, cur, e))
                prev = cur
        if not transitions:
            lines.append("_No transitions recorded._")
        else:
            trans_rows = [[ts, frm or "(start)", to, e.get("k2", e.get("k1", "?"))]
                          for ts, frm, to, e in transitions]
            lines.extend(md_table(["Time", "From", "To", "k2 (or k1 on P4)"], trans_rows))
        lines.append("")

        # k1 vs k2 range, from replica-capacity-decision.
        rc_events = [e for e in events
                     if e["_msg"] == "replica-capacity-decision" and e.get("variant") == variant]
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

        # Per-iteration detail: every cycle's k2-decision joined with that same
        # cycle's replica-capacity-decision. computeReplicaCapacity logs exactly
        # one k2-decision then exactly one replica-capacity-decision per replica,
        # per cycle, in that order — so at a timestamp shared by several ready
        # replicas, pairing by POSITION (not a full cross-product) is the correct
        # join. Ambiguous only if the two counts disagree, which would mean one
        # of the two log calls didn't fire for some replica.
        lines.append("### Per-iteration detail")
        lines.append("")
        lines.append("Every optimize cycle's k2 computation, with the inputs behind whichever "
                      "fallback tier fired and the resulting k1-vs-k2 comparison. Rows sharing a "
                      "timestamp are separate ready replicas of this variant in that cycle. Time "
                      "is HH:MM:SS on the run date above; see legend for the Inputs/Bound codes.")
        lines.append("")
        lines.append(f"Legend — Inputs: {INPUTS_LEGEND}.  Bound: {BOUND_LEGEND}.")
        lines.append("")
        rc_by_ts = defaultdict(list)
        for e in rc_events:
            rc_by_ts[e["_ts"]].append(e)
        rc_cursor = defaultdict(int)

        detail_rows = []
        for e in v_events:
            ts = e["_ts"]
            time_short = ts.split("T")[1].split("+")[0] if "T" in ts else ts
            priority = e.get("priority", "?")
            inputs = format_priority_inputs(priority, e)

            pool = rc_by_ts.get(ts, [])
            idx = rc_cursor[ts]
            rc = pool[idx] if idx < len(pool) else None
            rc_cursor[ts] += 1

            if rc is None:
                detail_rows.append([time_short, priority, inputs, e.get("k2", e.get("k1", "?")),
                                     "?", "?", "?", "?"])
                continue
            detail_rows.append([
                time_short, priority, inputs,
                rc.get("k2ComputeBound", "?"), rc.get("k1MemoryBound", "?"),
                format_bound(rc.get("boundBy", "?")), rc.get("replicaDemand", "?"),
                f"{rc.get('queueLength','?')}/{rc.get('queueThreshold','?')}",
            ])
        lines.extend(md_table(
            ["Time", "Priority", "Inputs", "k2", "k1", "Bound", "Demand", "Queue"],
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
