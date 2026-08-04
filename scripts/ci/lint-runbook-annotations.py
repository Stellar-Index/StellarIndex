#!/usr/bin/env python3
"""YAML-aware guard: every alert rule's runbook_url must live in
`annotations`, not `labels`.

WHY THIS EXISTS (audit 2026-07-16, C4-1/C4-15):
Alertmanager keeps `.Labels` and `.Annotations` strictly separate. Both
Discord fanout templates render the runbook line with
`{{ if .Annotations.runbook_url }}Runbook: {{ .Annotations.runbook_url }}{{ end }}`.
For years the rules put `runbook_url` under each alert's `labels:` block,
so `.Annotations.runbook_url` was ALWAYS empty and no page ever showed
its runbook link. The old `lint-docs.sh §9` check was a blind grep over
raw text — it matched a `runbook_url:` string anywhere in the file, so it
never noticed the key was in the wrong block (false confidence).

This check PARSES each rule and asserts, per alert:
  * `annotations.runbook_url` is present and non-empty;
  * `runbook_url` (or the legacy `runbook`) is NOT sitting in `labels`;
  * `annotations` doesn't use the inconsistent bare `runbook` key;
  * a runbook_url that points at a local docs/operations/runbooks/*.md
    file resolves to a file that exists (subsumes the old §9 grep).

It FAILS if a runbook_url regresses back into `labels`. Pure-Python
(PyYAML); mirrors lint-rule-structure.py so it runs anywhere verify.sh
does. Invoked from lint-docs.sh §9 and alongside lint-rule-structure.py.
"""
import glob
import os
import sys

try:
    import yaml
except ImportError:
    # Fail-closed, not skip. This mirrors the fix already made in the
    # sibling lint-rule-structure.py: a silent exit-0 here let every
    # runbook_url regress into labels: whenever PyYAML happened to be
    # absent — the exact vacuous pass this lint exists to prevent (C4-1:
    # 266 of 270 alerts, no page ever showed a runbook link). This script
    # is invoked standalone from lint-docs.sh in the doc-checks CI job,
    # which does NOT run its sibling, so nothing else covered the gap.
    # Cold audit 2026-08-04.
    print(
        "lint-runbook-annotations: FAIL — PyYAML not available; cannot lint "
        "rule files (install pyyaml so this check runs; refusing to pass "
        "vacuously)",
        file=sys.stderr,
    )
    sys.exit(2)

DIRS = ["deploy/monitoring/rules", "configs/prometheus/rules.r1"]
RUNBOOKS_MARKER = "docs/operations/runbooks/"
bad = 0


def err(path, alert, msg):
    global bad
    bad += 1
    print(f"  {path}: alert '{alert}': {msg}")


def resolve_local_runbook(value):
    """Return a repo-relative path if `value` targets a local runbook
    file, else None (external/opaque URL — presence-only)."""
    if value.startswith(RUNBOOKS_MARKER):
        return value
    if value.startswith("https://") and RUNBOOKS_MARKER in value:
        return RUNBOOKS_MARKER + value.split(RUNBOOKS_MARKER, 1)[1]
    return None


# Zero rule files is a BROKEN GATE, not a clean tree: a moved or renamed
# rule directory would otherwise report "every alert has a non-empty
# annotations.runbook_url" over an empty set. Same fail-closed posture the
# sibling lint-rule-structure.py already takes (cold audit 2026-08-04).
_seen = 0
for d in DIRS:
    if not os.path.isdir(d):
        print(
            f"lint-runbook-annotations: FAIL — rule directory {d} does not "
            "exist; a moved/renamed dir would make this lint a vacuous pass",
            file=sys.stderr,
        )
        sys.exit(1)
    for path in sorted(glob.glob(f"{d}/*.yml")):
        _seen += 1
        try:
            doc = yaml.safe_load(open(path))
        except yaml.YAMLError as e:
            print(f"  {path}: YAML parse error: {e}")
            bad += 1
            continue
        if not isinstance(doc, dict):
            continue
        for g in doc.get("groups") or []:
            for r in g.get("rules") or []:
                if not isinstance(r, dict) or "alert" not in r:
                    continue  # record rules carry no runbook
                name = r.get("alert")
                labels = r.get("labels") or {}
                ann = r.get("annotations") or {}

                # Regression guard: runbook must NOT be in labels.
                if "runbook_url" in labels:
                    err(path, name, "runbook_url is in `labels` — move it to "
                        "`annotations` (Alertmanager templates read "
                        ".Annotations.runbook_url, never .Labels)")
                if "runbook" in labels:
                    err(path, name, "legacy `runbook` key is in `labels` — "
                        "use `annotations.runbook_url`")
                # Inconsistent bare key in annotations (C4-15).
                if "runbook" in ann and "runbook_url" not in ann:
                    err(path, name, "uses inconsistent `annotations.runbook` "
                        "key — rename to `annotations.runbook_url`")

                # Presence + non-empty.
                value = ann.get("runbook_url")
                if value is None:
                    err(path, name, "missing `annotations.runbook_url`")
                    continue
                if not isinstance(value, str) or not value.strip():
                    err(path, name, "`annotations.runbook_url` is empty")
                    continue

                # File existence for local-runbook targets (ex-§9).
                local = resolve_local_runbook(value.strip())
                if local is not None and not os.path.isfile(local):
                    err(path, name, f"runbook_url points to missing file: {local}")

if _seen == 0:
    print(
        "lint-runbook-annotations: FAIL — 0 rule files found; refusing to "
        "pass vacuously",
        file=sys.stderr,
    )
    sys.exit(1)

if bad:
    print(f"lint-runbook-annotations: {bad} problem(s) found", file=sys.stderr)
    sys.exit(1)
print("lint-runbook-annotations: every alert has a non-empty "
      "annotations.runbook_url pointing at an existing runbook")
