#!/usr/bin/env bash
# config-backup-test.sh — fixture tests for config-backup.sh.
#
# The properties that matter: no plaintext backup is ever kept, a run that
# wrote nothing never stamps success, and an off-site push counts only when
# the object at the destination has the local size. age and mc are stubbed
# on PATH, so this runs anywhere in about a second.
#
# Run: bash scripts/ops/config-backup-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SCRIPT="$PWD/scripts/ops/config-backup.sh"
[[ -r "$SCRIPT" ]] || { echo "config-backup-test: missing $SCRIPT" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

mkdir -p "$TMP/bin" "$TMP/remote" "$TMP/src/etc/stellarindex"
echo "secret = 1" > "$TMP/src/etc/stellarindex/app.toml"

cat > "$TMP/bin/age" <<'STUB'
#!/usr/bin/env bash
[[ -n "${MOCK_AGE_FAIL:-}" ]] && exit 1
out=""
while [[ $# -gt 0 ]]; do
  if [[ "$1" == "-o" ]]; then out="$2"; shift 2; else shift; fi
done
{ echo "age-encryption.org/v1"; cat; } > "$out"
STUB
cat > "$TMP/bin/mc" <<'STUB'
#!/usr/bin/env bash
# alias list <name> | cp --quiet <src> <dst> | stat --json <dst>
remote="$MOCK_REMOTE"
case "$1" in
  alias) [[ -n "${MOCK_ALIAS_URL:-}" ]] && echo "  URL       : $MOCK_ALIAS_URL" ;;
  cp)    cp "$3" "$remote/$(basename "$4")" ;;
  stat)  f="$remote/$(basename "$3")"; [[ -e "$f" ]] || exit 1
         size=$(( $(wc -c < "$f") + ${MOCK_SIZE_SKEW:-0} ))
         echo "{\"status\":\"success\",\"size\":$size,\"type\":\"file\"}" ;;
esac
STUB
chmod +x "$TMP/bin/age" "$TMP/bin/mc"

# run_backup <case> [VAR=value ...] — one isolated invocation; prints the exit code.
run_backup() {
  local name="$1"; shift
  rm -rf "${TMP:?}/$name" "$TMP/remote"/*
  mkdir -p "$TMP/$name/out" "$TMP/$name/textfile"
  env PATH="$TMP/bin:$PATH" \
    AGE_RECIPIENT="age1test" \
    OUT_DIR="$TMP/$name/out" \
    TEXTFILE_DIR="$TMP/$name/textfile" \
    BACKUP_PATHS="$TMP/src/etc/stellarindex* $TMP/src/missing" \
    BACKUP_MC_TARGET="" \
    MOCK_REMOTE="$TMP/remote" \
    "$@" bash "$SCRIPT" >"$TMP/$name/stdout" 2>"$TMP/$name/stderr"
  echo "$?"
}
prom() { cat "$TMP/$1/textfile/config_backup.prom" 2>/dev/null; }
backups() { find "$TMP/$1/out" -type f 2>/dev/null; }

echo "config-backup-test:"

rc=$(run_backup local)
f=$(backups local)
if [[ "$rc" -eq 0 && -n "$f" && "$(head -n 1 "$f")" == "age-encryption.org/v1" ]]; then
  ok "a clean run writes one age-encrypted file"
else
  bad "clean run: rc=$rc file=$f"
fi
if grep -q '^stellarindex_config_backup_last_success_unix [0-9]' <<<"$(prom local)" \
   && grep -q '^stellarindex_config_backup_offsite_configured 0$' <<<"$(prom local)" \
   && ! grep -q 'offsite_last_success_unix [0-9]' <<<"$(prom local)"; then
  ok "a local-only run stamps success and reports no off-site target"
else
  bad "local-only metrics: $(prom local)"
fi
if [[ -n "$f" ]] && grep -q 'etc/stellarindex/app.toml' <<<"$(tail -n +2 "$f" | tar -tzf - 2>/dev/null)"; then
  ok "the archive holds the configured paths and skips missing ones"
else
  bad "archive contents"
fi

rc=$(run_backup norecipient AGE_RECIPIENT=)
if [[ "$rc" -eq 1 && -z "$(backups norecipient)" ]] && ! grep -q 'last_success_unix [0-9]' <<<"$(prom norecipient)"; then
  ok "no recipient: refuses, writes nothing, stamps nothing"
else
  bad "no recipient: rc=$rc"
fi

rc=$(run_backup agefail MOCK_AGE_FAIL=1)
if [[ "$rc" -eq 1 && -z "$(backups agefail)" ]] && ! grep -q 'last_success_unix [0-9]' <<<"$(prom agefail)"; then
  ok "age failure: no file, no .part, no success stamp"
else
  bad "age failure: rc=$rc files=$(backups agefail)"
fi

rc=$(run_backup nopaths BACKUP_PATHS="$TMP/src/missing")
if [[ "$rc" -eq 1 && -z "$(backups nopaths)" ]]; then
  ok "no existing path: refuses an empty backup"
else
  bad "no paths: rc=$rc"
fi

rc=$(run_backup offsite BACKUP_MC_TARGET=b2/config MOCK_ALIAS_URL=https://s3.example.com)
if [[ "$rc" -eq 0 ]] && grep -q '^stellarindex_config_backup_offsite_last_success_unix [0-9]' <<<"$(prom offsite)"; then
  ok "a verified push stamps the off-site metric"
else
  bad "offsite: rc=$rc $(cat "$TMP/offsite/stderr")"
fi

rc=$(run_backup skew BACKUP_MC_TARGET=b2/config MOCK_ALIAS_URL=https://s3.example.com MOCK_SIZE_SKEW=1)
if [[ "$rc" -eq 2 ]] && grep -q 'last_success_unix [0-9]' <<<"$(prom skew)" && ! grep -q 'offsite_last_success_unix' <<<"$(prom skew)"; then
  ok "a size mismatch at the destination is a failed push (rc 2)"
else
  bad "size skew: rc=$rc"
fi

for u in http://127.0.0.1:9000 ""; do
  rc=$(run_backup refuse BACKUP_MC_TARGET=b2/config MOCK_ALIAS_URL="$u")
  if [[ "$rc" -eq 2 && -z "$(ls "$TMP/remote")" ]] && ! grep -q 'offsite_last_success_unix' <<<"$(prom refuse)"; then
    ok "alias '${u:-unknown}' is refused, nothing pushed"
  else
    bad "alias '$u': rc=$rc"
  fi
done

mkdir -p "$TMP/retain/out"
touch -t 202001010000 "$TMP/retain/out/config-20200101T000000Z.tar.gz.age"
rm -rf "${TMP:?}/retain/textfile"
env PATH="$TMP/bin:$PATH" AGE_RECIPIENT=age1test OUT_DIR="$TMP/retain/out" \
  TEXTFILE_DIR=/dev/null BACKUP_PATHS="$TMP/src/etc/stellarindex" \
  bash "$SCRIPT" >/dev/null 2>&1
if [[ ! -e "$TMP/retain/out/config-20200101T000000Z.tar.gz.age" && "$(backups retain | wc -l | tr -d ' ')" -eq 1 ]]; then
  ok "backups older than RETAIN_DAYS are pruned"
else
  bad "retention: $(backups retain)"
fi

echo "config-backup-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
