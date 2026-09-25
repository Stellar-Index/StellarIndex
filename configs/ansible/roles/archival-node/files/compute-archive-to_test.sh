#!/usr/bin/env bash
# compute-archive-to_test.sh — GH-1095: ARCHIVE_TO had no floor. A
# cursor reset or a partial reap-cursors leaving MAX(last_ledger) far
# below its true position silently shrank the verified range instead
# of failing. Pins that a regression against the previously-written
# ARCHIVE_TO now fails loudly instead of overwriting the env file.
#
# Uses a fake `psql` on PATH and overridable STELLARINDEX_ENV_FILE /
# ARCHIVE_COMPLETENESS_ENV_FILE (same override pattern as TEXTFILE_DIR
# in scripts/ops/config-assertions_test.sh) so this runs without root
# or a live Postgres.
#
# Run: bash configs/ansible/roles/archival-node/files/compute-archive-to_test.sh
set -uo pipefail

cd "$(dirname "$0")" || exit 1
GATE="$PWD/compute-archive-to.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

FAKEBIN="$TMP/bin"
mkdir -p "$FAKEBIN"
cat > "$FAKEBIN/psql" <<'EOF'
#!/usr/bin/env bash
echo "${FAKE_CURSOR_TO:-0}"
EOF
chmod +x "$FAKEBIN/psql"

echo "STELLARINDEX_POSTGRES_DSN=postgres://fake" > "$TMP/stellarindex.env"

pass=0
fail=0

# run <fake-cursor-to> — one invocation against the persistent env
# file left by the previous invocation (or none, on the first call).
run() {
  PATH="$FAKEBIN:$PATH" \
    STELLARINDEX_ENV_FILE="$TMP/stellarindex.env" \
    ARCHIVE_COMPLETENESS_ENV_FILE="$TMP/archive-completeness.env" \
    FAKE_CURSOR_TO="$1" \
    bash "$GATE" >"$TMP/stdout" 2>"$TMP/stderr"
  STATUS=$?
}

expect() {
  local name="$1" got="$2" want="$3"
  if [ "$got" != "$want" ]; then
    echo "FAIL: $name — got $got, want $want" >&2
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

# First run ever: no prior env file, nothing to floor against.
run 1000000
expect 'first run succeeds' "$STATUS" 0
expect 'first run writes ARCHIVE_TO' "$(cat "$TMP/archive-completeness.env")" "ARCHIVE_TO=1000000"

# Cursor advances normally: no regression, succeeds and advances.
run 2000000
expect 'advance succeeds' "$STATUS" 0
expect 'advance writes new ARCHIVE_TO' "$(cat "$TMP/archive-completeness.env")" "ARCHIVE_TO=2000000"

# GH-1095 reproduction: a reset/reaped cursor computes a TO far below
# the last verified checkpoint. Before the fix this silently
# overwrote the env file and the nightly run stamped success for a
# shrunk range. After the fix it must fail closed and leave the prior
# checkpoint in place.
run 1200000
expect 'regression is rejected (non-zero exit)' "$STATUS" 1
expect 'regression does not overwrite the prior checkpoint' "$(cat "$TMP/archive-completeness.env")" "ARCHIVE_TO=2000000"
if ! grep -q "regressed below the previous checkpoint" "$TMP/stderr"; then
  echo "FAIL: regression error message missing from stderr" >&2
  fail=$((fail + 1))
else
  echo "ok: regression error message present"
  pass=$((pass + 1))
fi

echo "---"
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
