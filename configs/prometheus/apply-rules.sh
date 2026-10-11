#!/usr/bin/env bash
# Apply configs/prometheus/rules.r1/ to R1 and PROVE the rules loaded.
#
# A binary deploy swaps binaries only; rule files are a separate config
# surface. "Merged" and "live" are different states with nothing reconciling
# them: a new alert can deploy yet watch nothing (file never reached the host),
# and a removed alert can keep FIRING from a file the repo no longer has. The
# final check POLLS rather than sampling once, because a single sample inside
# the propagation window misreads a fired deploy as not-fired.
#
# Usage:
#   apply-rules.sh --check-only [SRC_DIR]   validate only; no install
#   apply-rules.sh --live-check [SRC_DIR]   validate + diff SRC_DIR against the
#                                            alerts $PROM_URL has loaded RIGHT
#                                            NOW; no install. For a scheduled
#                                            reconciliation on the host: apply,
#                                            ci.yml's --check-only and the verify
#                                            step below only run AT deploy time,
#                                            so an out-of-band host edit or a
#                                            restart that fails to reload goes
#                                            undetected until the next deploy.
#   apply-rules.sh [SRC_DIR]                validate, install, reload, verify
#
# SRC_DIR defaults to the directory this script lives in, /rules.r1.
#
# Scope is Prometheus RULE FILES only: pure declarations, validatable before
# install, revertible by restoring one file. Everything else (host config,
# prometheus.yml, migrations) stays with ansible's `--check --diff` review.
#
# Why not the ansible prometheus role: it asserts an inventory group
# `prometheus_pair` of exactly 2 hosts (the future HA pair, ha-plan §7), and
# R1 only defines `archival_nodes`, so the role refuses it by design and names
# configs/prometheus/rules.r1 as R1's out-of-band surface. This script is that
# surface; it retires when R1 joins a prometheus_pair. It borrows the role's two
# safety properties so the paths cannot drift:
#   - stale-file cleanup, because copying never deletes;
#   - the empty-source guard: an empty source dir must ABORT, never mean "delete
#     every rule" (a path typo would reload Prometheus with zero alerts).

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

CHECK_ONLY=0
LIVE_CHECK=0
case "${1:-}" in
  --check-only)
    CHECK_ONLY=1
    shift
    ;;
  --live-check)
    LIVE_CHECK=1
    shift
    ;;
esac

SRC="${1:-$SCRIPT_DIR/rules.r1}"
DEST="${RULES_DEST:-/etc/prometheus/rules.r1}"
PROM_URL="${PROM_URL:-http://localhost:9090}"

# How long to wait for a reloaded Prometheus to publish the new rule set. Reload
# is async, so the rules API briefly answers with the OLD set. 60s is far beyond
# the observed reload time and free when the apply worked.
VERIFY_TIMEOUT_S="${VERIFY_TIMEOUT_S:-60}"

die() { echo "apply-rules: $*" >&2; exit 1; }

[ -d "$SRC" ] || die "source dir not found: $SRC"

command -v promtool >/dev/null 2>&1 || die "promtool not on PATH"

# Checked UP FRONT: without curl or python3 the poll reads an empty rule list,
# waits out its timeout, restores the backup and blames the rules for a missing
# interpreter. (--check-only needs neither.)
if [ "$CHECK_ONLY" != "1" ]; then
  for t in curl python3; do
    command -v "$t" >/dev/null 2>&1 \
      || die "$t not on PATH — required to VERIFY the rules loaded; refusing to install rules we cannot verify"
  done
fi

# Parses a /api/v1/rules JSON body on stdin into one loaded rule name per line.
# Shared by --live-check and the verify poll so they agree on "loaded".
parse_loaded_rule_names() {
  python3 -c 'import sys,json
try: d=json.load(sys.stdin)
except Exception: sys.exit(0)
for g in d.get("data",{}).get("groups",[]):
    for r in g.get("rules",[]):
        n=r.get("name")
        if n: print(n)' 2>/dev/null | sort -u
}

# Prints "name: health" for every rule whose health equals $1 ("err" or
# "unknown"). A rule stuck at "unknown" is not evaluating, so the caller polls it
# like name-presence rather than trusting one sample. Optional $2 skips groups
# whose interval exceeds it: they cannot evaluate inside the window.
rules_with_health() {
  python3 -c 'import sys,json
want=sys.argv[1]
cap=float(sys.argv[2]) if len(sys.argv)>2 else None
try: d=json.load(sys.stdin)
except Exception: sys.exit(0)
for g in d.get("data",{}).get("groups",[]):
    if cap is not None and float(g.get("interval",0))>cap: continue
    for r in g.get("rules",[]):
        if r.get("health")==want:
            print(r.get("name")+": "+str(r.get("health")))' "$@" 2>/dev/null | sort -u
}

# ─── 1. Validate BEFORE touching anything ───
# On the host, not only CI: this checks what is about to be installed, which can
# differ from the repo copy (partial scp, wrong SRC_DIR, local edit).
echo "apply-rules: validating $SRC"
shopt -s nullglob
src_files=("$SRC"/*.yml)
shopt -u nullglob
[ ${#src_files[@]} -gt 0 ] || die "no .yml rule files in $SRC — refusing to install an empty rule set (that would silently disable every alert)"

promtool check rules "${src_files[@]}" >/dev/null \
  || die "promtool rejected the rule set — nothing installed"
echo "apply-rules: ${#src_files[@]} file(s) valid"

# Alert names the incoming set declares. This is what we verify loaded.
expected_alerts="$(grep -hoE '^[[:space:]]*-[[:space:]]*alert:[[:space:]]*[^[:space:]]+' "${src_files[@]}" \
  | sed -E 's/^[[:space:]]*-[[:space:]]*alert:[[:space:]]*//' | sort -u)"
expected_count="$(printf '%s\n' "$expected_alerts" | grep -c . || true)"
[ "$expected_count" -gt 0 ] || die "the incoming rule set declares no alerts — refusing (an empty set would look like a successful apply while disabling everything)"
echo "apply-rules: expecting $expected_count alert(s) to load"

# Recording rule names the incoming set declares. /api/v1/rules reports them under
# the same "name" field as alerts, so --live-check must know them or it reads
# every recording rule as drift ("loaded but NOT in the repo").
expected_records="$(grep -hoE '^[[:space:]]*-[[:space:]]*record:[[:space:]]*[^[:space:]]+' "${src_files[@]}" \
  | sed -E 's/^[[:space:]]*-[[:space:]]*record:[[:space:]]*//' | sort -u)"

if [ "$CHECK_ONLY" = "1" ]; then
  echo "apply-rules: --check-only, stopping before install"
  exit 0
fi

if [ "$LIVE_CHECK" = "1" ]; then
  expected_names="$(printf '%s\n%s\n' "$expected_alerts" "$expected_records" | grep -v '^$' | sort -u)"
  loaded="$(curl -sf --max-time 10 "$PROM_URL/api/v1/rules" 2>/dev/null | parse_loaded_rule_names || true)"
  missing="$(comm -23 <(printf '%s\n' "$expected_names") <(printf '%s\n' "$loaded") || true)"
  extra="$(comm -13 <(printf '%s\n' "$expected_names") <(printf '%s\n' "$loaded") || true)"
  status=0
  if [ -n "$missing" ]; then
    echo "apply-rules: live-check: in the repo but NOT loaded on $PROM_URL:" >&2
    printf '  %s\n' "$missing" >&2
    status=1
  fi
  if [ -n "$extra" ]; then
    echo "apply-rules: live-check: loaded on $PROM_URL but NOT in the repo (stale host or out-of-band change):" >&2
    printf '  %s\n' "$extra" >&2
    status=1
  fi
  expected_record_count="$(printf '%s\n' "$expected_records" | grep -c . || true)"
  [ "$status" = 0 ] && echo "apply-rules: live-check OK — $PROM_URL matches the repo ($expected_count alert(s), $expected_record_count recording rule(s))"
  exit "$status"
fi

[ -d "$DEST" ] || die "destination not found: $DEST"

# ─── 2. Atomic install, with a backup we can restore ───
STAMP="$(date +%Y%m%d-%H%M%S)"
BACKUP="${DEST}.bak-${STAMP}"
cp -a "$DEST" "$BACKUP"
echo "apply-rules: backup at $BACKUP"

for f in "${src_files[@]}"; do
  install -m 0644 "$f" "$DEST/$(basename "$f")"
done
chown -R prometheus:prometheus "$DEST" 2>/dev/null || true

# Rule files deleted from the repo must disappear from the host too, or a
# removed rule keeps firing.
for existing in "$DEST"/*.yml; do
  base="$(basename "$existing")"
  [ -f "$SRC/$base" ] || { echo "apply-rules: removing $base (no longer in the repo)"; rm -f "$existing"; }
done

restore() {
  echo "apply-rules: restoring $BACKUP" >&2
  rm -rf "$DEST"
  cp -a "$BACKUP" "$DEST"
  reload_prometheus || true
}

# ─── 3. Reload ───
# The HTTP lifecycle API is DISABLED on r1 (--web.enable-lifecycle unset), so
# POST /-/reload changes nothing; use a signal.
reload_prometheus() {
  systemctl reload prometheus 2>/dev/null && return 0
  local pid
  pid="$(pgrep -x prometheus | sed -n '1p')" || return 1
  [ -n "$pid" ] && kill -HUP "$pid"
}
reload_prometheus || { restore; die "could not reload prometheus"; }
echo "apply-rules: reloaded"

# ─── 4. Verify the rules loaded AND are healthy, by POLLING ───
# Reload is async, so one sample would report a working apply as broken. A bad
# expression loads fine then evaluates to errors, so name-presence is not proof.
# "unknown" is the transient state before the first evaluation: poll until it
# settles. "err" never improves and fails fast.
deadline=$(( $(date +%s) + VERIFY_TIMEOUT_S ))
while :; do
  rules_json="$(curl -sf --max-time 10 "$PROM_URL/api/v1/rules" 2>/dev/null || true)"
  loaded="$(printf '%s' "$rules_json" | parse_loaded_rule_names)"
  missing="$(comm -23 <(printf '%s\n' "$expected_alerts") <(printf '%s\n' "$loaded") || true)"

  errored="$(printf '%s' "$rules_json" | rules_with_health err)"
  if [ -n "$errored" ]; then
    echo "apply-rules: rule(s) loaded but errored:" >&2
    printf '  %s\n' "$errored" >&2
    restore
    die "verification failed — rules restored from backup"
  fi

  pending="$(printf '%s' "$rules_json" | rules_with_health unknown "$VERIFY_TIMEOUT_S")"
  if [ -z "$missing" ] && [ -z "$pending" ]; then
    slow="$(printf '%s' "$rules_json" | rules_with_health unknown | wc -l | tr -d ' ')"
    echo "apply-rules: verified — all $expected_count alert(s) loaded and healthy ($slow in groups slower than ${VERIFY_TIMEOUT_S}s not yet evaluated)"
    break
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "apply-rules: never loaded/healthy after ${VERIFY_TIMEOUT_S}s:" >&2
    printf '  %s\n' "$missing" "$pending" >&2
    restore
    die "verification failed — rules restored from backup"
  fi
  sleep 3
done

# ─── 5. Prune old backups ───
# One backup per apply, every deploy; unpruned they fill the disk whose pressure
# is the top cause of the ClickHouse outage this script's first alert catches.
# Keep 5, matching the deploy workflow's binary-backup retention.
find "$(dirname "$DEST")" -maxdepth 1 -name "$(basename "$DEST").bak-*" -printf '%T@ %p\n' 2>/dev/null \
  | sort -rn | tail -n +6 | cut -d' ' -f2- | while read -r old; do
  rm -rf "$old"
done

echo "apply-rules: OK"
