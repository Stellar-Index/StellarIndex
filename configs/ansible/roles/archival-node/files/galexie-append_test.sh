#!/usr/bin/env bash
# galexie-append_test.sh — a failed
# `mc alias set` (MinIO reachable at boot, restarts mid-run) would leave
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

# run <mc_alias_rc> <bucket_ls> [partition_ls] [ls_rc] — invoke the real
# wrapper against a stub mc whose `alias set` exits mc_alias_rc, whose bucket
# `ls` prints bucket_ls and whose partition `ls` prints partition_ls (both exit
# ls_rc), plus a stub galexie that just records its argv.
run() {
  local alias_rc="$1" bucket_ls="$2" part_ls="${3:-}" ls_rc="${4:-0}"
  printf '%b\n' "$bucket_ls" > "$TMP/bucket_ls"
  printf '%b\n' "$part_ls" > "$TMP/part_ls"
  cat > "$TMP/bin/mc" <<EOF
#!/usr/bin/env bash
case "\$1" in
  alias) exit $alias_rc ;;
  ls)
    case "\$2" in
      live/galexie-live/) cat "$TMP/bucket_ls" ;;
      *) echo "\$2" >> "$TMP/part_args"; cat "$TMP/part_ls" ;;
    esac
    exit $ls_rc ;;
esac
EOF
  cat > "$TMP/bin/galexie" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" > "$GALEXIE_ARGV_FILE"
EOF
  chmod +x "$TMP/bin/mc" "$TMP/bin/galexie"
  rm -f "$TMP/argv" "$TMP/part_args"
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

# 2. Healthy restart: only the partition with the highest range end is
# listed, whatever order mc returns them in, and the resume is its max+1.
run 0 "[2026-10-16] 0B FC21BDFF--64896000-64959999/\n[2026-10-16] 0B FC20C9FF--64960000-65023999/" \
  "[2026-10-16] 1KiB FC207B7F--64980096.xdr.zst\n[2026-10-16] 1KiB FC207B7A--64980101.xdr.zst"
if [ "$STATUS" -eq 0 ] && grep -q -- "--start 64980102" "$TMP/argv" 2>/dev/null; then
  ok "non-empty bucket: resumes at the newest partition's max+1"
else
  bad "non-empty bucket: expected --start 64980102, got: $(cat "$TMP/argv" 2>/dev/null) (status=$STATUS)"
fi
if [ "$(cat "$TMP/part_args" 2>/dev/null)" = "live/galexie-live/FC20C9FF--64960000-65023999/" ]; then
  ok "non-empty bucket: lists only the newest partition"
else
  bad "non-empty bucket: listed partitions: $(cat "$TMP/part_args" 2>/dev/null)"
fi

# 3. ledgers_per_file > 1: a range object name contributes its end ledger.
run 0 "[2026-10-16] 0B FC20C9FF--64960000-65023999/" \
  "[2026-10-16] 9KiB FC207B3F--64980160-64980223.xdr.zstd"
if [ "$STATUS" -eq 0 ] && grep -q -- "--start 64980224" "$TMP/argv" 2>/dev/null; then
  ok "range object names: resumes at the range end+1"
else
  bad "range object names: expected --start 64980224, got: $(cat "$TMP/argv" 2>/dev/null) (status=$STATUS)"
fi

# 4. A failed bucket listing must be fatal, never the fresh-deploy fallback.
run 0 "" "" 1
if [ "$STATUS" -ne 0 ] && [ ! -f "$TMP/argv" ]; then
  ok "listing failure: wrapper exits non-zero without exec'ing galexie"
else
  bad "listing failure: status=$STATUS argv: $(cat "$TMP/argv" 2>/dev/null)"
fi

echo
echo "galexie-append_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
