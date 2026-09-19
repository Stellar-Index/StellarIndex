#!/usr/bin/env bash
# zfs-snapshot-coverage-test.sh — WHICH datasets the rolling-snapshot job
# actually covers (NS03, audit-2026-09-02).
#
# scripts/ci/zfs-snapshot-test.sh pins how the job BEHAVES on the datasets
# it is given. Nothing pinned WHICH ones it is given, and that is where the
# hole was: the list was `data/clickhouse:3 data/postgres:7` — the two
# tiers that are DERIVED from the Galexie LCM archive — while the archive
# itself (`data/minio`, measured 2.64 TB on r1 2026-09-19) had zero
# snapshots and, pgBackRest's Postgres-only repo2 aside, no off-host copy
# either. Protecting the derivatives and not the source is the wrong way
# round: an `mc rm --recursive` against the wrong prefix had nothing to
# roll back to, and the only remaining path was a re-ingest of the public
# Stellar history archives (days–weeks, no third-party SLA).
#
# Three things have to agree, and they live in three files that are never
# read together:
#
#   1. the role's `zfs_snapshot_datasets` (defaults/main.yml), rendered
#      through the REAL zfs-snapshot.env.j2 into /etc/default/zfs-snapshot;
#   2. the built-in `ZFS_SNAPSHOT_DATASETS` default in
#      scripts/ops/zfs-snapshot.sh — what a hand-run gets when the env
#      file is missing or an operator's shell has not sourced it;
#   3. the set of durable stores that MUST be covered, derived from the
#      role's own `zfs_datasets` mountpoints rather than hardcoded here,
#      so renaming the dataset cannot quietly drop the coverage.
#
# The parse arm feeds the script's own default through the script's own
# `parse_datasets` (it is sourceable for exactly this), so the assertion
# is on what the shipped code resolves, not on a regex over its text.
#
# Run: bash scripts/ci/zfs-snapshot-coverage-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 2

ROLE="${ROLE:-configs/ansible/roles/archival-node}"
DEFAULTS="${DEFAULTS:-$ROLE/defaults/main.yml}"
TEMPLATE="${TEMPLATE:-$ROLE/templates/zfs-snapshot.env.j2}"
SCRIPT="${SCRIPT:-scripts/ops/zfs-snapshot.sh}"

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

for f in "$DEFAULTS" "$TEMPLATE" "$SCRIPT"; do
  [ -r "$f" ] || { echo "zfs-snapshot-coverage-test: missing $f" >&2; exit 2; }
done

# ─── 1+3. render the role's list and check what it covers ──────────────
# Same interpreter discovery as clickhouse-exporter-test.sh: ansible ships
# its own python with jinja2 + PyYAML. Fail-closed in CI, skip locally.
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

RENDERED=""
if [ -z "$PY" ]; then
  if [ "${CI:-}" = "true" ]; then
    echo "zfs-snapshot-coverage-test: FAIL — no python3 with jinja2+PyYAML (required in CI)" >&2
    exit 1
  fi
  echo "  SKIP render checks (no python3 with jinja2+PyYAML locally; CI enforces)"
else
  echo "zfs-snapshot-coverage-test: role list"
  RENDERED="$("$PY" - "$TEMPLATE" "$DEFAULTS" <<'PY_EOF'
import sys

import jinja2
import yaml

template_path, defaults_path = sys.argv[1], sys.argv[2]
with open(template_path, encoding="utf-8") as fh:
    src = fh.read()
with open(defaults_path, encoding="utf-8") as fh:
    defaults = yaml.safe_load(fh) or {}

env = jinja2.Environment(
    trim_blocks=True, lstrip_blocks=False, keep_trailing_newline=True,
    undefined=jinja2.StrictUndefined,
)
out = env.from_string(src).render(**defaults)
for line in out.splitlines():
    if line.startswith("ZFS_SNAPSHOT_DATASETS="):
        print(line.split("=", 1)[1].strip().strip('"'))
        break
PY_EOF
)"
  rc=$?
  if [ "$rc" -ne 0 ] || [ -z "$RENDERED" ]; then
    bad "zfs-snapshot.env.j2 renders a ZFS_SNAPSHOT_DATASETS line (rc $rc)"
  else
    ok "zfs-snapshot.env.j2 renders: $RENDERED"
  fi

  # The datasets that MUST be covered, named by what they HOLD. Derived
  # from the role's own zfs_datasets mountpoints (minio_data_path for the
  # Galexie LCM archive, /var/lib/postgresql, /var/lib/clickhouse), so a
  # rename moves the requirement instead of silently voiding it.
  "$PY" - "$DEFAULTS" "$RENDERED" <<'PY_EOF'
import sys

import yaml

defaults_path, rendered = sys.argv[1], sys.argv[2]
with open(defaults_path, encoding="utf-8") as fh:
    defaults = yaml.safe_load(fh) or {}

pool = defaults["zfs_data_pool_name"]
covered = {}
for entry in rendered.split():
    name, _, days = entry.rpartition(":")
    covered[name] = int(days)

by_mount = {d.get("mountpoint"): d["name"] for d in defaults["zfs_datasets"]}
required = [
    (defaults["minio_data_path"],
     "the Galexie LCM archive — the CDP source of truth the lake and the "
     "served tier are re-derived from (NS03)"),
    ("/var/lib/postgresql", "the served money state"),
    ("/var/lib/clickhouse", "the Tier-1 lake"),
]

rc = 0
for mount, why in required:
    ds = by_mount.get(mount)
    if ds is None:
        print("  FAIL no zfs_datasets entry mounts %s — cannot check coverage" % mount)
        rc = 1
        continue
    full = "%s/%s" % (pool, ds)
    if full in covered:
        print("  ok   %s is snapshotted (%d d) — %s" % (full, covered[full], why))
    else:
        print("  FAIL %s has NO rolling snapshot — %s" % (full, why))
        rc = 1
sys.exit(rc)
PY_EOF
  rc=$?
  if [ "$rc" -eq 0 ]; then pass=$((pass + 3)); else fail=$((fail + 1)); fi
fi

# ─── 2. the script's own default, through the script's own parser ──────
echo "zfs-snapshot-coverage-test: script default"

# Sourced, not regexed: main() is guarded by the BASH_SOURCE check, so
# this loads the real definitions and then asks parse_datasets what the
# shipped default resolves to. ZFS_SNAPSHOT_DATASETS must be unset for
# the built-in default to apply.
(
  unset ZFS_SNAPSHOT_DATASETS
  TEXTFILE_DIR=/dev/null
  export TEXTFILE_DIR
  # shellcheck disable=SC1090  # the script under test, by path
  . "./$SCRIPT"
  parse_datasets
  printf '%s\n' "${DATASETS[*]}"
  printf '%s\n' "${RETENTION_DAYS[*]}"
) > "${TMPDIR:-/tmp}/zfs-cov-$$.out" 2>&1
rc=$?
parsed_ds=""
parsed_days=""
if [ "$rc" -eq 0 ]; then
  parsed_ds="$(sed -n '1p' "${TMPDIR:-/tmp}/zfs-cov-$$.out")"
  parsed_days="$(sed -n '2p' "${TMPDIR:-/tmp}/zfs-cov-$$.out")"
  ok "zfs-snapshot.sh parses its built-in default: $parsed_ds"
else
  bad "zfs-snapshot.sh could not parse its built-in default (rc $rc)"
  sed 's/^/       /' "${TMPDIR:-/tmp}/zfs-cov-$$.out"
fi
rm -f "${TMPDIR:-/tmp}/zfs-cov-$$.out"

# The archive is in the built-in default too, not only in the rendered
# env file: zfs-snapshot-now.sh and an operator shell both fall back to it.
case " $parsed_ds " in
  *" data/minio "*)
    ok "the built-in default covers data/minio (the Galexie archive)" ;;
  *)
    bad "the built-in default does not cover data/minio — a hand-run or a host with no /etc/default/zfs-snapshot leaves the archive unprotected" ;;
esac

# ─── 1 ↔ 2 parity ──────────────────────────────────────────────────────
if [ -n "$RENDERED" ]; then
  # Order-insensitive: the rendered file and the built-in default must
  # describe the same {dataset: retention} map, not the same string.
  # awk 'NF' drops the empty fields a trailing separator leaves behind; it
  # reads to EOF, so no early-exit consumer under pipefail (#475).
  norm() { printf '%s\n' "$1" | tr ' ' '\n' | awk 'NF' | sort | tr '\n' ' '; }
  script_list=""
  i=1
  for d in $parsed_ds; do
    days="$(printf '%s\n' "$parsed_days" | cut -d' ' -f"$i")"
    script_list="$script_list$d:$days "
    i=$((i + 1))
  done
  if [ "$(norm "$RENDERED")" = "$(norm "$script_list")" ]; then
    ok "role default and script default agree ($(norm "$RENDERED"))"
  else
    bad "role default [$(norm "$RENDERED")] != script default [$(norm "$script_list")] — a host with no env file snapshots a different set"
  fi
fi

echo
echo "zfs-snapshot-coverage-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
