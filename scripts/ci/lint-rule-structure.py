#!/usr/bin/env python3
"""Promtool-free structural lint for Prometheus rule files.

Catches the class of error that reds CI but slips past a local verify.sh
when promtool isn't installed (the 2026-07-06 galexie-archive.yml incident:
alerts indented at group level instead of inside `rules:` → promtool
"field expr not found in type rulefmt.RuleGroup"). Pure-Python (PyYAML),
so it runs everywhere verify.sh does.

Checks each groups[].rules[] entry has: exactly one of alert|record, an
`expr`, and no stray rule-shaped keys at the group level.

Also enforces the alert-routing contract (OBS-1): every ALERT rule must
carry the labels Alertmanager routes on — severity, team, component — and
the annotations every Discord/page template renders — summary, description.
A missing label silently mis-routes a page (drops to the catch-all route or
loses team ownership); a missing annotation renders a blank page body. These
are non-empty presence checks; record rules are exempt (they carry neither).
"""
import glob, os, re, sys
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
# OBS-1: labels Alertmanager routes on + annotations every page template renders.
REQUIRED_ALERT_LABELS = ("severity", "team", "component")
REQUIRED_ALERT_ANNOTATIONS = ("summary", "description")
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
                # OBS-1: alert rules must carry the routing labels + page
                # annotations. Record rules carry neither, so only gate alerts.
                if "alert" in r:
                    name = r.get("alert")
                    labels = r.get("labels") if isinstance(r.get("labels"), dict) else {}
                    ann = r.get("annotations") if isinstance(r.get("annotations"), dict) else {}
                    for key in REQUIRED_ALERT_LABELS:
                        val = labels.get(key)
                        if val is None or (isinstance(val, str) and not val.strip()):
                            err(path, f"alert '{name}' missing/empty required label `{key}` "
                                      f"(Alertmanager routes on {sorted(REQUIRED_ALERT_LABELS)})")
                    for key in REQUIRED_ALERT_ANNOTATIONS:
                        val = ann.get(key)
                        if val is None or (isinstance(val, str) and not val.strip()):
                            err(path, f"alert '{name}' missing/empty required annotation `{key}` "
                                      f"(page templates render {sorted(REQUIRED_ALERT_ANNOTATIONS)})")

# ─────────────────────────────────────────────────────────────────────
# Rule-TEST label realism (wave-D ALERT-02 / ALERT-11).
#
# promtool test rules asserts a rule's behaviour against series the
# FIXTURE invents. Nothing checks those series are shapes production can
# actually emit — so a test can assert against an impossible label set,
# go green, and certify an alert that cannot fire.
#
# That is not hypothetical. stellarindex_oracle_stale compared
# {source, asset} against {source} with no on(), so it matched nothing
# and could never fire for any oracle. Its promtool case passed anyway,
# because the fixture wrote
# stellarindex_oracle_resolution_seconds{...,asset="XLM"} — and that
# metric is declared []string{"source"}, so WithLabelValues with two
# arguments would panic. The test asserted against a series the emitter
# is structurally incapable of producing.
#
# A coverage-percentage gate was considered and rejected: it would have
# caught neither that bug nor the empty-vector ones, and it creates
# pressure to shrink a baseline by writing more fixtures — the exact
# mechanism that produced the false green. Checking fixtures against the
# emitter's DECLARED labels attacks the defect directly.
# `(?!Name:\s*")` is load-bearing: without it the non-greedy gap scans PAST
# a label-less declaration (prometheus.NewGauge / NewCounter, which take no
# []string) until it finds the NEXT metric's label slice, and attributes
# that slice to the wrong metric. 19 metrics in internal/obs/metrics.go
# were affected, so the fixture-realism check below ran against fabricated
# "declared" sets and its error message named labels the emitter does not
# have. Worked example: stellarindex_anomaly_freeze_active is a bare
# NewGauge with ZERO labels, yet the old pattern credited it with {op} —
# so a fixture writing anomaly_freeze_active{op="…"}, a series production
# can never emit, passed the realism check. Refusing to cross another
# `Name:` keeps every match inside one declaration.
DECL_RE = re.compile(
    r'Name:\s*"(stellarindex_[a-z0-9_]+)"((?:(?!Name:\s*").)*?)\[\]string\{([^}]*)\}',
    re.S,
)
# Labels attached by the SCRAPE, not by the emitter. A fixture must be
# allowed to set these even though no Go declaration lists them.
#
# `job`/`instance` are Prometheus built-ins. The rest are read from the
# scrape config's static_configs `labels:` blocks rather than hardcoded —
# r1 stamps `binary: stellarindex-{indexer,api,aggregator}` on every
# target, so a fixture carrying `binary=` is realistic, and a lint that
# called it bogus would be wrong about 19 existing cases. Deriving them
# means a new target label added tomorrow does not turn this lint into a
# source of false failures.
def scrape_attached_labels():
    out = {"job", "instance"}
    for path in ("configs/prometheus/prometheus.r1.yml",):
        if not os.path.exists(path):
            continue
        try:
            with open(path, encoding="utf-8") as fh:
                cfg = yaml.safe_load(fh) or {}
        except Exception:
            continue
        for sc in cfg.get("scrape_configs") or []:
            for static in sc.get("static_configs") or []:
                out.update((static.get("labels") or {}).keys())
    return out


SCRAPE_LABELS = scrape_attached_labels()
SERIES_RE = re.compile(r'^\s*-\s*series:\s*[\'"]?([a-zA-Z_:][a-zA-Z0-9_:]*)\{([^}]*)\}')


# A metric can have MORE THAN ONE emitter with different label sets, and
# the scraped one is not always the Go CounterVec. verify-archive is the
# worked example: the in-process vec declares {chunk_idx, reason}, but
# what production actually scrapes is the node_exporter TEXTFILE the
# binary renders, which carries {tier, reason}. Checking fixtures against
# the Go declaration alone flags correct tests as bogus — the first draft
# of this lint did exactly that on five stellar_test.yml lines.
#
# So the declared set is the UNION over every emitter shape: the Go
# declaration, plus any `metric{label=...}` literal written by a textfile
# renderer (Go fmt strings, shell probes, inline ansible content blocks,
# checked-in .prom files).
EMITTED_LITERAL_DIRS = (
    "internal", "cmd", "scripts", "configs/healthchecks",
    "configs/ansible/roles/archival-node/files",
    "configs/ansible/roles/archival-node/tasks",
)
LABEL_KEY_RE = re.compile(r'([a-zA-Z_][a-zA-Z0-9_]*)\s*=')


def declared_label_sets():
    """metric name -> set(label names any emitter can produce)."""
    out = {}
    for path in glob.glob("internal/obs/*.go"):
        with open(path, encoding="utf-8") as fh:
            body = fh.read()
        for name, _between, labels in DECL_RE.findall(body):
            names = {l.strip().strip('"') for l in labels.split(",") if l.strip()}
            # Union, never overwrite: narrowing invents violations.
            out.setdefault(name, set()).update(names)
    if not out:
        return out

    # Second pass: union in labels from rendered `metric{...}` literals.
    lit_res = {m: re.compile(re.escape(m) + r'\{([^}\n]*)\}') for m in out}
    for root in EMITTED_LITERAL_DIRS:
        for dirpath, _dirs, files in os.walk(root):
            for fn in files:
                if not fn.endswith((".go", ".sh", ".prom", ".yml", ".yaml")):
                    continue
                fp = os.path.join(dirpath, fn)
                try:
                    with open(fp, encoding="utf-8", errors="ignore") as fh:
                        body = fh.read()
                except OSError:
                    continue
                for metric, rx in lit_res.items():
                    if metric not in body:
                        continue
                    for blob in rx.findall(body):
                        out[metric].update(LABEL_KEY_RE.findall(blob))
    return out


declared = declared_label_sets()
if declared:
    for path in sorted(glob.glob("deploy/monitoring/rule-tests/*.yml")):
        with open(path, encoding="utf-8") as fh:
            for lineno, line in enumerate(fh, 1):
                m = SERIES_RE.match(line)
                if not m:
                    continue
                metric, labelblob = m.group(1), m.group(2)
                if metric not in declared:
                    continue  # not an obs-declared metric (node_*, textfile, …)
                used = {
                    kv.split("=", 1)[0].strip()
                    for kv in labelblob.split(",")
                    if "=" in kv
                }
                bogus = sorted(used - declared[metric] - SCRAPE_LABELS)
                if bogus:
                    err(f"{path}:{lineno}",
                        f"fixture series for `{metric}` sets label(s) {bogus} that the "
                        f"emitter does not declare (declared: {sorted(declared[metric]) or 'none'}). "
                        f"Production cannot produce this series — WithLabelValues would panic — "
                        f"so any assertion built on it certifies behaviour that cannot occur.")

if bad:
    print(f"lint-rule-structure: {bad} problem(s) found", file=sys.stderr)
    sys.exit(1)
print("lint-rule-structure: all Prometheus rule files structurally OK")
