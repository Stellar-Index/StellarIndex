#!/usr/bin/env python3
"""YAML-aware guard: the alerts catalogue's Severity column must equal the
rule's own `labels.severity`.

WHY THIS EXISTS (issue #362, 2026-09-02):
`docs/operations/alerts-catalog.md` is what a responder reads to decide how
urgently to react. Its Severity column read `P1`/`P2`/`P3` and the legend
claimed those mapped to SEV-1/2/3 — but no rule has ever carried a `P*`
severity label, so the column was not the routing key and nothing could
check it. 190 of the 203 rows disagreed with the rule they described.
The dangerous class was `P3`: it reads as "low but escalating" and was in
fact `informational` for 15 alerts, which `alertmanager.r1.yml` routes to
`receiver: silent` — a receiver with no webhook, i.e. **zero delivery**.
`stellarindex_ingestion_orphan_events` and `..._decode_error` were both in
that set. lint-docs.sh §10 only checked that each alert NAME appears
somewhere in the doc, which every one of those rows satisfied.

This check PARSES both rule trees and asserts, per alert:
  * every rule has a catalogue row and every catalogue row has a rule
    (bidirectional — §10 is one-directional and text-based);
  * the row's Severity cell equals the rule's `labels.severity`;
  * the two rule trees agree on that severity (they are kept in step by
    lint-rule-equivalence; a divergence here would make "the" severity
    ambiguous).

Pure-Python (PyYAML); mirrors lint-runbook-annotations.py so it runs
anywhere verify.sh does. Invoked from lint-docs.sh §10.
"""
import glob
import os
import re
import sys

try:
    import yaml
except ImportError:
    # Fail-closed, not skip — the same reasoning as lint-runbook-annotations.py.
    # A silent exit-0 on a missing dependency is how a gate reports "clean"
    # by never running.
    print("lint-alerts-catalog: FAIL — PyYAML is required (pip install pyyaml)", file=sys.stderr)
    sys.exit(2)

CATALOG = "docs/operations/alerts-catalog.md"
TREES = ("deploy/monitoring/rules/*.yml", "configs/prometheus/rules.r1/*.yml")
ROW = re.compile(r"^\|\s*`(stellarindex_[A-Za-z0-9_]+)`\s*\|")
VALID = ("page", "ticket", "informational")


def load_tree(pattern):
    """alert name -> severity, for one rule tree."""
    out = {}
    for path in sorted(glob.glob(pattern)):
        with open(path, encoding="utf-8") as fh:
            doc = yaml.safe_load(fh)
        for group in (doc or {}).get("groups", []) or []:
            for rule in group.get("rules", []) or []:
                name = rule.get("alert")
                if name:
                    out[name] = (rule.get("labels") or {}).get("severity", "")
    return out


def split_cells(row):
    """Split a markdown table row on `|`, ignoring pipes inside `code spans`.

    PromQL cells routinely contain `{a=~"x|y"}`, so a naive split shifts every
    column after it and silently reads the wrong cell as the severity.
    """
    cells, cur, in_code = [], [], False
    for ch in row:
        if ch == "`":
            in_code = not in_code
        if ch == "|" and not in_code:
            cells.append("".join(cur).strip())
            cur = []
        else:
            cur.append(ch)
    cells.append("".join(cur).strip())
    return cells


def main():
    problems = []

    trees = {}
    for pattern in TREES:
        loaded = load_tree(pattern)
        if not loaded:
            print(f"lint-alerts-catalog: FAIL — no alert rules found under {pattern} "
                  "(bad glob or moved tree?); this gate must not pass vacuously",
                  file=sys.stderr)
            return 2
        trees[pattern] = loaded

    primary_pattern, rules = next(iter(trees.items()))
    for pattern, other in list(trees.items())[1:]:
        for name in sorted(set(rules) | set(other)):
            a, b = rules.get(name), other.get(name)
            if a != b:
                problems.append(
                    f"{name}: severity differs between rule trees "
                    f"({primary_pattern} = {a!r}, {pattern} = {b!r}) — "
                    "the catalogue cannot describe both")

    if not os.path.isfile(CATALOG):
        print(f"lint-alerts-catalog: FAIL — {CATALOG} not found", file=sys.stderr)
        return 2

    doc_rows = {}
    with open(CATALOG, encoding="utf-8") as fh:
        for line in fh:
            m = ROW.match(line)
            if not m:
                continue
            cells = split_cells(line)
            # ['', Name, Metric, Condition, Severity, Runbook, '']
            if len(cells) != 7:
                problems.append(
                    f"{m.group(1)}: catalogue row has {len(cells) - 2} columns, expected 5 "
                    "(Name | Metric | Condition | Severity | Runbook)")
                continue
            doc_rows[m.group(1)] = cells[4]

    for name in sorted(set(rules) - set(doc_rows)):
        problems.append(f"{name}: alert rule has no row in {CATALOG}")
    for name in sorted(set(doc_rows) - set(rules)):
        problems.append(f"{name}: catalogue row names an alert that no rule tree defines")

    for name in sorted(set(rules) & set(doc_rows)):
        want, got = rules[name], doc_rows[name].replace("*", "").strip()
        if want not in VALID:
            problems.append(f"{name}: rule severity {want!r} is not one of {VALID} "
                            "(alertmanager.r1.yml routes on exactly these)")
        elif got != want:
            problems.append(
                f"{name}: catalogue Severity is {got!r} but the rule's "
                f"labels.severity is {want!r}"
                + (" — 'informational' routes to receiver `silent`, which has NO delivery"
                   if want == "informational" else ""))

    print(f"lint-alerts-catalog: checked {len(rules)} alert rule(s) against "
          f"{len(doc_rows)} catalogue row(s) — {len(problems)} problem(s) found")
    for p in problems:
        print(f"  {p}")
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
