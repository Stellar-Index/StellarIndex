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
#                                            NOW and byte-compare it with the
#                                            files in $RULES_DEST; no install. For a scheduled
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

# Prints "name: err" for every errored rule, scoped by which rule files this
# apply changes ($CHANGED_FILES, basenames, one per line): "changed" keeps rules
# in a changed file (or with no file reported: fail closed), "unchanged" keeps
# the rest. An error in a file this apply did not touch is not caused by it and
# a rollback would not cure it.
errored_rules_scoped() {
  CHANGED="${CHANGED_FILES:-}" python3 -c 'import sys,json,os
scope=sys.argv[1]
changed=set(os.environ.get("CHANGED","").split())
try: d=json.load(sys.stdin)
except Exception: sys.exit(0)
for g in d.get("data",{}).get("groups",[]):
    base=os.path.basename(g.get("file") or "")
    mine = (not base) or base in changed
    if (scope=="changed") != mine: continue
    for r in g.get("rules",[]):
        if r.get("health")=="err":
            print(r.get("name")+": err")' "$1" 2>/dev/null | sort -u
}

# Prints Prometheus's last successful config/rules reload time (it only
# advances on success), empty when it cannot be read.
prom_reload_ts() {
  curl -sf --max-time 10 --get --data-urlencode 'query=prometheus_config_last_reload_success_timestamp_seconds' \
    "$PROM_URL/api/v1/query" 2>/dev/null \
    | python3 -c 'import sys,json
try: print(json.load(sys.stdin)["data"]["result"][0]["value"][1])
except Exception: pass' 2>/dev/null || true
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
  # Names alone pass a weakened expr/for/threshold under the same alert name; the installed bytes must be the repo's.
  if [ ! -d "$DEST" ]; then
    echo "apply-rules: live-check: $DEST not found — cannot compare the installed rule files" >&2
    status=1
  else
    for f in "${src_files[@]}"; do
      if ! cmp -s -- "$f" "$DEST/${f##*/}"; then
        echo "apply-rules: live-check: $DEST/${f##*/} is missing or differs from the repo's ${f##*/}" >&2
        status=1
      fi
    done
    shopt -s nullglob
    for f in "$DEST"/*.yml; do
      if [ ! -e "$SRC/${f##*/}" ]; then
        echo "apply-rules: live-check: $f is installed but NOT in the repo" >&2
        status=1
      fi
    done
    shopt -u nullglob
  fi
  expected_record_count="$(printf '%s\n' "$expected_records" | grep -c . || true)"
  [ "$status" = 0 ] && echo "apply-rules: live-check OK — $PROM_URL matches the repo ($expected_count alert(s), $expected_record_count recording rule(s))"
  exit "$status"
fi

[ -d "$DEST" ] || die "destination not found: $DEST"

# ─── 2. Install by directory swap, with a backup we can restore ───
# Two callers (release deploy, main-push workflow) run in different concurrency
# groups, so serialise on the host: an interleaved apply would back up, delete
# and restore each other's files.
command -v flock >/dev/null 2>&1 || die "flock not on PATH — refusing an unserialised apply"
exec 9>"${DEST}.lock"
flock -w 120 9 || die "another apply holds ${DEST}.lock"

[ -L "$DEST" ] && die "destination is a symlink: $DEST — refusing to swap it"

STAMP="$(date +%Y%m%d-%H%M%S)-$$"
BACKUP="${DEST}.bak-${STAMP}"

CHANGED_FILES=""
for f in "${src_files[@]}"; do
  d="$DEST/$(basename "$f")"
  # Only a regular file is compared: a FIFO planted in the old directory would block cmp forever.
  if [ -f "$d" ] && [ ! -L "$d" ] && cmp -s "$f" "$d"; then continue; fi
  CHANGED_FILES+="$(basename "$f")"$'\n'
done
reload_before="$(prom_reload_ts)"

# Build the new set in a fresh root-only directory and swap it in by rename: a
# failure leaves the live directory untouched, and root never writes inside a
# directory the shared prometheus uid can modify. Rule files deleted from the
# repo disappear because the new directory holds only the repo's files.
STAGE="$(mktemp -d "${DEST}.new.XXXXXX")"
swapped=0
restore() {
  echo "apply-rules: restoring $BACKUP" >&2
  rm -rf "$DEST"
  cp -a "$BACKUP" "$DEST"
  reload_prometheus || true
}
on_abort() {
  trap - ERR HUP INT TERM
  rm -rf "$STAGE"
  if [ "$swapped" = 1 ]; then restore; fi
  die "aborted — live rules left as they were before this apply"
}
trap on_abort ERR HUP INT TERM

for f in "${src_files[@]}"; do
  install -m 0640 "$f" "$STAGE/$(basename "$f")"
done
# Read-only for the daemon, as roles/prometheus installs its rule files.
chown -R root:prometheus "$STAGE"
chmod 0750 "$STAGE"

mv "$DEST" "$BACKUP"
swapped=1
mv "$STAGE" "$DEST"
echo "apply-rules: installed; previous set kept at $BACKUP"

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

# The rules API answers with the OLD rule set until the reload lands, so a
# healthy-looking first sample proves nothing. Wait for the reload timestamp to
# advance (it only does on success) before reading any verdict.
if [ -z "$reload_before" ]; then
  echo "apply-rules: WARNING: could not read the reload timestamp; verifying without the reload barrier" >&2
else
  while :; do
    reload_now="$(prom_reload_ts)"
    if [ -n "$reload_now" ] && awk -v a="$reload_now" -v b="$reload_before" 'BEGIN{exit !(a>b)}'; then
      break
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "apply-rules: prometheus did not complete a successful reload within ${VERIFY_TIMEOUT_S}s" >&2
      restore
      die "verification failed — rules restored from backup"
    fi
    sleep 1
  done
fi

while :; do
  rules_json="$(curl -sf --max-time 10 "$PROM_URL/api/v1/rules" 2>/dev/null || true)"
  loaded="$(printf '%s' "$rules_json" | parse_loaded_rule_names)"
  missing="$(comm -23 <(printf '%s\n' "$expected_alerts") <(printf '%s\n' "$loaded") || true)"

  errored="$(printf '%s' "$rules_json" | errored_rules_scoped changed)"
  preexisting="$(printf '%s' "$rules_json" | errored_rules_scoped unchanged)"
  if [ -n "$preexisting" ] && [ -z "${warned_preexisting:-}" ]; then
    warned_preexisting=1
    echo "apply-rules: WARNING: rule(s) in files this apply did not change are errored (not rolled back):" >&2
    printf '  %s\n' "$preexisting" >&2
  fi
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
trap - ERR HUP INT TERM

# ─── 5. Prune old backups ───
# One backup per apply, every deploy; unpruned they fill the disk whose pressure
# is the top cause of the ClickHouse outage this script's first alert catches.
# Keep 5, matching the deploy workflow's binary-backup retention.
find "$(dirname "$DEST")" -maxdepth 1 -name "$(basename "$DEST").bak-*" -printf '%T@ %p\n' 2>/dev/null \
  | sort -rn | tail -n +6 | cut -d' ' -f2- | while read -r old; do
  rm -rf "$old"
done

echo "apply-rules: OK"
