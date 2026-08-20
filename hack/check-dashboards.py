#!/usr/bin/env python3
"""Check the Grafana dashboards under deploy/grafana/ for defects a reader cannot see.

Grafana repacks a dashboard on load, so a broken layout still displays -- just not
the layout the JSON describes. Nine overlapping panel pairs sat in the operational
dashboard for months that way, each new panel appended at coordinates that
collided with one already there. Nothing failed; the dashboard simply showed an
arbitrary arrangement.

The same blindness covers the namespace labels. wva_* series carry the workload
namespace as `exported_namespace` and the controller's as `namespace`, so the
dashboards offer a `$namespace_label` variable to choose between them. Engine and
EPP series carry only a plain `namespace`, so using the variable there empties the
panel for whoever picks the other value -- and dropping the matcher altogether
plots every tenant on the cluster, which is how the EPP queue panel came to draw
three models for a namespace that serves one.

Checked here because none of it needs a cluster, a datasource or a browser.
"""
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
DASHBOARDS = sorted((ROOT / "deploy" / "grafana").glob("*-dashboard.json"))

GRID_COLUMNS = 24

# Series whose namespace label is the WORKLOAD's, chosen by the reader.
WVA = re.compile(r"^wva_")
# Series emitted by the EPP or the engines. These carry a plain `namespace` only.
WORKLOAD = re.compile(r"^(llm_d_epp_|inference_|vllm:|sglang:)")
METRIC = re.compile(r"\b(wva_[a-z0-9_]+|llm_d_epp_[a-z0-9_]+|inference_[a-z0-9_]+)\b|(vllm:[a-z0-9_:]+|sglang:[a-z0-9_:]+)")
LABEL_NAME = re.compile(r"([$\w]+)\s*(?:=~|!~|=|!=)")


def panels_of(dashboard):
    """Every panel, rows included, as (panel, siblings-key) pairs."""
    for panel in dashboard.get("panels", []):
        yield panel, "top"
        for child in panel.get("panels") or []:
            yield child, f"row:{panel.get('title', panel.get('id'))}"


def overlaps(a, b):
    ax, ay, aw, ah = (a["gridPos"][k] for k in "xywh")
    bx, by, bw, bh = (b["gridPos"][k] for k in "xywh")
    return ax < bx + bw and bx < ax + aw and ay < by + bh and by < ay + ah


def check_layout(name, dashboard, fail):
    groups = {}
    for panel, group in panels_of(dashboard):
        pos = panel.get("gridPos")
        title = panel.get("title") or f"id {panel.get('id')}"
        if not pos or not all(k in pos for k in "xywh"):
            fail(f"NO GRIDPOS    {name}: {title}")
            continue
        if pos["x"] < 0 or pos["y"] < 0 or pos["w"] < 1 or pos["h"] < 1:
            fail(f"BAD GRIDPOS   {name}: {title} {pos}")
            continue
        if pos["x"] + pos["w"] > GRID_COLUMNS:
            fail(f"OFF GRID      {name}: {title} ends at column {pos['x'] + pos['w']} of {GRID_COLUMNS}")
        groups.setdefault(group, []).append(panel)

    for group, members in groups.items():
        for i, a in enumerate(members):
            for b in members[i + 1:]:
                if overlaps(a, b):
                    fail(f"OVERLAP       {name} [{group}]: "
                         f"{a.get('title')} {a['gridPos']} vs {b.get('title')} {b['gridPos']}")


def check_identity(name, dashboard, fail):
    """Ids address a panel for rendering; titles name the file it renders to."""
    seen_ids, seen_titles = {}, {}
    for panel, _ in panels_of(dashboard):
        pid, title = panel.get("id"), panel.get("title")
        if pid is None:
            fail(f"NO ID         {name}: {title}")
        elif pid in seen_ids:
            fail(f"DUPLICATE ID  {name}: {title} and {seen_ids[pid]} are both id {pid}")
        else:
            seen_ids[pid] = title
        if panel.get("type") == "row":
            continue
        if not title:
            fail(f"NO TITLE      {name}: panel id {pid}")
        elif title.lower() in seen_titles:
            fail(f"DUPLICATE     {name}: two panels titled {title!r} render to one image")
        else:
            seen_titles[title.lower()] = pid


def selectors(expr):
    """Yield (metric, label-names) for every metric reference in a PromQL expression."""
    for match in METRIC.finditer(expr):
        metric = match.group(0)
        rest = expr[match.end():]
        braces = re.match(r"\s*\{([^}]*)\}", rest)
        yield metric, set(LABEL_NAME.findall(braces.group(1))) if braces else set()


def check_namespace_labels(name, dashboard, fail):
    for panel, _ in panels_of(dashboard):
        title = panel.get("title")
        for target in panel.get("targets") or []:
            expr = (target.get("expr") or "").strip()
            for metric, labels in selectors(expr):
                if WVA.match(metric):
                    wrong = labels & {"namespace", "exported_namespace"}
                    if wrong:
                        fail(f"FIXED LABEL   {name}: {title}: {metric} matches on "
                             f"{'/'.join(sorted(wrong))}; wva_* series must use $namespace_label")
                elif WORKLOAD.match(metric):
                    if "$namespace_label" in labels or "exported_namespace" in labels:
                        fail(f"VARIED LABEL  {name}: {title}: {metric} carries a plain `namespace` "
                             f"only; $namespace_label empties it whenever a reader picks the other value")
                    elif "namespace" not in labels:
                        fail(f"UNSCOPED      {name}: {title}: {metric} has no namespace matcher, "
                             f"so it plots every tenant on the cluster")


def check_format(name, text, dashboard, fail):
    """Both files are exactly json.dumps(indent=2). Keeping them that way is what
    makes a one-line change show up as a one-line diff -- a Grafana export or a
    hand edit that reflows the file buries the change in a thousand lines."""
    if text != json.dumps(dashboard, indent=2):
        fail(f"REFORMATTED   {name}: not json.dumps(indent=2); re-dump it so diffs stay readable")


def main():
    failures = []
    if not DASHBOARDS:
        print("no dashboards found under deploy/grafana/")
        return 1
    for path in DASHBOARDS:
        text = path.read_text(encoding="utf-8").replace("\r\n", "\n").rstrip("\n")
        dashboard = json.loads(text)
        name = path.name
        check_format(name, text, dashboard, failures.append)
        check_layout(name, dashboard, failures.append)
        check_identity(name, dashboard, failures.append)
        check_namespace_labels(name, dashboard, failures.append)

    for line in failures:
        print(line)
    panels = sum(len(list(panels_of(json.loads(p.read_text(encoding="utf-8"))))) for p in DASHBOARDS)
    print(f"{len(DASHBOARDS)} dashboards, {panels} panels checked, {len(failures)} problem(s)")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
