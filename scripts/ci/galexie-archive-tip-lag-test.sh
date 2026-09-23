#!/usr/bin/env bash
# galexie-archive-tip-lag-test.sh — fixture tests for
# configs/ansible/roles/archival-node/files/galexie-archive-tip-lag.sh,
# the 5-minute live-vs-archive tip probe behind the
# stellarindex_galexie_archive_tip_lag_* alerts.
#
# THE DEFECT THESE PIN. Every `mc ls` was `2>/dev/null || true` and an
# empty listing became ledger 0, so an unreadable bucket (wrong
# credentials, MinIO down, bucket gone) published
# `galexie_archive_tip_lag_ledgers 0` plus a fresh
# `galexie_archive_tip_lag_updated_seconds` — "archive exactly at the
# live tip", the healthy answer — and disarmed every threshold.
#
# What must hold:
#   1. a healthy read publishes both tips, the lag, probe_success 1 and
#      a fresh success stamp;
#   2. a failed or empty read publishes probe_success 0 and NO lag or
#      tip gauge (absent, never a fabricated 0), carries the previous
#      success stamp forward unchanged, and exits non-zero;
#   3. with no previous file there is no stamp to carry, and none is
#      invented;
#   4. the output is valid Prometheus text and no temp file survives;
#   5. the script's own --self-test (object-name parser) passes.
#
# The SHIPPED script is executed with `mc` stubbed on PATH and
# TEXTFILE_DIR redirected: no MinIO, no systemd, no write outside TMPDIR.
#
# Run: bash scripts/ci/galexie-archive-tip-lag-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
PROBE="$PWD/configs/ansible/roles/archival-node/files/galexie-archive-tip-lag.sh"
[[ -r "$PROBE" ]] || { echo "galexie-archive-tip-lag-test: missing $PROBE" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

# Stub: LIVE_MODE / ARCHIVE_MODE = ok | fail | empty per bucket.
mkdir -p "$TMP/bin" "$TMP/textfile"
cat > "$TMP/bin/mc" <<'SH'
#!/usr/bin/env bash
[ "$1" = "ls" ] || exit 64
case "$2" in
  local/galexie-live/*) mode="${LIVE_MODE:-ok}" part="FFFE0C6E--127872-191871" tip="FFFE0C6E--150000.xdr.zst" ;;
  local/galexie-archive/*) mode="${ARCHIVE_MODE:-ok}" part="FFFE0C6F--63936-127871" tip="FFFE0C6F--127871.xdr.zst" ;;
  *) exit 64 ;;
esac
case "$mode" in
  fail) echo "mc: <ERROR> Unable to list. Access Denied." >&2; exit 1 ;;
  empty) exit 0 ;;
esac
case "$2" in
  */*/*/) printf '[2026-09-05 10:00:00 UTC] 1.2KiB STANDARD %s\n' "$tip" ;;
  *)      printf '[2026-09-05 10:00:00 UTC]     0B %s/\n' "$part" ;;
esac
SH
chmod +x "$TMP/bin/mc"

OUT="$TMP/textfile/galexie_archive_tip_lag.prom"
run() {  # run <live-mode> <archive-mode>; sets rc
  PATH="$TMP/bin:$PATH" TEXTFILE_DIR="$TMP/textfile" LIVE_MODE="$1" ARCHIVE_MODE="$2" \
    bash "$PROBE" > "$TMP/stdout" 2> "$TMP/stderr"
  rc=$?
}
value() { awk -v m="$1" '$1 == m {print $2; exit}' "$OUT"; }
has() { grep -q "^$1 " "$OUT"; }
valid() {
  if command -v promtool > /dev/null; then
    promtool check metrics < "$OUT" > /dev/null 2>&1
  else
    awk '!/^#/ && NF && NF != 2 {bad=1} END {exit bad}' "$OUT"
  fi
}

eq() {  # eq <label> <got> <want>
  if [ "$2" = "$3" ]; then ok "$1"; else bad "$1: got '$2', want '$3'"; fi
}
check() {  # check <label> <command...>
  local label="$1"
  shift
  if "$@"; then ok "$label"; else bad "$label"; fi
}
fresh() { [[ "$1" =~ ^[0-9]+$ ]] && [ "$1" -ge $(($(date +%s) - 60)) ]; }
unmeasured_absent() {
  ! has galexie_archive_tip_lag_ledgers && ! has galexie_live_tip_ledger && ! has galexie_archive_tip_ledger
}

echo "galexie-archive-tip-lag-test:"

# 1. healthy
run ok ok
eq "healthy read: exit status" "$rc" 0
eq "healthy read: probe_success" "$(value galexie_archive_tip_lag_probe_success)" 1
eq "healthy read: live tip" "$(value galexie_live_tip_ledger)" 150000
eq "healthy read: archive tip (range end)" "$(value galexie_archive_tip_ledger)" 127871
eq "healthy read: lag" "$(value galexie_archive_tip_lag_ledgers)" 22129
check "healthy read: fresh success stamp" fresh "$(value galexie_archive_tip_lag_updated_seconds)"
check "healthy read: valid exposition" valid

# Age the stamp so a carried value is distinguishable from a fresh one.
sed -i.bak 's/^galexie_archive_tip_lag_updated_seconds .*/galexie_archive_tip_lag_updated_seconds 1700000000/' "$OUT" && rm -f "$OUT.bak"

# 2. failed / empty reads, with a previous file present
for c in "ok fail:archive bucket unreadable" "fail ok:live bucket unreadable" "empty ok:live bucket lists nothing" "ok empty:archive bucket lists nothing"; do
  modes="${c%%:*}" label="${c#*:}"
  # shellcheck disable=SC2086 # two modes, split on purpose
  run $modes
  check "$label: exits non-zero" test "$rc" -ne 0
  eq "$label: probe_success" "$(value galexie_archive_tip_lag_probe_success)" 0
  check "$label: lag and tips omitted, not fabricated" unmeasured_absent
  eq "$label: previous success stamp carried unchanged" "$(value galexie_archive_tip_lag_updated_seconds)" 1700000000
  check "$label: valid exposition" valid
done

# 3. failure with no previous file
rm -f "$OUT"
run ok fail
eq "first-run failure: probe_success" "$(value galexie_archive_tip_lag_probe_success)" 0
eq "first-run failure: no success stamp invented" "$(value galexie_archive_tip_lag_updated_seconds)" ""

# 4. no temp file survives
eq "no temp file left behind" "$(find "$TMP/textfile" -name '*.tmp.*')" ""

# 5. the parser self-test
check "--self-test passes" bash "$PROBE" --self-test

echo "galexie-archive-tip-lag-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
