#!/usr/bin/env bash
# ansible-listing-sync-gate-test.sh — listing-sync must be installed and
# enabled on pubnet only, and retired everywhere else.
#
# The unit caches CoinGecko's Stellar platform→address map; every address
# in it is a pubnet asset, so on a test net the job corroborates nothing
# and, keyless from a datacenter address, fails hourly on a 403.
#
# Checks: listing_sync_enabled renders true for pubnet (and when
# stellar_network is unset) and false for testnet/futurenet; every task in
# 14-stellarindex-services.yml that installs or enables a listing-sync unit
# sits under `listing_sync_enabled | bool`, and a block under its negation
# stops, disables and removes both units.
#
# ROLE_TASKS / ROLE_DEFAULTS override the files so a pre-fix tree can be
# replayed (the red proof). Needs a python with jinja2 + PyYAML (ansible's
# own); fail-closed in CI, skip locally.
# Run: bash scripts/ci/ansible-listing-sync-gate-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 2

ROLE="$PWD/configs/ansible/roles/archival-node"
TASKS="${ROLE_TASKS:-$ROLE/tasks}/14-stellarindex-services.yml"
DEFAULTS="${ROLE_DEFAULTS:-$ROLE/defaults/main.yml}"

for f in "$TASKS" "$DEFAULTS"; do
  [ -r "$f" ] || { echo "ansible-listing-sync-gate-test: missing $f" >&2; exit 2; }
done

# Same interpreter discovery as zfs-snapshot-coverage-test.sh.
PY=""
for cand in python3 /opt/homebrew/bin/python3; do
  if command -v "$cand" >/dev/null 2>&1 &&
     "$cand" -c 'import jinja2, yaml' >/dev/null 2>&1; then
    PY="$cand"; break
  fi
done
if [ -z "$PY" ] && command -v ansible-playbook >/dev/null 2>&1; then
  cand=$(head -1 "$(command -v ansible-playbook)" | sed 's|^#!||' | awk '{print $1}')
  if [ -x "$cand" ] && "$cand" -c 'import jinja2, yaml' >/dev/null 2>&1; then PY="$cand"; fi
fi
if [ -z "$PY" ]; then
  if [ "${CI:-}" = "true" ]; then
    echo "ansible-listing-sync-gate-test: FAIL — no python3 with jinja2+PyYAML (required in CI)" >&2
    exit 1
  fi
  echo "ansible-listing-sync-gate-test: SKIP (no python3 with jinja2+PyYAML locally; CI enforces)"
  exit 0
fi

echo "ansible-listing-sync-gate-test"
"$PY" - "$TASKS" "$DEFAULTS" <<'PY_EOF' || exit 1
import json
import sys

import jinja2
import yaml

tasks_path, defaults_path = sys.argv[1:3]
GATE = "listing_sync_enabled | bool"
NOT_GATE = "not (listing_sync_enabled | bool)"
UNITS = {"listing-sync.service", "listing-sync.timer"}
fail = 0


def check(cond, msg):
    global fail
    print(("  ok   " if cond else "  FAIL ") + msg)
    if not cond:
        fail += 1


defaults = yaml.safe_load(open(defaults_path)) or {}
expr = defaults.get("listing_sync_enabled")
check(expr is not None, "defaults/main.yml defines listing_sync_enabled")
if expr is not None:
    for net, want in ((None, True), ("pubnet", True), ("testnet", False), ("futurenet", False)):
        ctx = {} if net is None else {"stellar_network": net}
        got = jinja2.Template(str(expr)).render(**ctx).strip() == "True"
        check(got == want, f"listing_sync_enabled is {want} for stellar_network={net or '<unset>'}")


def whens(node):
    w = node.get("when", [])
    return [str(x).strip() for x in (w if isinstance(w, list) else [w])]


def walk(nodes, inherited):
    for n in nodes or []:
        eff = inherited + whens(n)
        if "block" in n:
            yield from walk(n["block"], eff)
            continue
        yield n, eff


def units_of(task):
    body = json.dumps(task)
    return {u for u in UNITS if u in body}


install, enable, stop, remove = set(), set(), set(), set()
for task, eff in walk(yaml.safe_load(open(tasks_path)), []):
    units = units_of(task)
    if not units:
        continue
    name = task.get("name", "<unnamed>")
    if "ansible.builtin.template" in task:
        check(GATE in eff, f"'{name}' installs {sorted(units)} under `{GATE}`")
        install |= units if GATE in eff else set()
    sd = task.get("ansible.builtin.systemd")
    if isinstance(sd, dict) and sd.get("enabled") is True:
        check(GATE in eff, f"'{name}' enables {sorted(units)} under `{GATE}`")
        enable |= units if GATE in eff else set()
    if isinstance(sd, dict) and sd.get("enabled") is False:
        check(NOT_GATE in eff, f"'{name}' disables {sorted(units)} under `{NOT_GATE}`")
        stop |= units if NOT_GATE in eff else set()
    fl = task.get("ansible.builtin.file")
    if isinstance(fl, dict) and fl.get("state") == "absent":
        check(NOT_GATE in eff, f"'{name}' removes {sorted(units)} under `{NOT_GATE}`")
        remove |= units if NOT_GATE in eff else set()

check(install == UNITS, "pubnet installs listing-sync.service + .timer")
check("listing-sync.timer" in enable, "pubnet enables listing-sync.timer")
check("listing-sync.timer" in stop, "other networks stop + disable listing-sync.timer")
check(remove == UNITS, "other networks remove listing-sync.service + .timer")

print(f"ansible-listing-sync-gate-test: {'FAIL' if fail else 'PASS'} ({fail} failed)")
sys.exit(1 if fail else 0)
PY_EOF
