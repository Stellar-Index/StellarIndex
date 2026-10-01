#!/usr/bin/env bash
# Pins the page→ticket inhibit rule to alert families. It does not call
# amtool — it evaluates the rule's own matchers the way Alertmanager does
# (source_matchers AND target_matchers AND label equality on `equal`, a
# missing label comparing equal to "") against every page/ticket pair in
# both rule trees, under both apply paths (alertmanager.r1.yml and the
# ansible alertmanager.yml.j2).
#
#   T1  a component=meta page never inhibits stellarindex_deadmansswitch
#   T2  each tree's {alertname: alert_family} map equals EXPECTED, the trees
#       agree, and the deadmansswitch has no family
#   T3  every family has a page and a lower-severity member on one component
#   T4  page P inhibits ticket/informational T iff P has a family, the
#       families and components match, and T is not the deadmansswitch
#   T5  a templated per-entity family never crosses entities
#
# Exit: 0 = every check holds; 1 = at least one fails.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
REPO="$SCRIPT_DIR/../.."
R1="$SCRIPT_DIR/alertmanager.r1.yml"
J2="$REPO/configs/ansible/roles/prometheus/templates/alertmanager.yml.j2"

PY=""
for cand in python3 /opt/homebrew/bin/python3; do
  if command -v "$cand" >/dev/null 2>&1 && "$cand" -c 'import jinja2, yaml' >/dev/null 2>&1; then
    PY="$cand"
    break
  fi
done
if [ -z "$PY" ]; then
  if [ "${CI:-}" = "true" ]; then
    echo "inhibit-rules-test: FAIL — no python3 with jinja2 + PyYAML (required in CI)" >&2
    exit 1
  fi
  echo "inhibit-rules-test: SKIP (no python3 with jinja2 + PyYAML locally; CI enforces)"
  exit 0
fi

AM_R1="$R1" AM_J2="$J2" \
  RULES_DEPLOY="$REPO/deploy/monitoring/rules" RULES_R1="$REPO/configs/prometheus/rules.r1" \
  "$PY" - <<'PY'
import glob
import os
import re
import sys

import jinja2
import yaml

DMS = "stellarindex_deadmansswitch"

# A family is one signal at a page and a milder threshold. Cause/effect
# pairs stay out on purpose so the diagnostic ticket still notifies.
EXPECTED = {
    "stellarindex_api_error_rate_critical": "api_error_rate",
    "stellarindex_api_error_rate_high": "api_error_rate",
    "stellarindex_slo_latency_burn_fast": "slo_latency_burn",
    "stellarindex_slo_latency_burn_medium": "slo_latency_burn",
    "stellarindex_slo_latency_burn_slow": "slo_latency_burn",
    "stellarindex_slo_availability_burn_fast": "slo_availability_burn",
    "stellarindex_slo_availability_burn_medium": "slo_availability_burn",
    "stellarindex_slo_availability_burn_slow": "slo_availability_burn",
    "stellarindex_archive_completeness_critical_stale": "archive_completeness_stale",
    "stellarindex_archive_completeness_stale": "archive_completeness_stale",
    "stellarindex_galexie_archive_tip_lag_severe": "galexie_archive_tip_lag",
    "stellarindex_galexie_archive_tip_lag_high": "galexie_archive_tip_lag",
    "stellarindex_zfs_pool_critical_space": "zfs_pool_capacity",
    "stellarindex_zfs_pool_low_space": "zfs_pool_capacity",
    "stellarindex_ingestion_all_sources_stopped": "ingestion_source_stopped",
    "stellarindex_ingestion_source_stopped": "ingestion_source_stopped",
    "stellarindex_ingestion_source_stopped_low_volume_dex": "ingestion_source_stopped",
    "stellarindex_ingestion_source_stopped_daily_publisher": "ingestion_source_stopped",
    "stellarindex_ingestion_ch_live_sink_drops_sustained": "ingestion_ch_live_sink_drops",
    "stellarindex_ingestion_ch_live_sink_drops": "ingestion_ch_live_sink_drops",
    "stellarindex_process_mappings_critical": "process_mappings/{{ $labels.process }}",
    "stellarindex_process_mappings_high": "process_mappings/{{ $labels.process }}",
    "stellarindex_stellar_stack_protocol_lag": "stellar_stack_version_lag/{{ $labels.stack_component }}",
    "stellarindex_stellar_stack_lagging": "stellar_stack_version_lag/{{ $labels.stack_component }}",
    "stellarindex_timescale_disk_full": "timescale_disk",
    "stellarindex_timescale_disk_warning": "timescale_disk",
    "stellarindex_node_root_disk_full": "node_root_disk",
    "stellarindex_node_root_disk_warning": "node_root_disk",
    "stellarindex_timescale_backup_none_24h": "timescale_backup_age/{{ $labels.stanza }}",
    "stellarindex_timescale_backup_failed": "timescale_backup_age/{{ $labels.stanza }}",
    "stellarindex_supply_snapshot_critical_stale": "supply_snapshot_stale",
    "stellarindex_supply_snapshot_stale": "supply_snapshot_stale",
    "stellarindex_zfs_pool_free_critical": "zfs_pool_free/{{ $labels.pool }}",
    "stellarindex_zfs_pool_free_low": "zfs_pool_free/{{ $labels.pool }}",
}

TEMPLATE = re.compile(r"\{\{\s*\$labels\.(\w+)\s*\}\}")
LOWER = ("ticket", "informational")


def matches(matchers, labels):
    for m in matchers:
        m = m.strip()
        for op in ("=~", "!=", "="):
            if op in m:
                name, val = m.split(op, 1)
                name = name.strip()
                val = val.strip().strip('"')
                got = labels.get(name, "")
                if op == "=" and got != val:
                    return False
                if op == "!=" and got == val:
                    return False
                if op == "=~" and not re.fullmatch(val, got):
                    return False
                break
        else:
            return False
    return True


def would_inhibit(inhibit_rules, source, target):
    for rule in inhibit_rules:
        if not matches(rule["source_matchers"], source):
            continue
        if not matches(rule["target_matchers"], target):
            continue
        # Alertmanager equates a missing label with an empty one.
        if any(source.get(k, "") != target.get(k, "") for k in rule.get("equal", [])):
            continue
        return True
    return False


def load_alerts(tree):
    alerts = []
    for path in sorted(glob.glob(os.path.join(tree, "*.yml"))):
        doc = yaml.safe_load(open(path)) or {}
        for group in doc.get("groups") or []:
            for rule in group.get("rules") or []:
                if "alert" in rule:
                    labels = {k: str(v) for k, v in (rule.get("labels") or {}).items()}
                    labels["alertname"] = rule["alert"]
                    alerts.append(labels)
    return alerts


def render(labels, entity):
    return {k: TEMPLATE.sub(lambda _: entity, v) for k, v in labels.items()}


def expect_inhibit(p, t):
    fam = p.get("alert_family", "")
    return (
        fam != ""
        and fam == t.get("alert_family", "")
        and p.get("component", "") == t.get("component", "")
        and t["alertname"] != DMS
    )


def report(problems, title, items):
    if items:
        problems.append(f"{title}: {len(items)} offending")
        problems.extend("    " + i for i in items[:10])


problems = []

r1_rules = yaml.safe_load(open(os.environ["AM_R1"]))["inhibit_rules"]
env = jinja2.Environment(undefined=jinja2.StrictUndefined, keep_trailing_newline=True)
rendered = env.from_string(open(os.environ["AM_J2"]).read()).render(
    alertmanager_healthchecks_deadmansswitch_url="https://hc-ping.com/dead",
    alertmanager_healthchecks_alert_delivery_url="https://hc-ping.com/deliv",
    alertmanager_discord_webhook_url_pages="https://discord.com/api/webhooks/1/PAGES",
    alertmanager_discord_webhook_url_alerts="https://discord.com/api/webhooks/2/ALERTS",
    alertmanager_discord_webhook_url_informational="https://discord.com/api/webhooks/3/INFO",
    alertmanager_pagerduty_key="",
)
configs = {"alertmanager.r1.yml": r1_rules, "alertmanager.yml.j2": yaml.safe_load(rendered)["inhibit_rules"]}

# T1
meta_page = {"alertname": "stellarindex_alertmanager_down", "severity": "page", "component": "meta"}
dms = {"alertname": DMS, "severity": "informational", "component": "meta"}
for name, rules in configs.items():
    if would_inhibit(rules, meta_page, dms):
        problems.append(f"T1 {name}: a component=meta page inhibits {DMS}")

trees = {"deploy/monitoring/rules": os.environ["RULES_DEPLOY"], "configs/prometheus/rules.r1": os.environ["RULES_R1"]}
family_maps = {}
for tname, tpath in trees.items():
    alerts = load_alerts(tpath)
    names = {a["alertname"] for a in alerts}

    # T2
    fmap = {a["alertname"]: a["alert_family"] for a in alerts if "alert_family" in a}
    family_maps[tname] = fmap
    diff = sorted(
        f"{n}: have {fmap.get(n)!r}, want {EXPECTED.get(n)!r}"
        for n in set(fmap) | set(EXPECTED)
        if fmap.get(n) != EXPECTED.get(n)
    )
    report(problems, f"T2 {tname}: alert_family map differs from EXPECTED", diff)
    if fmap.get(DMS):
        problems.append(f"T2 {tname}: {DMS} carries an alert_family")

    # T3
    by_family = {}
    for a in alerts:
        if a.get("alert_family"):
            by_family.setdefault(a["alert_family"], []).append(a)
    bad = []
    for fam, members in sorted(by_family.items()):
        sev = {m.get("severity") for m in members}
        comps = {m.get("component") for m in members}
        if "page" not in sev or not sev & set(LOWER) or len(comps) != 1:
            bad.append(f"{fam}: severities {sorted(map(str, sev))}, components {sorted(map(str, comps))}")
    report(problems, f"T3 {tname}: family without a page, a lower member, or one component", bad)

    pages = [render(a, "e1") for a in alerts if a.get("severity") == "page"]
    targets = [render(a, "e1") for a in alerts if a.get("severity") in LOWER]
    for cname, rules in configs.items():
        # T4
        wrong = [
            f"{p['alertname']} -> {t['alertname']}: inhibits={got}, want {want}"
            for p in pages
            for t in targets
            if (got := would_inhibit(rules, p, t)) != (want := expect_inhibit(p, t))
        ]
        report(problems, f"T4 {tname} x {cname}: inhibition outside the page's family", wrong)
        for src, dst in (
            ("stellarindex_api_error_rate_critical", "stellarindex_customer_webhook_delivery_failing"),
            ("stellarindex_api_down", "stellarindex_api_latency_p95_high"),
        ):
            if src not in names or dst not in names:
                problems.append(f"T4 {tname}: named pair {src} -> {dst} no longer exists; update this test")
                continue
            p = next(a for a in pages if a["alertname"] == src)
            t = next(a for a in targets if a["alertname"] == dst)
            if would_inhibit(rules, p, t):
                problems.append(f"T4 {tname} x {cname}: {src} inhibits {dst}")

        # T5
        crossed = []
        for a in alerts:
            if a.get("severity") != "page" or not TEMPLATE.search(a.get("alert_family", "")):
                continue
            for b in alerts:
                if b.get("severity") not in LOWER or b.get("alert_family") != a["alert_family"]:
                    continue
                if would_inhibit(rules, render(a, "e1"), render(b, "e2")):
                    crossed.append(f"{a['alertname']}[e1] -> {b['alertname']}[e2]")
                if not would_inhibit(rules, render(a, "e1"), render(b, "e1")):
                    crossed.append(f"{a['alertname']}[e1] does not inhibit {b['alertname']}[e1]")
        report(problems, f"T5 {tname} x {cname}: per-entity family crosses entities", crossed)

if family_maps["deploy/monitoring/rules"] != family_maps["configs/prometheus/rules.r1"]:
    problems.append("T2: the two rule trees carry different alert_family maps")

if problems:
    print("inhibit-rules-test: FAIL")
    for p in problems:
        print(p if p.startswith(" ") else "  - " + p)
    sys.exit(1)

print(f"inhibit-rules-test: OK — {len(EXPECTED)} family alerts; pages inhibit only their own family; deadmansswitch never inhibited")
PY
