#!/usr/bin/env bash
# GH-900 regression: the inhibit rule's `equal: [component]` is coarse
# enough that ANY page sharing a component with stellarindex_deadmansswitch
# (component=meta) silences the heartbeat whose entire purpose is to catch
# Prometheus/Alertmanager being down (deploy/monitoring/rules/meta.yml
# stellarindex_alertmanager_down / _not_notifying are both severity=page,
# component=meta). This does not call amtool — it evaluates the rule's own
# matchers the way Alertmanager does (source_matchers AND target_matchers
# AND label equality on `equal`) against a synthetic firing page and a
# synthetic deadmansswitch alert, so it exercises the actual TRIGGERING
# condition rather than an inert shape.
#
# Exit: 0 = deadmansswitch is provably never inhibited by both configs.
#       1 = at least one config would still inhibit it (the pre-fix defect).
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" &>/dev/null && pwd)"
R1="$SCRIPT_DIR/alertmanager.r1.yml"
J2="$SCRIPT_DIR/../ansible/roles/prometheus/templates/alertmanager.yml.j2"

PY=""
for cand in python3 /opt/homebrew/bin/python3; do
  if command -v "$cand" >/dev/null 2>&1 && "$cand" -c 'import jinja2, yaml' >/dev/null 2>&1; then
    PY="$cand"
    break
  fi
done
if [ -z "$PY" ]; then
  if [ "${CI:-}" = "true" ]; then
    echo "inhibit-deadmansswitch-test: FAIL — no python3 with jinja2 + PyYAML (required in CI)" >&2
    exit 1
  fi
  echo "inhibit-deadmansswitch-test: SKIP (no python3 with jinja2 + PyYAML locally; CI enforces)"
  exit 0
fi

AM_R1="$R1" AM_J2="$J2" "$PY" - <<'PY'
import os
import re
import sys

import jinja2
import yaml

PAGE = {"alertname": "stellarindex_alertmanager_down", "severity": "page", "component": "meta"}
DMS = {"alertname": "stellarindex_deadmansswitch", "severity": "informational", "component": "meta"}


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
        if any(source.get(k) != target.get(k) for k in rule.get("equal", [])):
            continue
        return True
    return False


problems = []

r1doc = yaml.safe_load(open(os.environ["AM_R1"]))
if would_inhibit(r1doc["inhibit_rules"], PAGE, DMS):
    problems.append("alertmanager.r1.yml: a component=meta page still inhibits stellarindex_deadmansswitch")

env = jinja2.Environment(undefined=jinja2.StrictUndefined, keep_trailing_newline=True)
tpl = env.from_string(open(os.environ["AM_J2"]).read())
rendered = tpl.render(
    alertmanager_healthchecks_deadmansswitch_url="https://hc-ping.com/dead",
    alertmanager_healthchecks_alert_delivery_url="https://hc-ping.com/deliv",
    alertmanager_discord_webhook_url_pages="https://discord.com/api/webhooks/1/PAGES",
    alertmanager_discord_webhook_url_alerts="https://discord.com/api/webhooks/2/ALERTS",
    alertmanager_discord_webhook_url_informational="https://discord.com/api/webhooks/3/INFO",
    alertmanager_pagerduty_key="",
)
j2doc = yaml.safe_load(rendered)
if would_inhibit(j2doc["inhibit_rules"], PAGE, DMS):
    problems.append("alertmanager.yml.j2: a component=meta page still inhibits stellarindex_deadmansswitch")

if problems:
    print("inhibit-deadmansswitch-test: FAIL")
    for p in problems:
        print("  - " + p)
    sys.exit(1)

print("inhibit-deadmansswitch-test: OK — stellarindex_deadmansswitch is never inhibited by a same-component page")
PY
