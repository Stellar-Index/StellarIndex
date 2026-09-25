#!/usr/bin/env bash
# galexie-append_test.sh — CA2-A37-correct-1 / CA2-A37-harden-1: a failed
# `mc alias set` (MinIO reachable at boot, restarts mid-run) used to leave
# last_exported="" and fall through silently to the fresh-deploy fallback
# (GALEXIE_START or archive-tip-minus-margin), which can skip past ledgers
# this deploy already exported — a live-tier gap. The wrapper must instead
# exit non-zero so systemd's Restart=on-failure retries.
#
# Stubs mc/galexie/curl/jq via MC_BIN/GALEXIE_BIN (test-only overrides) and
# PATH so this runs without root, MinIO or a real galexie install.
#
# Run: bash configs/ansible/roles/archival-node/files/galexie-append_test.sh
set -uo pipefail

cd "$(dirname "$0")" || exit 1
SCRIPT="$PWD/galexie-append.sh"

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"

# jq/curl stubs for the fresh-deploy fallback path (only reached if the
# wrapper wrongly falls through past a failed alias set).
cat > "$TMP/bin/jq" <<'EOF'
#!/usr/bin/env bash
echo '999999'
EOF
cat > "$TMP/bin/curl" <<'EOF'
#!/usr/bin/env bash
echo '{"currentLedger":999999}'
EOF
chmod +x "$TMP/bin/jq" "$TMP/bin/curl"

# run <mc_alias_rc> <mc_ls_output> — invoke the real wrapper against a
# stub mc whose `alias set` exits mc_alias_rc and whose `ls` prints
# mc_ls_output, plus a stub galexie that just records its argv.
run() {
  local alias_rc="$1" ls_out="$2"
  cat > "$TMP/bin/mc" <<EOF
#!/usr/bin/env bash
case "\$1" in
  alias) exit $alias_rc ;;
  ls) printf '%s\n' '$ls_out' ;;
esac
EOF
  cat > "$TMP/bin/galexie" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" > "$GALEXIE_ARGV_FILE"
EOF
  chmod +x "$TMP/bin/mc" "$TMP/bin/galexie"
  rm -f "$TMP/argv"
  PATH="$TMP/bin:$PATH" \
    MC_BIN="$TMP/bin/mc" GALEXIE_BIN="$TMP/bin/galexie" \
    GALEXIE_ARGV_FILE="$TMP/argv" \
    AWS_ENDPOINT_URL="http://127.0.0.1:9000" \
    AWS_ACCESS_KEY_ID="fake" AWS_SECRET_ACCESS_KEY="fake" \
    GALEXIE_START=42 \
    bash "$SCRIPT" >"$TMP/stdout" 2>"$TMP/stderr"
  STATUS=$?
}

# 1. mc alias set fails (MinIO unreachable at probe time) — must exit
# non-zero and must NOT exec galexie via the fallback.
run 1 ""
if [ "$STATUS" -eq 0 ]; then
  bad "alias-set failure: wrapper exited 0 (should be fatal) — argv: $(cat "$TMP/argv" 2>/dev/null)"
else
  ok "alias-set failure: wrapper exits non-zero ($STATUS)"
fi
if [ -f "$TMP/argv" ]; then
  bad "alias-set failure: galexie was exec'd anyway (fell through to fallback) — argv: $(cat "$TMP/argv")"
else
  ok "alias-set failure: galexie was never exec'd"
fi
if grep -q "mc alias set failed" "$TMP/stderr"; then
  ok "alias-set failure: reported on stderr"
else
  bad "alias-set failure: no diagnostic on stderr: $(cat "$TMP/stderr")"
fi

# 2. mc alias set succeeds, bucket has an object — resumes from
# last_exported+1 (baseline behaviour must survive the fix untouched).
run 0 "live/galexie-live/chunk/FC43AFEC--62672915.xdr.zst"
if [ "$STATUS" -eq 0 ] && grep -q -- "--start 62672916" "$TMP/argv" 2>/dev/null; then
  ok "alias-set success + non-empty bucket: resumes at last_exported+1"
else
  bad "alias-set success + non-empty bucket: expected --start 62672916, got: $(cat "$TMP/argv" 2>/dev/null) (status=$STATUS)"
fi

echo
echo "galexie-append_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
