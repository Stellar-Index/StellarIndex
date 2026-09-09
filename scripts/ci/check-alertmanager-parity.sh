#!/usr/bin/env bash
# check-alertmanager-parity.sh — the two Alertmanager apply paths must
# render the same routing (#501).
#
# WHY THIS EXISTS. /etc/prometheus/alertmanager.yml has two producers:
#
#   configs/alertmanager/alertmanager.r1.yml   (+ apply.sh, the r1 path)
#   configs/ansible/roles/prometheus/templates/alertmanager.yml.j2
#                                              (the role, for new hosts)
#
# Both files SAID they mirrored each other — in a header comment, which
# is not a gate. On 2026-09-08, c0815d73e fixed #485 by routing
# `severity: informational` to a new `chat-informational` Discord
# receiver, and touched only the first file. The Ansible template kept
# routing informational to `silent`, a receiver with no *_configs block,
# which accepts alerts and delivers them to nobody. An apply of the
# prometheus role would have silently reinstated the exact bug that had
# just been fixed, and nothing in CI could have seen it because nothing
# in CI read both files. This does.
#
# WHAT PARITY MEANS. Rendered with identical inputs, the two must agree
# on `global`, `route` (the whole tree), `inhibit_rules` and `receivers`
# — including the Discord Go templates, whose payload bounds are what
# keep a large alert group under Discord's 4096-character embed limit
# (the 2026-09-07 outage: 11 h of HTTP 400, every ticket alert dropped).
# A bound present on one path and not the other is one apply away from
# repeating it.
#
# BOTH RENDER BRANCHES are compared, because they fail differently:
#
#   wired — every URL set. Catches a routing or template divergence.
#   dark  — every URL empty. Catches a divergence in the DEGRADED shape:
#           apply.sh strips the `*_configs:` block with a line-based
#           Python walker, the template drops it with `{% if %}`, and
#           the two mechanisms can disagree about what is left behind.
#           Getting this wrong is invisible until the day a URL is unset.
#
# `alertmanager_pagerduty_key` is rendered EMPTY. The template carries an
# optional PagerDuty leg on chat-page that the r1 file has no equivalent
# for; that is a deliberate multi-host-only extra, not drift, so parity
# is defined at the shape both files can express. If the r1 path ever
# grows a PagerDuty leg, set the key here and the gate covers it too.
#
# Rendering uses apply.sh's OWN `--render-only` mode rather than a second
# copy of the block-stripper: a parity gate that reimplements the thing
# it checks proves only that the two copies agree.
#
# Environment:
#   AM_PARITY_ROOT  repo root to check (default: this repo). The
#                   self-test points it at a fixture tree.
#
# Exit: 0 = the two paths agree, 1 = they diverge (or the gate could not
# run in CI). Skips with 0 locally when no python3 with jinja2 + PyYAML
# is available, matching lint-jinja-templates.sh; CI is fail-closed.
set -euo pipefail

ROOT="${AM_PARITY_ROOT:-$(cd "$(dirname "$0")/../.." && pwd)}"
cd "$ROOT" || exit 1

J2="configs/ansible/roles/prometheus/templates/alertmanager.yml.j2"
APPLY="configs/alertmanager/apply.sh"
R1="configs/alertmanager/alertmanager.r1.yml"

for f in "$J2" "$APPLY" "$R1"; do
  if [ ! -f "$f" ]; then
    echo "check-alertmanager-parity: FAIL — missing $f (under $ROOT)" >&2
    exit 1
  fi
done

# ── interpreter discovery ──────────────────────────────────────────
# Same three candidates as lint-jinja-templates.sh, plus PyYAML: ansible
# ships its own interpreter with both, which is what makes this runnable
# on a workstation that has ansible but no system jinja2.
PY=""
for cand in python3 /opt/homebrew/bin/python3; do
  if command -v "$cand" >/dev/null 2>&1 &&
    "$cand" -c 'import jinja2, yaml' >/dev/null 2>&1; then
    PY="$cand"
    break
  fi
done
if [ -z "$PY" ] && command -v ansible-playbook >/dev/null 2>&1; then
  cand=$(head -1 "$(command -v ansible-playbook)" | sed 's|^#!||' | awk '{print $1}')
  if [ -x "$cand" ] && "$cand" -c 'import jinja2, yaml' >/dev/null 2>&1; then PY="$cand"; fi
fi
if [ -z "$PY" ]; then
  if [ "${CI:-}" = "true" ]; then
    echo "check-alertmanager-parity: FAIL — no python3 with jinja2 + PyYAML (required in CI)" >&2
    exit 1
  fi
  echo "check-alertmanager-parity: SKIP (no python3 with jinja2 + PyYAML locally; CI enforces)"
  exit 0
fi

TMPD=$(mktemp -d)
trap 'rm -rf "$TMPD"' EXIT

# ── render the r1 path, both branches, through apply.sh itself ─────
# The dummy URLs are shaped like the real ones (hc-ping.com /
# discord.com/api/webhooks) so a renderer that ever grows a
# host-specific rule fails here rather than in production.
cat > "$TMPD/secrets.wired" <<'EOF'
HEALTHCHECKS_DEADMANSSWITCH_URL=https://hc-ping.com/00000000-0000-0000-0000-00000000dead
HEALTHCHECKS_ALERT_DELIVERY_URL=https://hc-ping.com/00000000-0000-0000-0000-0000000de117
DISCORD_WEBHOOK_URL_PAGES=https://discord.com/api/webhooks/1/PAGES
DISCORD_WEBHOOK_URL_ALERTS=https://discord.com/api/webhooks/2/ALERTS
DISCORD_WEBHOOK_URL_INFORMATIONAL=https://discord.com/api/webhooks/3/INFORMATIONAL
EOF
: > "$TMPD/secrets.dark"

for branch in wired dark; do
  if ! ALERTMANAGER_SECRETS="$TMPD/secrets.$branch" \
    bash "$APPLY" --render-only "$TMPD/r1.$branch.yml"; then
    echo "check-alertmanager-parity: FAIL — apply.sh could not render the $branch branch" >&2
    exit 1
  fi
done

# ── render the template branch-for-branch and compare ──────────────
AM_PARITY_J2="$J2" AM_PARITY_TMPD="$TMPD" "$PY" - <<'PY'
import json
import os
import sys

import jinja2
import yaml

TMPD = os.environ["AM_PARITY_TMPD"]
src = open(os.environ["AM_PARITY_J2"]).read()

# StrictUndefined: a variable the role does not default is a failure
# here, not a silently-empty block. That is how an unset webhook var
# would otherwise render as the black hole this gate exists to catch.
env = jinja2.Environment(undefined=jinja2.StrictUndefined, keep_trailing_newline=True)
tpl = env.from_string(src)

BRANCHES = {
    "wired": {
        "alertmanager_healthchecks_deadmansswitch_url":
            "https://hc-ping.com/00000000-0000-0000-0000-00000000dead",
        "alertmanager_healthchecks_alert_delivery_url":
            "https://hc-ping.com/00000000-0000-0000-0000-0000000de117",
        "alertmanager_discord_webhook_url_pages":
            "https://discord.com/api/webhooks/1/PAGES",
        "alertmanager_discord_webhook_url_alerts":
            "https://discord.com/api/webhooks/2/ALERTS",
        "alertmanager_discord_webhook_url_informational":
            "https://discord.com/api/webhooks/3/INFORMATIONAL",
        "alertmanager_pagerduty_key": "",
    },
    "dark": {
        "alertmanager_healthchecks_deadmansswitch_url": "",
        "alertmanager_healthchecks_alert_delivery_url": "",
        "alertmanager_discord_webhook_url_pages": "",
        "alertmanager_discord_webhook_url_alerts": "",
        "alertmanager_discord_webhook_url_informational": "",
        "alertmanager_pagerduty_key": "",
    },
}

SECTIONS = ("global", "route", "inhibit_rules")

problems = []
for branch, vars_ in BRANCHES.items():
    try:
        rendered = tpl.render(**vars_)
    except jinja2.UndefinedError as exc:
        problems.append(
            "[%s] the template references a variable the role does not "
            "default: %s" % (branch, exc)
        )
        continue

    try:
        j2doc = yaml.safe_load(rendered)
    except yaml.YAMLError as exc:
        problems.append("[%s] the rendered template is not valid YAML: %s" % (branch, exc))
        continue

    r1doc = yaml.safe_load(open(os.path.join(TMPD, "r1.%s.yml" % branch)).read())

    for key in SECTIONS:
        if j2doc.get(key) != r1doc.get(key):
            problems.append(
                "[%s] `%s` differs between the two apply paths\n"
                "        template: %s\n"
                "        r1 file : %s"
                % (
                    branch,
                    key,
                    json.dumps(j2doc.get(key), sort_keys=True, ensure_ascii=False),
                    json.dumps(r1doc.get(key), sort_keys=True, ensure_ascii=False),
                )
            )

    j2r = {r["name"]: r for r in (j2doc.get("receivers") or [])}
    r1r = {r["name"]: r for r in (r1doc.get("receivers") or [])}
    j2names = [r["name"] for r in (j2doc.get("receivers") or [])]
    r1names = [r["name"] for r in (r1doc.get("receivers") or [])]
    if j2names != r1names:
        problems.append(
            "[%s] the receiver LIST differs\n"
            "        template: %s\n"
            "        r1 file : %s" % (branch, j2names, r1names)
        )
    for name in sorted(set(j2r) | set(r1r)):
        if j2r.get(name) != r1r.get(name):
            problems.append(
                "[%s] receiver `%s` differs\n"
                "        template: %s\n"
                "        r1 file : %s"
                % (
                    branch,
                    name,
                    json.dumps(j2r.get(name), sort_keys=True, ensure_ascii=False),
                    json.dumps(r1r.get(name), sort_keys=True, ensure_ascii=False),
                )
            )

if problems:
    print(
        "check-alertmanager-parity: FAIL — the two apply paths do not "
        "render the same Alertmanager config.",
        file=sys.stderr,
    )
    for p in problems:
        print("  ✗ " + p, file=sys.stderr)
    print(
        "\nBoth files produce /etc/prometheus/alertmanager.yml. A difference "
        "here means\napplying one path silently undoes the other — which is "
        "how #485's dead\ninformational routing survived its own fix. Change "
        "both files in the same\ncommit:\n"
        "  configs/alertmanager/alertmanager.r1.yml\n"
        "  configs/ansible/roles/prometheus/templates/alertmanager.yml.j2\n",
        file=sys.stderr,
    )
    sys.exit(1)

print(
    "check-alertmanager-parity: OK — both apply paths render identical "
    "routing, receivers and inhibit rules (wired + dark branches)."
)
PY
