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
# BSD mv has no -T: emulate GNU's refusal to move into an existing directory.
if [[ "$(mv --version 2>/dev/null)" != *GNU* ]]; then
  cat > "$TMP/bin/mv" <<'MV'
#!/usr/bin/env bash
[ "$1" = -T ] || exec /bin/mv "$@"
shift; [ "$1" = -- ] && shift
[ -d "$2" ] && { echo "mv: cannot overwrite directory '$2'" >&2; exit 1; }
exec /bin/mv -- "$1" "$2"
MV
  chmod +x "$TMP/bin/mv"
fi
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
  manifest) printf '[2026-09-05 10:00:00 UTC] 1.0KiB STANDARD .config.json\n'; exit 0 ;;
esac
# LIVE_FIRST adds the oldest live partition/object (last rows: names sort newest-first).
oldest="FFFFFFFF--0-63999"
case "$2" in
  */*/*/)
    printf '[2026-09-05 10:00:00 UTC] 1.2KiB STANDARD %s\n' "$tip"
    if [ -n "${LIVE_FIRST:-}" ] && [[ "$2" == *"/$oldest/" ]]; then
      printf '[2026-09-05 10:00:00 UTC] 1.2KiB STANDARD FFFFFFFF--%s.xdr.zst\n' "$LIVE_FIRST"
    fi ;;
  *)
    printf '[2026-09-05 10:00:00 UTC]     0B %s/\n' "$part"
    if [ -n "${LIVE_FIRST:-}" ] && [[ "$2" == local/galexie-live/ ]]; then
      printf '[2026-09-05 10:00:00 UTC]     0B %s/\n' "$oldest"
    fi ;;
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

unmeasured_archive() { ! has galexie_archive_tip_lag_ledgers && ! has galexie_archive_tip_ledger; }

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

# 3b. empty BY DESIGN (reset test net): archive lists nothing and live starts at
# ledger 2. Healthy, flagged, tip and lag absent (never a fabricated 0).
LIVE_FIRST=2 run ok empty
eq "empty by design: exit status" "$rc" 0
eq "empty by design: probe_success" "$(value galexie_archive_tip_lag_probe_success)" 1
eq "empty by design: flagged" "$(value galexie_archive_tip_lag_expected_empty)" 1
eq "empty by design: live tip" "$(value galexie_live_tip_ledger)" 150000
check "empty by design: no archive tip or lag" unmeasured_archive
check "empty by design: fresh success stamp" fresh "$(value galexie_archive_tip_lag_updated_seconds)"
check "empty by design: valid exposition" valid
LIVE_FIRST=64 run ok empty
eq "empty, live first at the bound: still healthy" "$rc" 0

# 3c. the same empty archive is a failure in every other state.
for c in "ok empty 65:empty archive, live starts above the bound" \
  "ok empty 4000000:empty archive, live starts high (range unfilled)" \
  "ok empty :empty archive, live first ledger unmeasurable" \
  "ok manifest 2:archive holds only a manifest" \
  "ok fail 2:archive unreadable, live starts low" \
  "fail empty 2:live unreadable, archive empty"; do
  rest="${c%%:*}" label="${c#*:}"
  # shellcheck disable=SC2086 # fields split on purpose
  set -- $rest
  LIVE_FIRST="${3:-}" run "$1" "$2"
  check "$label: exits non-zero" test "$rc" -ne 0
  eq "$label: probe_success" "$(value galexie_archive_tip_lag_probe_success)" 0
  eq "$label: not flagged empty" "$(value galexie_archive_tip_lag_expected_empty)" ""
done
# A non-empty archive is unchanged whatever the live start.
LIVE_FIRST=2 run ok ok
eq "normal archive, live starts low: lag published" "$(value galexie_archive_tip_lag_ledgers)" 22129
eq "normal archive: not flagged empty" "$(value galexie_archive_tip_lag_expected_empty)" ""

# 4. no temp file survives
eq "no temp file left behind" "$(find "$TMP/textfile" -name '*.tmp.*')" ""

# 5. the parser self-test
check "--self-test passes" bash "$PROBE" --self-test

echo "galexie-archive-tip-lag-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ]
