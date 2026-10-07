#!/usr/bin/env python3
"""YAML-aware guard: every alert rule's runbook_url must live in
`annotations`, not `labels`.

WHY THIS EXISTS:
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
    file resolves to a file that exists (subsumes the old §9 grep);
  * a `Runbook:` link in the description names the same local runbook
    as `runbook_url`;
  * a `#fragment` on that URL slugs to a heading on the target page
    (outside code fences), so a renamed heading cannot silently drop the
    responder at the top of the page.

It FAILS if a runbook_url regresses back into `labels`. Pure-Python
(PyYAML); mirrors lint-rule-structure.py so it runs anywhere verify.sh
does. Invoked from lint-docs.sh §9 and alongside lint-rule-structure.py.
"""
import glob
import os
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from lint_doc_links import anchors_of, slug  # noqa: E402  (GitHub slug rules, shared)

try:
    import yaml
except ImportError:
    # Fail-closed, not skip, like the sibling lint-rule-structure.py: a
    # silent exit-0 here would let every runbook_url regress into labels:
    # whenever PyYAML is absent — the vacuous pass this lint exists to
    # prevent. This script runs standalone from lint-docs.sh in the
    # doc-checks CI job, which does NOT run its sibling.
    print(
        "lint-runbook-annotations: FAIL — PyYAML not available; cannot lint "
        "rule files (install pyyaml so this check runs; refusing to pass "
        "vacuously)",
        file=sys.stderr,
    )
    sys.exit(2)

DIRS = ["deploy/monitoring/rules", "configs/prometheus/rules.r1"]
RUNBOOKS_MARKER = "docs/operations/runbooks/"
DESC_RUNBOOK_RE = re.compile(r"Runbook:\s*(https://\S+?\.md)")
bad = 0


def err(path, alert, msg):
    global bad
    bad += 1
    print(f"  {path}: alert '{alert}': {msg}")


def resolve_local_runbook(value):
    """Return a repo-relative path if `value` targets a local runbook
    file via an absolute https:// URL, else None (external/opaque URL —
    presence-only). A bare repo-relative path is NOT resolved here — it
    fails its own check below instead of being treated as valid."""
    if value.startswith("https://") and RUNBOOKS_MARKER in value:
        return RUNBOOKS_MARKER + value.split(RUNBOOKS_MARKER, 1)[1].split("#", 1)[0]
    return None


# Zero rule files is a BROKEN GATE, not a clean tree: a moved or renamed
# rule directory would otherwise report "every alert has a non-empty
# annotations.runbook_url" over an empty set. Same fail-closed posture the
# sibling lint-rule-structure.py already takes.
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

                value = value.strip()

                # A bare repo-relative path (no https:// scheme) is
                # unclickable in the Discord templates, which only ever
                # render `.Annotations.runbook_url` as raw text — this
                # must fail even when the target file exists.
                if value.startswith(RUNBOOKS_MARKER):
                    err(path, name, f"`annotations.runbook_url` is a bare "
                        f"repo-relative path ({value!r}) — Discord renders "
                        "it unclickable; use the absolute https:// form")
                    continue

                # File existence for local-runbook targets (ex-§9).
                local = resolve_local_runbook(value)
                if local is not None and not os.path.isfile(local):
                    err(path, name, f"runbook_url points to missing file: {local}")
                elif local is not None and "#" in value:
                    frag = value.split("#", 1)[1]
                    if slug(frag) not in (anchors_of(local) or set()):
                        err(path, name, f"runbook_url anchor '#{frag}' matches "
                            f"no heading in {local}")

                # Discord renders both links; a description that names a
                # different runbook sends the responder to the wrong page.
                want = resolve_local_runbook(value.split("#", 1)[0])
                desc = ann.get("description") or ""
                for link in DESC_RUNBOOK_RE.findall(desc if isinstance(desc, str) else ""):
                    got = resolve_local_runbook(link.split("#", 1)[0])
                    if got is not None and got != want:
                        err(path, name, f"description links runbook {got} but "
                            f"runbook_url points at {want or value}")

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
