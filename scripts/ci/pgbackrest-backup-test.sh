#!/usr/bin/env bash
# pgbackrest-backup-test.sh — fixture tests for the nightly pgBackRest
# wrapper the archival-node role installs as
# /usr/local/bin/pgbackrest-backup.sh (2026-08-29, repo2 = S3 live on r1).
#
# The wrapper is the role template
# configs/ansible/roles/archival-node/templates/pgbackrest-backup.sh.j2.
# It carries no Jinja (asserted below) so the shipped bytes are what is
# tested — not a hand-copied twin.
#
# What must hold — pgBackRest's `backup` is SINGLE-repo (User Guide,
# "Multiple Repositories": with no --repo it backs up only the
# highest-priority repo, repo1; only archive-push/expire fan out), and
# the wrapper shipped for two months with no --repo, so repo2 got WAL
# but never a base backup:
#
#   1. two repos in pgbackrest.conf → exactly two backups, --repo=1
#      then --repo=2, same --type, rc 0, per-repo metrics rc=0;
#   2. repo1 fails → repo2 STILL runs, wrapper exits with repo1's rc,
#      repo1's last_success is carried forward from the previous run
#      (not dropped), repo2's is fresh;
#   3. single-repo conf → the command is BYTE-IDENTICAL to the
#      pre-2026-08-29 wrapper (no --repo);
#   4. Sunday → --type=full; any other day → --type=diff;
#   5. no secret from pgbackrest.conf reaches argv or stdout/stderr.
#
# pgbackrest and date are stubbed on PATH (runs on macOS, no pgbackrest).
#
# Run: bash scripts/ci/pgbackrest-backup-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SRC="$PWD/configs/ansible/roles/archival-node/templates/pgbackrest-backup.sh.j2"
[[ -r "$SRC" ]] || { echo "pgbackrest-backup-test: missing $SRC" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

if grep -q '{{\|{%' "$SRC"; then
  echo "pgbackrest-backup-test: template carries Jinja — this test runs the raw file" >&2
  exit 2
fi
WRAP="$TMP/pgbackrest-backup.sh"
cp "$SRC" "$WRAP"
chmod +x "$WRAP"

# ─── stubs ──────────────────────────────────────────────────────────
mkdir -p "$TMP/bin" "$TMP/textfile"
REAL_DATE="$(command -v date)"
# pgbackrest stub: appends its argv to $PGBR_LOG; exits non-zero for
# any --repo=N listed in $PGBR_FAIL_REPOS (space-separated), or for
# every call when PGBR_FAIL_ALL=1 (the single-repo shape has no --repo).
cat > "$TMP/bin/pgbackrest" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$PGBR_LOG"
for a in "$@"; do
  case "$a" in --repo=*) r="${a#--repo=}";;
  esac
done
[[ "${PGBR_FAIL_ALL:-0}" == 1 ]] && exit 43
for f in ${PGBR_FAIL_REPOS:-}; do [[ "${r:-}" == "$f" ]] && exit 42; done
exit 0
SH
# date stub: FAKE_DOW answers `date +%u`; everything else goes to the
# real date, so timestamps stay real.
cat > "$TMP/bin/date" <<SH
#!/usr/bin/env bash
if [[ "\$1" == "+%u" ]]; then echo "\${FAKE_DOW:-3}"; exit 0; fi
exec "$REAL_DATE" "\$@"
SH
chmod +x "$TMP/bin/pgbackrest" "$TMP/bin/date"
export PATH="$TMP/bin:$PATH"
export TEXTFILE_DIR="$TMP/textfile"
PROM="$TEXTFILE_DIR/pgbackrest_backup.prom"

SECRET_S3="AKIA-STUB-KEY-SECRET-9f8e7d"  # gitleaks:allow — fixture, not a credential
SECRET_CIPHER="cipher-pass-STUB-1a2b3c"  # gitleaks:allow — fixture, not a credential
TWO_REPO="$TMP/two.conf"
cat > "$TWO_REPO" <<EOT
# rendered by ansible
[global]
repo1-path=/var/lib/pgbackrest
repo1-retention-full=2
repo1-bundle=y
repo2-type=s3
repo2-s3-endpoint=s3.example
repo2-s3-bucket=stellarindex-pgbackup
repo2-s3-key=stub-key
repo2-s3-key-secret=$SECRET_S3
repo2-path=/pgbackrest
repo2-retention-full=1
repo2-cipher-type=aes-256-cbc
repo2-cipher-pass=$SECRET_CIPHER
compress-type=zst
[stellarindex]
pg1-path=/var/lib/postgresql/15/main
EOT
ONE_REPO="$TMP/one.conf"
cat > "$ONE_REPO" <<EOT
[global]
repo1-path=/var/lib/pgbackrest
repo1-retention-full=2
[stellarindex]
pg1-path=/var/lib/postgresql/15/main
EOT

run() { # run <conf> <dow> [env=val ...] → stdout+stderr in $OUT, rc in $RC, argv log in $LOG
  local conf="$1" dow="$2"; shift 2
  LOG="$TMP/argv.$RANDOM.log"; : > "$LOG"
  OUT="$(env PGBACKREST_CONF="$conf" FAKE_DOW="$dow" PGBR_LOG="$LOG" "$@" "$WRAP" 2>&1)"
  RC=$?
}
metric() { grep -E "^$1\{repo=\"$2\"\} " "$PROM" | awk '{ print $2 }'; }

# ─── 1. two repos, both succeed ─────────────────────────────────────
echo "pgbackrest-backup-test: two repos, both succeed"
rm -f "$PROM"
run "$TWO_REPO" 3
if [[ $RC -eq 0 ]]; then ok "rc 0"; else bad "rc $RC (want 0): $OUT"; fi
if [[ "$(wc -l < "$LOG" | tr -d ' ')" == 2 ]]; then ok "exactly two pgbackrest invocations"; else bad "invocations: $(cat "$LOG")"; fi
if [[ "$(sed -n 1p "$LOG")" == "--stanza=stellarindex --repo=1 --type=diff backup" ]]; then ok "repo1 first, --repo=1 --type=diff"; else bad "first argv: $(sed -n 1p "$LOG")"; fi
if [[ "$(sed -n 2p "$LOG")" == "--stanza=stellarindex --repo=2 --type=diff backup" ]]; then ok "repo2 second, --repo=2 --type=diff"; else bad "second argv: $(sed -n 2p "$LOG")"; fi
if [[ "$(metric stellarindex_pgbackrest_backup_last_rc 1)" == 0 && "$(metric stellarindex_pgbackrest_backup_last_rc 2)" == 0 ]]; then ok "last_rc{repo=1,2} = 0"; else bad "last_rc: $(cat "$PROM" 2>&1)"; fi
T1="$(metric stellarindex_pgbackrest_backup_last_success_unix 1)"; T2="$(metric stellarindex_pgbackrest_backup_last_success_unix 2)"
NOW="$("$REAL_DATE" +%s)"
if [[ -n "$T1" && -n "$T2" && $((NOW - T1)) -lt 60 && $((NOW - T2)) -lt 60 ]]; then ok "last_success_unix{repo=1,2} fresh"; else bad "last_success: T1=$T1 T2=$T2 now=$NOW"; fi
if [[ -n "$(metric stellarindex_pgbackrest_backup_duration_seconds 2)" ]]; then ok "duration_seconds{repo=2} present"; else bad "duration missing: $(cat "$PROM")"; fi
if [[ "$(stat -f '%Lp' "$PROM" 2>/dev/null || stat -c '%a' "$PROM")" == 644 ]]; then ok "textfile is 0644"; else bad "textfile mode"; fi

# ─── 2. repo1 fails → repo2 still runs, rc non-zero, carry-forward ──
echo "pgbackrest-backup-test: repo1 fails, repo2 still runs"
run "$TWO_REPO" 3 PGBR_FAIL_REPOS=1
if [[ $RC -eq 42 ]]; then ok "exit rc = repo1's pgbackrest rc (42)"; else bad "rc $RC (want 42): $OUT"; fi
if [[ "$(wc -l < "$LOG" | tr -d ' ')" == 2 ]]; then ok "repo2 still attempted after repo1 failure"; else bad "invocations: $(cat "$LOG")"; fi
if grep -q -- "--repo=2" "$LOG"; then ok "--repo=2 invoked"; else bad "no --repo=2 call"; fi
if [[ "$(metric stellarindex_pgbackrest_backup_last_rc 1)" == 42 && "$(metric stellarindex_pgbackrest_backup_last_rc 2)" == 0 ]]; then ok "last_rc{repo=1}=42, {repo=2}=0"; else bad "last_rc: $(cat "$PROM")"; fi
if [[ -n "$T1" && "$(metric stellarindex_pgbackrest_backup_last_success_unix 1)" == "$T1" ]]; then ok "repo1 last_success carried forward from previous run"; else bad "repo1 last_success $(metric stellarindex_pgbackrest_backup_last_success_unix 1) != $T1"; fi
if [[ -n "$T2" && "$(metric stellarindex_pgbackrest_backup_last_success_unix 2 || echo 0)" -ge "$T2" ]]; then ok "repo2 last_success refreshed"; else bad "repo2 last_success not refreshed"; fi
if [[ "$OUT" == *"repo1"*"FAILED"* ]]; then ok "failure named per repo in log"; else bad "log: $OUT"; fi

# ─── 3. single-repo conf → byte-identical legacy command ────────────
echo "pgbackrest-backup-test: single-repo conf keeps the legacy command"
run "$ONE_REPO" 3
if [[ $RC -eq 0 ]]; then ok "rc 0"; else bad "rc $RC"; fi
if [[ "$(cat "$LOG")" == "--stanza=stellarindex --type=diff backup" ]]; then ok "argv byte-identical (no --repo)"; else bad "argv: $(cat "$LOG")"; fi
run "$ONE_REPO" 3 PGBR_FAIL_ALL=1
if [[ $RC -eq 43 ]]; then ok "single-repo failure propagates rc 43"; else bad "rc $RC (want 43)"; fi
run "$TMP/does-not-exist.conf" 3
if [[ "$(cat "$LOG")" == "--stanza=stellarindex --type=diff backup" ]]; then ok "unreadable conf falls back to the legacy single command"; else bad "argv: $(cat "$LOG")"; fi

# ─── 4. weekly full on Sunday, diff otherwise ───────────────────────
echo "pgbackrest-backup-test: type selection"
run "$TWO_REPO" 7
if [[ "$(grep -c -- '--type=full' "$LOG")" == 2 ]]; then ok "Sunday → --type=full on both repos"; else bad "Sunday argv: $(cat "$LOG")"; fi
run "$TWO_REPO" 6
if [[ "$(grep -c -- '--type=diff' "$LOG")" == 2 && ! "$(cat "$LOG")" == *full* ]]; then ok "Saturday → --type=diff on both repos"; else bad "Saturday argv: $(cat "$LOG")"; fi

# ─── 5. no secrets on argv / in output ──────────────────────────────
echo "pgbackrest-backup-test: no secrets leak"
run "$TWO_REPO" 3 PGBR_FAIL_REPOS="1 2"
if grep -qF -e "$SECRET_S3" -e "$SECRET_CIPHER" "$LOG" || [[ "$OUT" == *"$SECRET_S3"* || "$OUT" == *"$SECRET_CIPHER"* ]] || grep -qF -e "$SECRET_S3" -e "$SECRET_CIPHER" "$PROM"; then
  bad "secret material reached argv/log/metrics"
else
  ok "no secret material on argv, in output, or in the textfile"
fi
if [[ $RC -eq 42 ]]; then ok "both fail → rc non-zero"; else bad "rc $RC"; fi

printf 'pgbackrest-backup-test: %d passed, %d failed\n' "$pass" "$fail"
[[ $fail -eq 0 ]]
