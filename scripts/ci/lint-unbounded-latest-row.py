#!/usr/bin/env python3
"""lint-unbounded-latest-row — refuse an unbounded DISTINCT ON over a hypertable.

THE BUG CLASS (#594). `SELECT DISTINCT ON (key) ... FROM <hypertable>` with
no lower bound reads and sorts every row of every chunk to keep one per key.
It is fast on a fresh database and degrades with history, so no test sees it:
SupplyCoverageStats walked all of asset_supply_history on every diagnostics
refresh to count distinct assets.

A query is bounded when the same raw string constrains the hypertable's time
column (read from its create_hypertable call in migrations/) or `ledger` /
`ledger_sequence` with `>`, `>=` or BETWEEN. Anything else must say why:

ESCAPE HATCH: `unbounded-latest-ok: <reason>` in the comment block directly
above the line holding the string, or in the enclosing func's doc comment.
The usual reason is that a floor would drop keys whose last row predates it.
A package-level const/var query has no enclosing func, so only its own
comment block waives it — it never inherits a neighboring func's marker.

NOT DETECTED: a latest-row read shaped as a CTE + window function (a
`ROW_NUMBER() OVER (PARTITION BY ...)` filtered to rn = 1) instead of
`DISTINCT ON` — the same unbounded-scan risk, different SQL shape.

Scans non-test .go files under the given roots (default: internal cmd pkg).
Exit 1 on a finding, 2 when nothing was scanned (a vacuous pass).
"""

import os
import re
import sys

MARKER = re.compile(r"unbounded-latest-ok:\s*\S")
DISTINCT_ON = re.compile(r"\bDISTINCT\s+ON\b", re.I)
FROM_TABLE = re.compile(r'\bFROM\s+([A-Za-z_0-9."]+)', re.I)
HYPERTABLE = re.compile(r"create_hypertable\s*\(\s*'([^']+)'\s*,\s*'([^']+)'", re.S)


def raw_strings(src):
    """(start, text) of each Go raw string literal, skipping comments and
    interpreted strings so a backtick in a doc comment cannot mis-pair."""
    i, n = 0, len(src)
    while i < n:
        c = src[i]
        if src.startswith("//", i):
            j = src.find("\n", i)
            i = n if j < 0 else j
        elif src.startswith("/*", i):
            j = src.find("*/", i + 2)
            i = n if j < 0 else j + 2
        elif c in "\"'":
            j = i + 1
            while j < n and src[j] != c and src[j] != "\n":
                j += 2 if src[j] == "\\" else 1
            i = j + 1
        elif c == "`":
            j = src.find("`", i + 1)
            j = n if j < 0 else j
            yield i, src[i : j + 1]
            i = j + 1
        else:
            i += 1


def hypertables(migrations):
    out = {}
    for name in sorted(os.listdir(migrations)):
        if name.endswith(".up.sql"):
            with open(os.path.join(migrations, name), encoding="utf-8") as fh:
                for m in HYPERTABLE.finditer(fh.read()):
                    out[m.group(1)] = m.group(2)
    return out


def bounded(sql, time_col):
    for col in {time_col, "ledger", "ledger_sequence"}:
        if re.search(r'(?<![A-Za-z_0-9])"?%s"?\s*(>=?|BETWEEN\b)' % re.escape(col), sql, re.I):
            return True
    return False


def comment_block_above(lines, idx):
    """The contiguous // comment lines directly above lines[idx], joined."""
    out = []
    i = idx - 1
    while i >= 0 and lines[i].lstrip().startswith("//"):
        out.append(lines[i])
        i -= 1
    return "\n".join(out)


def waived(lines, idx):
    if MARKER.search(lines[idx]) or MARKER.search(comment_block_above(lines, idx)):
        return True
    for j in range(idx, -1, -1):
        line = lines[j]
        if line == "}":
            # A column-0 closing brace closes a preceding top-level
            # declaration idx is not inside (gofmt never indents at
            # column 0 inside a func body). idx is itself a
            # package-level decl (e.g. a const query) — stop, or a
            # sibling func's marker would wrongly waive it.
            return False
        if line.startswith("func ") and line.count("{") > line.count("}"):
            # Only an *unclosed* func line (no matching "}" on the same
            # line) can be idx's enclosing func; a fully self-contained
            # "func g() {}" above idx is a closed sibling, not a scope
            # idx is inside.
            return bool(MARKER.search(comment_block_above(lines, j)))
    return False


def main(argv):
    roots = argv or ["internal", "cmd", "pkg"]
    here = os.path.dirname(os.path.abspath(__file__))
    repo = os.path.normpath(os.path.join(here, "..", ".."))
    migrations = os.environ.get("MIGRATIONS_DIR", os.path.join(repo, "migrations"))
    tables = hypertables(migrations)
    if not tables:
        print(f"lint-unbounded-latest-row: FAIL — no create_hypertable in {migrations}; the gate would be vacuous", file=sys.stderr)
        return 2

    checked = 0
    findings = 0
    for root in roots:
        for dirpath, _, files in os.walk(root):
            for name in sorted(files):
                if not name.endswith(".go") or name.endswith("_test.go"):
                    continue
                path = os.path.join(dirpath, name)
                checked += 1
                with open(path, encoding="utf-8", errors="replace") as fh:
                    src = fh.read()
                if not DISTINCT_ON.search(src):
                    continue
                lines = src.split("\n")
                for lit_start, sql in raw_strings(src):
                    for d in DISTINCT_ON.finditer(sql):
                        m = FROM_TABLE.search(sql, d.end())
                        if not m:
                            continue
                        table = m.group(1).split(".")[-1].strip('"')
                        if table not in tables or bounded(sql, tables[table]):
                            continue
                        start = src.count("\n", 0, lit_start)
                        if waived(lines, start):
                            continue
                        line = src.count("\n", 0, lit_start + d.start()) + 1
                        print(
                            f"lint-unbounded-latest-row: {path}:{line} DISTINCT ON over hypertable "
                            f"{table} with no bound on {tables[table]} or ledger — add a floor, "
                            f"or state why none is honest with `// unbounded-latest-ok: <reason>`"
                        )
                        findings += 1

    if checked == 0:
        print(f"lint-unbounded-latest-row: FAIL — no Go files found under {' '.join(roots)}; the gate would be vacuous", file=sys.stderr)
        return 2
    if findings:
        print(f"lint-unbounded-latest-row: FAIL — {findings} unbounded latest-row read(s) in {checked} file(s)", file=sys.stderr)
        return 1
    print(f"lint-unbounded-latest-row: OK — {checked} production Go file(s), {len(tables)} hypertable(s), no unwaived unbounded DISTINCT ON")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
