#!/usr/bin/env bash
# ordinal-rederive-chunks_test.sh — pins T245: the logged failure rc must
# be the real exit code of the fake stellarindex-ops invocation, not the
# status of the negated `if ! cmd` test (which is always 0 on entry into
# the then-branch and would print "rc=0" for every failure).
#
# Uses a fake `stellarindex-ops` on PATH via OPS override, one chunk wide
# (START==BAND_END-CHUNK not needed; BAND_END=START+CHUNK forces exactly
# one iteration) so the script exits after the first failing chunk.
#
# Run: bash scripts/ops/ordinal-rederive-chunks_test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SCRIPT="$PWD/scripts/ops/ordinal-rederive-chunks.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

FAKE_OPS="$TMP/fake-ops"
cat > "$FAKE_OPS" <<'EOF'
#!/usr/bin/env bash
exit 7
EOF
chmod +x "$FAKE_OPS"

OUT="$(OPS="$FAKE_OPS" CONFIG_PATH="$TMP/none.toml" START=63000000 \
  BAND_END=63000001 CHUNK=110000 bash "$SCRIPT" 2>&1)"
STATUS=$?

pass=0
fail=0

if [ "$STATUS" -eq 1 ]; then
  echo "PASS: script exit status is 1"
  pass=$((pass + 1))
else
  echo "FAIL: script exit status is $STATUS, want 1"
  fail=$((fail + 1))
fi

if grep -q 'FAILED rc=7' <<<"$OUT"; then
  echo "PASS: logged the real exit code (rc=7)"
  pass=$((pass + 1))
else
  echo "FAIL: expected 'FAILED rc=7' in output, got:"
  echo "$OUT"
  fail=$((fail + 1))
fi

echo "== $pass passed, $fail failed =="
[ "$fail" -eq 0 ]
