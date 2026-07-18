#!/usr/bin/env python3
"""Promtool-free structural lint for Prometheus rule files.

Catches the class of error that reds CI but slips past a local verify.sh
when promtool isn't installed (the 2026-07-06 galexie-archive.yml incident:
alerts indented at group level instead of inside `rules:` → promtool
"field expr not found in type rulefmt.RuleGroup"). Pure-Python (PyYAML),
so it runs everywhere verify.sh does.

Checks each groups[].rules[] entry has: exactly one of alert|record, an
`expr`, and no stray rule-shaped keys at the group level.
"""
import glob, os, sys
try:
    import yaml
except ImportError:
    # Fail-closed, not skip: a silent exit-0 here (the old behaviour) let a
    # mis-indented rule reach main whenever PyYAML happened to be absent — the
    # exact vacuous-pass this lint exists to prevent. Mirror lint-metric-refs.sh's
    # `exit 2` on a missing prerequisite so CI installs/repairs it instead.
    print("lint-rule-structure: FAIL — PyYAML not available; cannot lint rule files "
          "(install pyyaml so this check runs; refusing to pass vacuously)", file=sys.stderr)
    sys.exit(2)

DIRS = ["deploy/monitoring/rules", "configs/prometheus/rules.r1"]
GROUP_LEVEL_RULE_KEYS = {"alert", "record", "expr", "for", "labels", "annotations"}
bad = 0

def err(path, msg):
    global bad
    bad += 1
    print(f"  {path}: {msg}")

for d in DIRS:
    if not os.path.isdir(d):
        err(d, "rule directory does not exist — a moved/renamed dir would make this "
               "lint a vacuous pass (empty glob -> exit 0); restore it or fix DIRS")
        continue
    yml_files = sorted(glob.glob(f"{d}/*.yml"))
    if not yml_files:
        err(d, "rule directory matched zero *.yml files — nothing to lint; a wrong "
               "path or moved files would otherwise pass silently")
        continue
    for path in yml_files:
        try:
            doc = yaml.safe_load(open(path))
        except yaml.YAMLError as e:
            err(path, f"YAML parse error: {e}"); continue
        if not isinstance(doc, dict) or "groups" not in doc:
            err(path, "no top-level `groups:` key"); continue
        for gi, g in enumerate(doc["groups"] or []):
            if not isinstance(g, dict):
                err(path, f"groups[{gi}] is not a mapping"); continue
            if "name" not in g:
                err(path, f"groups[{gi}] missing `name`")
            # a rule-shaped key at group level = a mis-indented rule (the incident)
            stray = GROUP_LEVEL_RULE_KEYS & set(g)
            if stray:
                err(path, f"group '{g.get('name','?')}' has rule-level key(s) {sorted(stray)} at GROUP level — a rule is mis-indented (should be under `rules:`)")
            for ri, r in enumerate(g.get("rules") or []):
                if not isinstance(r, dict):
                    err(path, f"group '{g.get('name','?')}' rules[{ri}] is not a mapping"); continue
                has = [k for k in ("alert", "record") if k in r]
                if len(has) != 1:
                    err(path, f"group '{g.get('name','?')}' rules[{ri}] must have exactly one of alert|record (has {has})")
                if "expr" not in r:
                    err(path, f"group '{g.get('name','?')}' rule '{r.get('alert') or r.get('record') or ri}' missing `expr`")

if bad:
    print(f"lint-rule-structure: {bad} problem(s) found", file=sys.stderr)
    sys.exit(1)
print("lint-rule-structure: all Prometheus rule files structurally OK")
