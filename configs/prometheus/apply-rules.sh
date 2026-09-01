#!/usr/bin/env bash
# Apply configs/prometheus/rules.r1/ to R1 — and PROVE the rules loaded.
#
# A binary deploy swaps binaries only; Prometheus rule files are a
# separate config surface. That gap bit three times on 2026-09-01:
#
#   - #462 added a ClickHouse availability alert. Merged, released,
#     deployed — and watching nothing, because the rule file never
#     reached the host. It was the only health signal ClickHouse had.
#   - #465 removed stellarindex_recognition_unattributed_jump. It kept
#     FIRING on r1 for hours afterwards, from a rule file the repo no
#     longer contained.
#   - A deploy that had in fact fired was read as not-fired from a
#     single `gh run list` sampled inside the propagation window, and a
#     duplicate was launched.
#
# The first two are the same defect: "merged" and "live" are different
# states with nothing reconciling them. The third is why the check at
# the end of this script POLLS instead of sampling once.
#
# Usage:
#   apply-rules.sh --check-only [SRC_DIR]   validate only; no install
#   apply-rules.sh [SRC_DIR]                validate, install, reload, verify
#
# SRC_DIR defaults to the directory this script lives in, /rules.r1.
#
# Scope is deliberately Prometheus RULE FILES and nothing else. Every
# other config surface — ansible-managed host config, prometheus.yml
# itself, migrations — stays with ansible and its `--check --diff`
# review, and the deploy's config-apply gate must keep failing for
# those. Rule files are the narrow case that is safe to automate: pure
# declarations, validatable before install, and revertible by restoring
# one file.
#
# Why this is not just the ansible prometheus role: that role asserts an
# inventory group `prometheus_pair` of exactly 2 hosts (the future HA
# pair, ha-plan §7). R1's inventory defines only `archival_nodes`, so
# the role REFUSES to run against it — deliberately, and its own
# preflight says so, naming configs/prometheus/rules.r1 as R1's
# out-of-band surface. This script is that out-of-band surface, made
# reconciling and verified. When R1 joins a prometheus_pair the role
# takes over and this script retires.
#
# Its two safety properties are borrowed from that role on purpose, so
# the single-host and HA paths cannot drift in behaviour:
#   - stale-file cleanup, because copying never deletes;
#   - the F-1357 guard: an empty source directory must ABORT, never be
#     read as "delete every rule". A path typo would otherwise reload
#     Prometheus with zero alerts and look like a clean run.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

CHECK_ONLY=0
if [ "${1:-}" = "--check-only" ]; then
  CHECK_ONLY=1
  shift
fi

SRC="${1:-$SCRIPT_DIR/rules.r1}"
DEST="${RULES_DEST:-/etc/prometheus/rules.r1}"
PROM_URL="${PROM_URL:-http://localhost:9090}"

# How long to wait for a reloaded Prometheus to publish the new rule
# set. Prometheus re-reads on SIGHUP asynchronously, so the rules API
# briefly still answers with the OLD set — sampling once inside that
# window reports a successful apply as a failure. 60s is far beyond the
# observed reload time and costs nothing when the apply worked.
VERIFY_TIMEOUT_S="${VERIFY_TIMEOUT_S:-60}"

die() { echo "apply-rules: $*" >&2; exit 1; }

[ -d "$SRC" ] || die "source dir not found: $SRC"

command -v promtool >/dev/null 2>&1 || die "promtool not on PATH"

# Checked UP FRONT, not discovered during verification. Without curl or
# python3 the poll below reads an empty rule list, waits out its full
# timeout, then restores the backup and reports "these alerts never
# loaded" — blaming the rules for a missing interpreter. Fail here with
# the real reason instead. (--check-only needs neither, so this runs
# after the early exit would have been possible but before any install.)
if [ "$CHECK_ONLY" != "1" ]; then
  for t in curl python3; do
    command -v "$t" >/dev/null 2>&1 \
      || die "$t not on PATH — required to VERIFY the rules loaded; refusing to install rules we cannot verify"
  done
fi

# ─── 1. Validate BEFORE touching anything ─────────────────────────
#
# On the host, not only in CI. CI validates the repo's copy; this
# validates what is about to be installed, which is the thing that can
# differ (a partial scp, a wrong SRC_DIR, a local edit).
echo "apply-rules: validating $SRC"
shopt -s nullglob
src_files=("$SRC"/*.yml)
shopt -u nullglob
[ ${#src_files[@]} -gt 0 ] || die "no .yml rule files in $SRC — refusing to install an empty rule set (that would silently disable every alert)"

promtool check rules "${src_files[@]}" >/dev/null \
  || die "promtool rejected the rule set — nothing installed"
echo "apply-rules: ${#src_files[@]} file(s) valid"

# Alert names the incoming set declares. This is what we verify loaded.
expected_alerts="$(grep -hoE '^\s*-\s*alert:\s*\S+' "${src_files[@]}" \
  | sed -E 's/^\s*-\s*alert:\s*//' | sort -u)"
expected_count="$(printf '%s\n' "$expected_alerts" | grep -c . || true)"
[ "$expected_count" -gt 0 ] || die "the incoming rule set declares no alerts — refusing (an empty set would look like a successful apply while disabling everything)"
echo "apply-rules: expecting $expected_count alert(s) to load"

if [ "$CHECK_ONLY" = "1" ]; then
  echo "apply-rules: --check-only, stopping before install"
  exit 0
fi

[ -d "$DEST" ] || die "destination not found: $DEST"

# ─── 2. Atomic install, with a backup we can restore ──────────────
STAMP="$(date +%Y%m%d-%H%M%S)"
BACKUP="${DEST}.bak-${STAMP}"
cp -a "$DEST" "$BACKUP"
echo "apply-rules: backup at $BACKUP"

for f in "${src_files[@]}"; do
  install -m 0644 "$f" "$DEST/$(basename "$f")"
done
chown -R prometheus:prometheus "$DEST" 2>/dev/null || true

# Rule files DELETED from the repo must disappear from the host too.
# This is the half that was missing on 2026-09-01: #465's alert kept
# firing because removing it from git removed it from nowhere else.
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

# ─── 3. Reload ────────────────────────────────────────────────────
#
# The HTTP lifecycle API is DISABLED on r1 (--web.enable-lifecycle is
# not set), so POST /-/reload returns "Lifecycle API is not enabled"
# and changes nothing. Discovered the hard way; use a signal.
reload_prometheus() {
  systemctl reload prometheus 2>/dev/null && return 0
  local pid
  pid="$(pgrep -x prometheus | head -1)" || return 1
  [ -n "$pid" ] && kill -HUP "$pid"
}
reload_prometheus || { restore; die "could not reload prometheus"; }
echo "apply-rules: reloaded"

# ─── 4. Verify the rules actually loaded — POLLING ────────────────
#
# Without this the script automates "copied a file", not "the alert is
# watching production". With a single sample it would ALSO report a
# working apply as broken, because the reload is asynchronous.
deadline=$(( $(date +%s) + VERIFY_TIMEOUT_S ))
while :; do
  loaded="$(curl -sf --max-time 10 "$PROM_URL/api/v1/rules" 2>/dev/null \
    | python3 -c 'import sys,json
try: d=json.load(sys.stdin)
except Exception: sys.exit(0)
for g in d.get("data",{}).get("groups",[]):
    for r in g.get("rules",[]):
        n=r.get("name")
        if n: print(n)' 2>/dev/null | sort -u || true)"

  missing="$(comm -23 <(printf '%s\n' "$expected_alerts") <(printf '%s\n' "$loaded") || true)"
  if [ -z "$missing" ]; then
    echo "apply-rules: verified — all $expected_count alert(s) loaded"
    break
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "apply-rules: these alerts never loaded after ${VERIFY_TIMEOUT_S}s:" >&2
    printf '  %s\n' $missing >&2
    restore
    die "verification failed — rules restored from backup"
  fi
  sleep 3
done

# Any rule group that loaded but is unhealthy is also a failed apply:
# a group with a bad expression loads and then evaluates to errors, so
# name-presence alone is not proof it works.
unhealthy="$(curl -sf --max-time 10 "$PROM_URL/api/v1/rules" 2>/dev/null \
  | python3 -c 'import sys,json
d=json.load(sys.stdin)
for g in d.get("data",{}).get("groups",[]):
    for r in g.get("rules",[]):
        if r.get("health") not in (None,"ok","unknown"):
            print(f"{r.get(\"name\")}: {r.get(\"health\")}")' 2>/dev/null || true)"
if [ -n "$unhealthy" ]; then
  echo "apply-rules: rules loaded but are unhealthy:" >&2
  printf '  %s\n' "$unhealthy" >&2
  restore
  die "verification failed — rules restored from backup"
fi

# ─── 5. Prune old backups ─────────────────────────────────────────
#
# One backup per apply, and this runs on every deploy — unpruned they
# accumulate on the box whose disk pressure is the top cause of the
# ClickHouse outage this script's first alert exists to catch. Keep 5,
# matching the deploy workflow's retention for binary backups.
ls -1dt "${DEST}.bak-"* 2>/dev/null | tail -n +6 | while read -r old; do
  rm -rf "$old"
done

echo "apply-rules: OK"
