#!/usr/bin/env bash
# galexie-archive-mirror-test.sh — fixture tests for the Galexie archive
# off-site mirror (scripts/ops/galexie-archive-mirror.sh).
#
# The properties that matter:
#   1. no DEST_ENDPOINT configured is reported (configured 0), exit 0, no
#      success stamp.
#   2. a clean mirror + clean dry-run verification stamps success.
#   3. the real `mc mirror` failing does not stamp success.
#   4. the post-mirror `mc mirror --dry-run` verification is judged on its
#      EXIT STATUS, not just its stdout: a dry-run that exits non-zero with
#      EMPTY stdout (the auth/network-failure shape) must NOT be read as
#      "nothing left to copy" — that is the bug this test pins.
#   5. a dry-run that exits 0 but still lists outstanding objects is also
#      not a success.
#
# `mc` is a stub on PATH. Run: bash scripts/ops/galexie-archive-mirror-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SCRIPT="$PWD/scripts/ops/galexie-archive-mirror.sh"
[[ -r "$SCRIPT" ]] || { echo "galexie-archive-mirror-test: missing $SCRIPT" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }
expect_rc() { if [[ "$rc" -eq "$1" ]]; then ok "$2"; else bad "$2 (rc=$rc)"; fi; }
PROM="$TMP/tf/galexie_archive_mirror.prom"
prom_has() { if grep -qx -- "$1" "$PROM" 2>/dev/null; then ok "$2"; else bad "$2"; fi; }
prom_stamped() { if grep -q '^stellarindex_galexie_archive_mirror_last_success_timestamp [0-9]' "$PROM" 2>/dev/null; then ok "$1"; else bad "$1"; fi; }
prom_unstamped() { if grep -q last_success_timestamp "$PROM" 2>/dev/null; then bad "$1"; else ok "$1"; fi; }

mkdir -p "$TMP/bin"
cat > "$TMP/bin/mc" <<'STUB'
#!/usr/bin/env bash
if [[ "$1" == "mirror" && "$2" == "--overwrite" ]]; then
  exit "${MOCK_MIRROR_RC:-0}"
elif [[ "$1" == "mirror" && "$2" == "--dry-run" ]]; then
  if [[ -n "${MOCK_DRYRUN_OUT:-}" ]]; then echo "$MOCK_DRYRUN_OUT"; fi
  exit "${MOCK_DRYRUN_RC:-0}"
fi
echo "galexie-archive-mirror-test: unexpected mc invocation: $*" >&2
exit 9
STUB
chmod +x "$TMP/bin/mc"
export PATH="$TMP/bin:$PATH"

run() {
  rm -rf "$TMP/tf"
  MC_CMD=mc TEXTFILE_DIR="$TMP/tf" \
    "$SCRIPT" >"$TMP/out" 2>"$TMP/err"
  rc=$?
}

echo "== not configured =="
DEST_ENDPOINT="" run
expect_rc 0 "not-configured run exits 0"
prom_has 'stellarindex_galexie_archive_mirror_configured 0' "reports configured=0"
prom_unstamped "does not stamp success when unconfigured"

echo "== clean mirror + clean dry-run verification =="
DEST_ENDPOINT="https://acct.r2.cloudflarestorage.com/bucket/" MOCK_MIRROR_RC=0 MOCK_DRYRUN_RC=0 MOCK_DRYRUN_OUT="" run
expect_rc 0 "clean run exits 0"
prom_has 'stellarindex_galexie_archive_mirror_configured 1' "reports configured=1"
prom_has 'stellarindex_galexie_archive_mirror_last_run_ok 1' "reports last_run_ok=1"
prom_stamped "stamps success timestamp"

echo "== real mirror fails =="
DEST_ENDPOINT="https://acct.r2.cloudflarestorage.com/bucket/" MOCK_MIRROR_RC=1 run
expect_rc 1 "failed mirror exits 1"
prom_has 'stellarindex_galexie_archive_mirror_last_run_ok 0' "reports last_run_ok=0 on mirror failure"
prom_unstamped "does not stamp success on mirror failure"

echo "== dry-run verification fails with EMPTY stdout (the pinned bug) =="
DEST_ENDPOINT="https://acct.r2.cloudflarestorage.com/bucket/" MOCK_MIRROR_RC=0 MOCK_DRYRUN_RC=1 MOCK_DRYRUN_OUT="" run
expect_rc 1 "non-zero dry-run with empty stdout must fail the run"
prom_has 'stellarindex_galexie_archive_mirror_last_run_ok 0' "reports last_run_ok=0 on a failed empty-stdout dry-run"
prom_unstamped "does not stamp success on a failed empty-stdout dry-run"

echo "== dry-run exits 0 but still lists outstanding objects =="
DEST_ENDPOINT="https://acct.r2.cloudflarestorage.com/bucket/" MOCK_MIRROR_RC=0 MOCK_DRYRUN_RC=0 MOCK_DRYRUN_OUT="galexie-archive/ledgers/00001.xdr.zstd" run
expect_rc 1 "dry-run listing outstanding objects must fail the run"
prom_unstamped "does not stamp success while objects remain outstanding"

echo
echo "$pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
