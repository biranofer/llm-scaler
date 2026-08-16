#!/usr/bin/env bash
# Apply our fixes to the llm-d-benchmark clone.
#
# The clone is gitignored (.gitignore:33) and benchmark-install re-checks it out
# to a pinned tag, so anything edited in place is invisible and disappears on the
# next install. Fixes therefore live here, in a versioned script, and are
# reapplied after every install.
#
# Both bugs below are upstream's, both are present in v0.7.8 AND on origin/main,
# so there is no release to upgrade to. Both are reported upstream; delete the
# corresponding block here when a release carries the fix.
#
# Why patching the clone reaches the cluster at all: step_06 builds the
# `llmdbench-harness-scripts` ConfigMap from the CHECKED-OUT tree --
# workload/harnesses/* plus llmdbenchmark/analysis/scripts/*-analyze_results.*
# -- and mounts it into the harness pod, "so a run can use a new/updated harness
# with an older benchmark image". Files outside those two sets (notably
# benchmark_report/native_to_br0_2.py) ship only in the image and cannot be
# fixed from here.
#
# Every edit is anchored on an exact upstream string and is idempotent. A
# missing anchor is a hard error, not a skip: a silent no-op after a version
# bump would leave us believing a fix is applied when it is not, which is the
# same failure mode that let an unversioned maxReplicas sit in the clone for
# weeks.
set -eu

REPO_DIR="${1:?usage: patch_harness.sh <llm-d-benchmark clone dir>}"
[ -d "$REPO_DIR" ] || { echo "patch_harness: no clone at $REPO_DIR" >&2; exit 1; }

PY=${PYTHON:-python3}
applied=0
skipped=0

note()  { printf '  %s\n' "$1"; }
fail()  { printf 'patch_harness: %s\n' "$1" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Fix 1 -- process_epp_logs.py: EPP emits "ts" as a float, not an ISO string.
#
#   ts_str = re.sub(r"(\.\d{6})\d+", r"\1", ts_str)
#   TypeError: expected string or bytes-like object, got 'float'
#
# EPP writes {"ts":1786849846.9172602,...} on EVERY line, so the parser dies on
# the first entry and every EPP metric is discarded. That is why "Avg queue
# depth (EPP)" has been "?" in every run we have ever taken -- 49.8 MB of logs
# per run, parsed to nothing. The harness truncates stderr to 200 chars and
# calls it non-fatal, which is why the cause stayed hidden.
# ---------------------------------------------------------------------------
EPP="$REPO_DIR/workload/harnesses/process_epp_logs.py"
[ -f "$EPP" ] || fail "expected file missing: $EPP"

"$PY" - "$EPP" <<'PYEOF' || fail "fix 1 (process_epp_logs.py) failed"
import io, sys

path = sys.argv[1]
src = io.open(path, encoding="utf-8").read()

MARK = "# wva-patch: numeric ts"
if MARK in src:
    print("  fix 1 (EPP float ts): already applied")
    sys.exit(0)

IMPORT_OLD = "from datetime import datetime\n"
IMPORT_NEW = "from datetime import datetime, timezone\n"

# Anchor on the exact line that raises, and insert the numeric branch ahead of
# it -- after the falsy guard, so ts=0/"" keeps returning None as before.
ANCHOR = '    # Handle nanosecond timestamps by truncating to 6 decimal places\n'
BRANCH = (
    MARK + ": EPP logs carry epoch seconds as a JSON number, not an ISO\n"
    "    # string. re.sub() then raises TypeError on the first entry and the whole\n"
    "    # log is dropped. Naive UTC here matches the ISO path below, which strips\n"
    "    # the trailing Z and returns a naive datetime -- mixing the two would make\n"
    "    # every downstream subtraction raise.\n"
    "    if isinstance(ts_str, (int, float)) and not isinstance(ts_str, bool):\n"
    "        return datetime.fromtimestamp(ts_str, timezone.utc).replace(tzinfo=None)\n"
)

if IMPORT_OLD not in src:
    sys.exit("anchor missing: %r" % IMPORT_OLD)
if ANCHOR not in src:
    sys.exit("anchor missing: %r" % ANCHOR)

src = src.replace(IMPORT_OLD, IMPORT_NEW, 1)
src = src.replace(ANCHOR, "    " + BRANCH + ANCHOR, 1)

io.open(path, "w", encoding="utf-8", newline="\n").write(src)
print("  fix 1 (EPP float ts): applied")
PYEOF

# ---------------------------------------------------------------------------
# Fix 2 -- guidellm-analyze_results.sh: stop a broken report conversion from
# failing an otherwise good run.
#
#   native_to_br0_2.py:2751  native["config"] = data["args"]
#   KeyError: 'args'
#
# benchmark-report expects guidellm's old top-level "args" (its invocation
# arguments: model, data sources, rate). Current guidellm emits "metadata",
# "config", "benchmarks" and no "args" at all, so the lookup always raises --
# the guard above it, `if not native.get("config")`, never helps because
# nothing populates it for guidellm (benchmark-report has no config-file flag;
# "-w guidellm" names the generator, not a file).
#
# We do NOT alias "config" onto "args". They are not the same thing: "config"
# holds the llm-d-benchmark workload profile (metadata/spec/benchmarks --
# prefill_heavy and friends), not guidellm's arguments. Aliasing gets past the
# KeyError and then dies on `get_nested(data, ["args","data"])` returning None
# two lines later; worse, had it survived, it would have written a report whose
# recorded config was the wrong document entirely. A silently wrong report is
# worse than no report. Mapping the new schema onto the old is a real,
# version-coupled job and belongs upstream.
#
# So: keep reporting the failure loudly, but do not propagate it. The pod's
# entrypoint does `exit $((LOADGEN_EC + REPORT_EC))`, so a failed conversion
# marks the whole run FAILED and burns MAX_TRIES x 30s retrying a deterministic
# error. That cost is real and the artifact is not: nothing we run consumes
# benchmark_report v0.1/v0.2 -- postprocess.py reads results.json directly and
# the replica timeline comes from our own sampler. The FAILED banner is not
# free either; it is what led us to misread a valid run as a lost one.
#
# The conversion itself runs inside the pod against code baked into the image
# (/opt/benchmark_report), so it cannot be fixed from here at all. Only the
# analyzer script can, because step_06 ships it from this clone.
# ---------------------------------------------------------------------------
ANALYZER="$REPO_DIR/llmdbenchmark/analysis/scripts/guidellm-analyze_results.sh"
[ -f "$ANALYZER" ] || fail "expected file missing: $ANALYZER"

"$PY" - "$ANALYZER" <<'PYEOF' || fail "fix 2 (guidellm-analyze_results.sh) failed"
import io, sys

path = sys.argv[1]
src = io.open(path, encoding="utf-8").read()

MARK = "# wva-patch: conversion is not fatal"
if MARK in src:
    print("  fix 2 (report conversion non-fatal): already applied")
    sys.exit(0)

ANCHOR = (
    'if [[ $LLMDBENCH_RUN_EXPERIMENT_CONVERT_RC -ne 0 ]]; then\n'
    '  echo "Results data conversion completed with errors."\n'
    '  exit $LLMDBENCH_RUN_EXPERIMENT_CONVERT_RC\n'
    'fi\n'
)
if ANCHOR not in src:
    sys.exit("anchor missing (upstream shape changed): %r" % ANCHOR)

REPLACEMENT = (
    'if [[ $LLMDBENCH_RUN_EXPERIMENT_CONVERT_RC -ne 0 ]]; then\n'
    '  echo "Results data conversion completed with errors."\n'
    '  ' + MARK + '\n'
    '  # benchmark-report cannot read current guidellm output (it wants a\n'
    '  # top-level "args" that guidellm no longer emits). The measurement data\n'
    '  # in results.json is unaffected and is what we actually consume, so do\n'
    '  # not fail the run over the derived report: the entrypoint exits\n'
    '  # $((LOADGEN_EC + REPORT_EC)), which would mark a good run FAILED and\n'
    '  # retry a deterministic error MAX_TRIES x 30s.\n'
    '  echo "NOTE: benchmark_report v0.1/v0.2 were NOT produced (upstream bug,'
    ' present in v0.7.8 and main)."\n'
    '  echo "NOTE: results.json is complete; treat this run as valid."\n'
    '  LLMDBENCH_RUN_EXPERIMENT_CONVERT_RC=0\n'
    'fi\n'
)

src = src.replace(ANCHOR, REPLACEMENT, 1)
io.open(path, "w", encoding="utf-8", newline="\n").write(src)
print("  fix 2 (report conversion non-fatal): applied")
PYEOF

echo "patch_harness: done ($REPO_DIR)"
