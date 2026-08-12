#!/usr/bin/env python3
"""Check every relative markdown link and heading anchor under docs/ and the READMEs.

Fenced code blocks are skipped: Go generics like `lru.New[podKey, chainNode](size)`
read as a markdown link to any regex that does not know where the fences are, and
a checker that cries wolf gets ignored, which is worse than not having one.
"""
import re, sys, pathlib

ROOT = pathlib.Path(__file__).resolve().parent.parent
FENCE = re.compile(r"^\s*(```|~~~)")

def strip_code(text):
    out, inside = [], False
    for line in text.splitlines():
        if FENCE.match(line):
            inside = not inside
            out.append("")
            continue
        out.append("" if inside else line)
    return "\n".join(out)

def slugs(text):
    return {re.sub(r"\s", "-", re.sub(r"[^\w\s-]", "", m.group(2).strip().lower()))
            for m in (re.match(r"^(#{1,6})\s+(.*)$", l) for l in text.splitlines()) if m}

def main():
    files = [ROOT / "README.md"] + sorted(ROOT.glob("deploy/**/*.md")) + sorted(ROOT.glob("docs/**/*.md"))
    bad = 0
    for p in files:
        raw = p.read_text(encoding="utf-8")
        prose = strip_code(raw)
        own = slugs(raw)
        for link in re.findall(r"\]\(([^)\s]+)\)", prose):
            if link.startswith(("http", "mailto:")):
                continue
            target, _, anchor = link.partition("#")
            if target:
                f = (p.parent / target).resolve()
                if not f.exists():
                    print(f"BROKEN FILE   {p.relative_to(ROOT)} -> {link}"); bad += 1; continue
                if anchor and f.suffix == ".md" and anchor not in slugs(f.read_text(encoding="utf-8")):
                    print(f"BROKEN ANCHOR {p.relative_to(ROOT)} -> {link}"); bad += 1
            elif anchor and anchor not in own:
                print(f"BROKEN ANCHOR {p.relative_to(ROOT)} -> #{anchor}"); bad += 1
    print(f"{len(files)} files checked, {bad} broken")
    return 1 if bad else 0

if __name__ == "__main__":
    sys.exit(main())
