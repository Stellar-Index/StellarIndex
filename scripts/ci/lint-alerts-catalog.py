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

THE INFORMATIONAL-DELIVERY REGISTER (issue #485, 2026-09-02):
#362 made the Severity column honest, which surfaced the operational
fact underneath it — `severity: informational` routes to `receiver:
silent`, a receiver declared with NO `*_configs` block, so those alerts
are accepted by Alertmanager and delivered to nobody. Twenty-one rules
sit in that bucket today. Whether each of them SHOULD be silent is a
policy question (#485) this lint deliberately does not answer; what it
does enforce is that landing a rule there is a deliberate, written-down
choice rather than a copied YAML block nobody re-read. So, additionally:
  * every `informational` rule has a row in the catalogue's
    "Informational alerts — delivery register" section (between the
    `informational-register:begin/end` HTML comments);
  * every register row still names a rule that is `informational`
    (stale rows fail, so the register cannot rot into fiction);
  * each row's Triage cell opens with an explicit `silent-correct` or
    `needs-delivery` token and carries a real reason after it;
  * the two COUNTS the doc states out loud — the Severity legend's
    per-severity "Rules" column and the register's headline split — match
    the parsed rules. The legend said `page | 48` the day after a 49th
    page rule landed: a stated number nothing checked is the same failure
    shape as the P1/P2/P3 column #362 removed.
A missing register section, or an empty one while informational rules
exist, is a FAILURE — not a pass over an empty set.

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

# The delivery register (#485). Delimited by HTML comments rather than by a
# heading so the row parser cannot be knocked off by an editorial re-title,
# and so register rows are excluded from the main-table parse (they share the
# `| \`stellarindex_x\` |` shape but have their own columns).
REGISTER_BEGIN = "<!-- informational-register:begin -->"
REGISTER_END = "<!-- informational-register:end -->"
REGISTER_SECTION = "Informational alerts — delivery register"
TRIAGE = re.compile(r"^`(silent-correct|needs-delivery)`(.*)$", re.DOTALL)
# The Severity legend's "Rules" column, and the register's headline split.
# Both are counts stated in prose, i.e. exactly the kind of claim that rots:
# the legend said `page | 48` within a day of a 49th page rule landing. Check
# them against the parsed rules rather than trusting the sentence.
LEGEND_COUNT = re.compile(r"^\s*\|\s*`(page|ticket|informational)`\s*\|\s*(\d+)\s*\|")
REGISTER_SPLIT = re.compile(
    r"Counts as of \d{4}-\d{2}-\d{2}: (\d+) rules? — "
    r"(\d+) `silent-correct`, (\d+) `needs-delivery`")
# A token with no argument behind it is the same non-decision the register
# exists to prevent, so require a sentence — long enough that "TBD", "n/a"
# and "see runbook" cannot satisfy it.
MIN_REASON = 40


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
    register_rows = {}
    legend_counts = {}
    saw_begin = saw_end = False
    in_register = False
    with open(CATALOG, encoding="utf-8") as fh:
        catalog_text = fh.read()
    # The headline split is a wrapped prose sentence, so match it against the
    # whole doc with newlines folded to spaces rather than line by line.
    claim = REGISTER_SPLIT.search(" ".join(catalog_text.split()))
    split_claim = tuple(int(x) for x in claim.groups()) if claim else None

    for line in catalog_text.splitlines(keepends=True):
        legend = LEGEND_COUNT.match(line)
        if legend:
            legend_counts[legend.group(1)] = int(legend.group(2))
        if line.startswith(REGISTER_BEGIN):
            saw_begin, in_register = True, True
            continue
        if line.startswith(REGISTER_END):
            saw_end, in_register = True, False
            continue
        m = ROW.match(line)
        if not m:
            continue
        cells = split_cells(line)
        if in_register:
            # ['', Alert, Component, Meaning, Triage, '']
            if len(cells) != 6:
                problems.append(
                    f"{m.group(1)}: delivery-register row has {len(cells) - 2} columns, "
                    "expected 4 (Alert | Component | What a firing tells an operator | "
                    "Triage)")
                continue
            register_rows[m.group(1)] = (cells[3], cells[4])
            continue
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

    # ── The Severity legend's own arithmetic ─────────────────────────────
    # The legend states a rule COUNT per severity. It said `page | 48` while
    # 49 page rules existed, one day after the 49th landed — a stated number
    # nothing checked, which is the same failure shape as the old P1/P2/P3
    # column. Check it.
    for severity in VALID:
        actual = sum(1 for sev in rules.values() if sev == severity)
        stated = legend_counts.get(severity)
        if stated is None:
            problems.append(
                f"{CATALOG}: the Severity legend has no `{severity}` row — the legend is "
                "how a responder learns where that severity is DELIVERED; restore it")
        elif stated != actual:
            problems.append(
                f"{CATALOG}: the Severity legend says {stated} `{severity}` rule(s); the "
                f"rule trees define {actual}")

    # ── The informational-delivery register (#485) ───────────────────────
    informational = {n for n, sev in rules.items() if sev == "informational"}

    if not (saw_begin and saw_end):
        problems.append(
            f'{CATALOG}: the "{REGISTER_SECTION}" section is missing its '
            f"{REGISTER_BEGIN} / {REGISTER_END} markers — without them every "
            "informational rule would pass unregistered, which is the exact "
            "vacuous pass this check exists to prevent (#485)")
    elif informational and not register_rows:
        problems.append(
            f"{CATALOG}: the delivery register is EMPTY while {len(informational)} rule(s) "
            "carry severity `informational` — a register that matches nothing is not a "
            "clean tree, it is a broken gate (#485)")

    for name in sorted(informational - set(register_rows)):
        problems.append(
            f"{name}: severity is `informational`, which routes to `receiver: silent` — a "
            f'receiver with NO delivery. Add a row to "{REGISTER_SECTION}" in {CATALOG} '
            "recording `silent-correct` or `needs-delivery` and why (#485). If it should "
            "reach a human, the answer is a different severity, not a register row.")
    for name in sorted(set(register_rows) - informational):
        actual = rules.get(name, "<no such rule>")
        problems.append(
            f"{name}: delivery-register row names a rule whose severity is {actual!r}, not "
            "`informational` — delete the stale row (the register describes only what is "
            "routed to `silent`)")

    for name in sorted(informational & set(register_rows)):
        meaning, triage = register_rows[name]
        if len(meaning.strip()) < MIN_REASON:
            problems.append(
                f"{name}: delivery-register 'what a firing tells an operator' cell is empty "
                "or too short to be useful — say what the operator would learn")
        m = TRIAGE.match(triage.strip())
        if not m:
            problems.append(
                f"{name}: delivery-register Triage cell must OPEN with `silent-correct` or "
                f"`needs-delivery` (in backticks); got {triage.strip()[:60]!r}")
            continue
        reason = m.group(2).lstrip(" —-–:").strip()
        if len(reason) < MIN_REASON:
            problems.append(
                f"{name}: delivery-register Triage token `{m.group(1)}` carries no reason "
                f"(need >= {MIN_REASON} chars saying WHY; got {len(reason)})")

    # The register's headline split is the summary a decision-maker reads
    # instead of counting 21 rows, so it is checked like any other claim.
    tokens = [TRIAGE.match(t.strip()) for _, t in register_rows.values()]
    tally = {
        "silent-correct": sum(1 for t in tokens if t and t.group(1) == "silent-correct"),
        "needs-delivery": sum(1 for t in tokens if t and t.group(1) == "needs-delivery"),
    }
    want_split = (len(informational), tally["silent-correct"], tally["needs-delivery"])
    if split_claim is None:
        problems.append(
            f"{CATALOG}: the delivery register is missing its headline count. Restore the "
            "sentence, exactly: 'Counts as of YYYY-MM-DD: N rules — X `silent-correct`, "
            "Y `needs-delivery`.' (it is machine-checked, so it cannot drift)")
    elif split_claim != want_split:
        problems.append(
            f"{CATALOG}: the delivery register claims {split_claim[0]} rules / "
            f"{split_claim[1]} silent-correct / {split_claim[2]} needs-delivery, but the "
            f"rules and rows give {want_split[0]} / {want_split[1]} / {want_split[2]}")

    print(f"lint-alerts-catalog: checked {len(rules)} alert rule(s) against "
          f"{len(doc_rows)} catalogue row(s), and {len(informational)} informational "
          f"rule(s) against {len(register_rows)} delivery-register row(s) — "
          f"{len(problems)} problem(s) found")
    for p in problems:
        print(f"  {p}")
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
