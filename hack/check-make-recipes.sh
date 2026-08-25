#!/usr/bin/env bash
#
# Finds `@#` comment lines that sit INSIDE a continued make recipe.
#
# A recipe line ending in a backslash continues into the next line, and make
# hands the whole thing to one shell. A `@#` on a continuation line is therefore
# not a comment at all: make never sees it as a recipe line, so it never strips
# the `@`, and the shell tries to run it:
#
#     @#: command not found
#
# This shipped in the model-cache target and was caught only by running it
# against a cluster -- it looks exactly like every other comment in the file.
#
# Written as a script rather than inline awk in the Makefile because make eats
# `$` and backslashes in a recipe, which silently turned the first version of
# this check into `awk: syntax error` followed by "OK" -- a lint that always
# passes, which is worse than no lint at all.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MAKEFILE="$ROOT/Makefile"
[ -f "$MAKEFILE" ] || { echo "no Makefile at $MAKEFILE" >&2; exit 1; }

bad="$(tr -d '\r' < "$MAKEFILE" | awk '
    # Only recipe lines matter (they start with a tab).
    /^\t/ {
        if (cont && $0 ~ /^\t[[:space:]]*@#/) {
            printf "  Makefile:%d: %s\n", NR, $0
            found = 1
        }
        # A trailing backslash continues into the next line.
        cont = ($0 ~ /\\$/)
        next
    }
    { cont = 0 }
    END { exit(found ? 1 : 0) }
')" && { echo "make recipe comments OK"; exit 0; }

echo "ERROR: a @# comment sits inside a continued recipe -- the shell will try to run it:" >&2
printf '%s\n' "$bad" >&2
cat >&2 <<'MSG'

Move the comment ABOVE the recipe (after the target line), or make it a plain
shell comment on its own continued line. Inside a continuation, make does not
strip the `@`, so the shell receives `@#...` as a command.
MSG
exit 1
