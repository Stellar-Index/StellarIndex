#!/usr/bin/env python3
"""Refuse a typographic quote character in tracked Go source.

WHY THIS EXISTS (2026-09-10). `make fmt` runs gofumpt, and gofumpt runs
go/doc/comment over every DOC comment. That printer still applies Go's
legacy TeX-style quoting rule: a doubled apostrophe becomes U+201D and a
doubled backtick becomes U+201C. So a doc comment that quoted a SQL
predicate was silently rewritten::

    - // Doc says the filter is `entry_xdr != ''` applied to the row FINAL kept.
    + // Doc says the filter is `entry_xdr != ”` applied to the row FINAL kept.
      const Q = `SELECT 1 WHERE entry_xdr != '' AND a = ''`   <- unchanged

The literal is untouched, so nothing breaks, nothing fails, and no test
notices. What changes is the sentence that documents the invariant — and
`entry_xdr != ''` is a predicate this tree writes down constantly, because
it is how the ClickHouse readers exclude tombstoned rows. The comment that
explained a load-bearing filter stopped saying what the filter is.

TWO PROPERTIES MAKE IT NASTY, and both are why this gate exists rather
than a formatter run.

  1. THE CORRUPTION IS FMT-STABLE. U+201D is a fixed point: `gofumpt -l`
     over the mangled file lists nothing. "The formatter ran and the tree
     was clean" is therefore not evidence of anything here — one corrupted
     occurrence survived exactly that check. Only a scan for the character
     itself can see it.

  2. THE OBVIOUS SCAN LIES. The first sweep for these characters used a
     basic regular expression with `\\|` alternation and reported the tree
     clean. Alternation is not part of POSIX BRE; a grep that does not
     implement the GNU extension matches a literal `|` instead and exits 1
     — indistinguishable from "no hits". Verified on the machine this was
     written on: /usr/bin/grep is BSD grep 2.6.0-FreeBSD and rejects `-P`
     outright (`invalid option -- P`, exit 2), which is at least loud;
     `\\|` is the quiet failure mode, and it is quiet in exactly the
     direction that lets a defect through.

WHY PYTHON AND NOT GREP. Both of the above are grep-shaped problems, and
the portable-grep answer (`grep -E` with four alternates, spelled in raw
UTF-8, over a `git ls-files -z` list, with a per-hit column and a waiver
scan) is longer and less legible than the Python. Python also decodes the
file rather than matching bytes, so it can name the codepoint it found,
report a column, and treat a Go file that is not valid UTF-8 as its own
finding. scripts/ci/lint-yaml-duplicate-keys.py and
scripts/ci/lint_textfile_exposition.py set the precedent; `python3` is
already a hard requirement of scripts/dev/verify.sh's tool preflight.

SCOPE. Tracked Go files, from `git ls-files`, minus two exclusions:

  - vendored trees (a `vendor/`, `third_party/` or `node_modules/` path
    segment) — not ours to reword; and
  - files carrying Go's standard generated marker,
    `// Code generated ... DO NOT EDIT.`, before the package clause. A
    generated file's comments come from a generator, so a finding there is
    not actionable at the file and would have to be waived on every
    regeneration.

testdata/ and fixtures are deliberately IN scope. A corrupted comment in a
fixture is still a corrupted comment, and the tree has no Go fixture that
needs the character today; the waiver below covers the one that eventually
will (a fixture asserting this very mangling).

FALSE POSITIVES. A typographic quote is occasionally correct — prose in a
user-facing string, a test asserting the mangling. Those are per-LINE
facts, not per-file ones, so the escape hatch is an inline waiver rather
than a baseline file:

    lint-quotes:ok: <reason>

on the offending line or the line directly above it. A reason is required;
`lint-quotes:ok` alone does not waive. This follows the tree's existing
inline-waiver markers (`# sigpipe-ok: ...`, `-- lint-money:ok ...`) rather
than scripts/ci/*.baseline, and for a reason worth stating: a baseline
records that a file is dirty somewhere, while the exception here needs to
sit on the character, next to the sentence that explains why it is not the
formatter's doing. Waivers are counted in the summary line, so they stay
visible instead of accumulating quietly.

WHAT THIS DOES NOT CHECK. It cannot tell a mangled `''` from a smart quote
pasted out of a document — it reports the character and lets the failure
text explain both origins. It says nothing about Markdown, YAML or
TypeScript, where a typographic quote is usually intentional. And it does
not stop gofumpt from mangling the NEXT comment; nothing can, short of
writing the comment in a form the rule does not touch, which is what the
failure text asks for.

Usage:
    lint-go-typographic-quotes.py               # every tracked Go file
    lint-go-typographic-quotes.py <file> ...    # an explicit list
Env: GO_QUOTES_LINT_ROOT overrides the tree to scan (used by the self-test).
Exit 0 clean, 1 on a finding, 2 on a usage/scope error.
"""

from __future__ import annotations

import os
import re
import subprocess
import sys
from pathlib import Path

TOOL = "lint-go-typographic-quotes"

# The four characters. The two gofumpt can PRODUCE are marked; the other
# two are here because a pasted smart quote is the same defect wearing a
# different hat — a character that reads as a quote and is not one.
QUOTES = {
    "‘": "U+2018 LEFT SINGLE QUOTATION MARK",
    "’": "U+2019 RIGHT SINGLE QUOTATION MARK",
    "“": "U+201C LEFT DOUBLE QUOTATION MARK (gofumpt renders `` as this)",
    "”": "U+201D RIGHT DOUBLE QUOTATION MARK (gofumpt renders '' as this)",
}

# Go's own marker, matched as cmd/go matches it: the whole line, before the
# package clause.
GENERATED_RE = re.compile(r"^// Code generated .* DO NOT EDIT\.$")

# A reason is mandatory — the bare marker must not waive anything.
WAIVER_RE = re.compile(r"lint-quotes:ok:?[ \t]+\S")

VENDORED_PARTS = {"vendor", "third_party", "node_modules"}


def is_vendored(rel: str) -> bool:
    return bool(VENDORED_PARTS & set(Path(rel).parts))


def is_generated(text: str) -> bool:
    for line in text.splitlines()[:20]:
        if line.startswith("package "):
            return False
        if GENERATED_RE.match(line):
            return True
    return False


def tracked_go_files(root: Path) -> list[str]:
    """Every tracked *.go path, relative to root."""
    try:
        out = subprocess.run(
            ["git", "-C", str(root), "ls-files", "-z", "--", "*.go"],
            capture_output=True,
            text=True,
            check=True,
        ).stdout
    except (OSError, subprocess.CalledProcessError) as exc:
        print(f"{TOOL}: cannot list tracked files under {root} ({exc})", file=sys.stderr)
        sys.exit(2)
    return sorted(p for p in out.split("\0") if p)


def looks_like_comment(line: str) -> bool:
    """Heuristic, and only ever used to word the hint."""
    stripped = line.lstrip()
    return stripped.startswith(("//", "/*", "*"))


def scan(root: Path, rel: str, findings: list[str]) -> int:
    """Append findings for one file; return the number of waived lines."""
    try:
        text = (root / rel).read_text(encoding="utf-8")
    except UnicodeDecodeError as exc:
        findings.append(f"{rel}: not valid UTF-8 ({exc}) — Go source must be UTF-8")
        return 0
    except OSError as exc:
        findings.append(f"{rel}: unreadable ({exc})")
        return 0

    if not QUOTES.keys() & set(text):
        return 0

    waived = 0
    lines = text.splitlines()
    for n, line in enumerate(lines, start=1):
        hits = [(i, ch) for i, ch in enumerate(line, start=1) if ch in QUOTES]
        if not hits:
            continue
        above = lines[n - 2] if n >= 2 else ""
        if WAIVER_RE.search(line) or WAIVER_RE.search(above):
            waived += 1
            continue
        col, ch = hits[0]
        where = "in a comment" if looks_like_comment(line) else "in source"
        extra = f" (+{len(hits) - 1} more on this line)" if len(hits) > 1 else ""
        findings.append(
            f"{rel}:{n}:{col}: {QUOTES[ch]} {where}{extra}\n    {line.strip()}"
        )
    return waived


def candidates(root: Path, argv: list[str]) -> list[str]:
    if argv:
        rels = []
        for a in argv:
            p = Path(a)
            try:
                rels.append(str(p.resolve().relative_to(root.resolve())))
            except ValueError:
                rels.append(str(p))
        return rels
    return tracked_go_files(root)


def main(argv: list[str]) -> int:
    root = Path(os.environ.get("GO_QUOTES_LINT_ROOT") or Path(__file__).resolve().parents[2])
    if not root.is_dir():
        print(f"{TOOL}: root {root} is not a directory", file=sys.stderr)
        return 2

    everything = candidates(root, argv)
    scanned: list[str] = []
    skipped = 0
    for rel in everything:
        if not rel.endswith(".go"):
            skipped += 1
            continue
        if is_vendored(rel):
            skipped += 1
            continue
        path = root / rel
        if not path.is_file():
            skipped += 1
            continue
        try:
            head = path.read_bytes()[:2048].decode("utf-8", errors="replace")
        except OSError:
            head = ""
        if is_generated(head):
            skipped += 1
            continue
        scanned.append(rel)

    if not scanned:
        print(
            f"{TOOL}: no Go files in scope ({len(everything)} candidate(s), "
            f"{skipped} skipped as vendored/generated/non-Go) — refusing to pass vacuously",
            file=sys.stderr,
        )
        return 2

    findings: list[str] = []
    waived = 0
    for rel in scanned:
        waived += scan(root, rel, findings)

    if findings:
        for f in findings:
            print(f"{TOOL}: {f}", file=sys.stderr)
        print(HELP, file=sys.stderr)
        print(
            f"{TOOL}: FAIL — {len(findings)} typographic quote(s) across "
            f"{len(scanned)} Go file(s) ({skipped} skipped, {waived} waived)",
            file=sys.stderr,
        )
        return 1

    print(
        f"{TOOL}: OK — {len(scanned)} Go file(s), no typographic quotes "
        f"({skipped} skipped as vendored/generated/non-Go, {waived} waived)"
    )
    return 0


HELP = """
CAUSE: `make fmt` runs gofumpt, whose DOC-COMMENT reformatter applies Go's
legacy TeX quoting rule — a doubled apostrophe ('') becomes U+201D and a
doubled backtick becomes U+201C. String literals and non-doc comments are
untouched, so only the prose changed and nothing failed.

DO NOT JUST RETYPE ''. The next `make fmt` mangles it again, and the
mangled form is a FIXED POINT — re-running the formatter reports nothing,
so a clean `gofumpt -l` is not evidence the comment is intact.

Two fixes that survive the formatter:

  1. Put the code in an indented code block inside the doc comment. gofmt
     does not requote a code block, so '' survives verbatim:

         // The row FINAL keeps is the one matching:
         //
         //\tentry_xdr != ''
         //

  2. Or reword the prose so the doubled apostrophe never occurs —
     "entry_xdr is not the empty string".

If the character is deliberate (prose in a user-facing string, a fixture
asserting this mangling), waive that line with a reason, on the line or
the line directly above it:

    lint-quotes:ok: <why this character belongs here>
"""


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
