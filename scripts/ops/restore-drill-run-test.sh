#!/usr/bin/env bash
# restore-drill-run-test.sh — drives the REAL restore-drill.sh through its
# failure paths with shimmed host tools, root-free and second-fast.
#
# THE DEFECTS THIS EXISTS FOR (audit 2026-08-28, backup-restore-3/-4):
#
#  1. The drill's only capacity guard was a fixed MIN_FREE_GB=200 floor,
#     below the ~600 G database it restores onto the single shared pool.
#     A pool at 500 G free passed the check and the restore would have
#     filled the pool mid-copy (stalling live WAL + ClickHouse), then the
#     cleanup KEPT the partial datadir "for diagnosis". The floor is now
#     derived from the latest backup's size (pgbackrest info) with margin
#     and WAL headroom, and a partial restore is always removed.
#
#  2. The two most likely failure modes — `pgbackrest restore` failing and
#     the scratch instance never reaching consistency — `exit`ed before
#     the evidence phase: no drill-log entry, and the PREVIOUS run's
#     textfile (failures=0, fresh last_success) kept being scraped as if
#     the backup had just been proven restorable. Every run past the
#     preconditions now records evidence + rewrites the metric.
#
# Nothing else can see these: restore-drill-test.sh is static, and the
# script needs root + pgbackrest + a Postgres to run for real. So this
# test puts fake `id`, `sudo`, `df`, `chown`, `pgbackrest`, `psql` and a
# fake $PG_BIN on PATH and runs the actual script end to end until the
# staged failure. Only the seams the drill already exposes as env
# overrides are used (DRILL_ROOT, RESTORE_DRILL_LOG_DIR, TEXTFILE_DIR,
# PG_BIN); nothing in the script is test-only.
#
# Run: bash scripts/ops/restore-drill-run-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
DRILL="$(pwd)/scripts/ops/restore-drill.sh"
[[ -r "$DRILL" ]] || { echo "restore-drill-run-test: missing $DRILL" >&2; exit 2; }
command -v jq >/dev/null || { echo "restore-drill-run-test: jq required" >&2; exit 2; }

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

work="$(mktemp -d "${TMPDIR:-/tmp}/restore-drill-run-test.XXXXXX")"
trap 'rm -rf "$work"' EXIT
shims="$work/shims"; pgbin="$work/pgbin"
mkdir -p "$shims" "$pgbin"

# ─── host-tool shims ────────────────────────────────────────────────
# Each reads its behaviour from FAKE_* env vars set per case.
cat > "$shims/id" <<'SH'
#!/usr/bin/env bash
[[ "${1:-}" == "-u" ]] && { echo 0; exit 0; }
exec /usr/bin/id "$@"
SH
cat > "$shims/sudo" <<'SH'
#!/usr/bin/env bash
# drop `-u <user>` (and any other option) and exec the command as-is
while [[ $# -gt 0 ]]; do
  case "$1" in -u) shift 2 ;; --) shift; break ;; -*) shift ;; *) break ;; esac
done
exec "$@"
SH
cat > "$shims/df" <<'SH'
#!/usr/bin/env bash
printf 'Avail\n%sG\n' "${FAKE_DF_AVAIL_G:?}"
SH
cat > "$shims/chown" <<'SH'
#!/usr/bin/env bash
exit 0
SH
cat > "$shims/pgbackrest" <<'SH'
#!/usr/bin/env bash
case "${*: -1}" in
  info)
    printf '[{"name":"%s","backup":[{"label":"old","info":{"size":1}},{"label":"latest","info":{"size":%s}}]}]\n' \
      "${FAKE_STANZA:-stellarindex}" "${FAKE_BACKUP_BYTES:?}" ;;
  restore)
    # the drill's real restore populates --pg1-path; mimic a partial copy
    for a in "$@"; do [[ "$a" == --pg1-path=* ]] && echo partial > "${a#--pg1-path=}/PG_VERSION"; done
    echo "pgbackrest: (fake) restore rc=${FAKE_RESTORE_RC:-0}" >&2
    exit "${FAKE_RESTORE_RC:-0}" ;;
  *) echo "fake pgbackrest: unexpected command: $*" >&2; exit 99 ;;
esac
SH
cat > "$shims/psql" <<'SH'
#!/usr/bin/env bash
exit 0
SH
cat > "$pgbin/postgres" <<'SH'
#!/usr/bin/env bash
exit 0
SH
cat > "$pgbin/pg_ctl" <<'SH'
#!/usr/bin/env bash
case " $* " in
  *" start "*) exit "${FAKE_PG_START_RC:-0}" ;;
  *) exit 0 ;;
esac
SH
chmod +x "$shims"/* "$pgbin"/*

# run_drill <case-dir>: runs the drill with the shims; stdout+stderr in
# $case/out, exit code in $case/rc. Callers set FAKE_* first.
run_drill() {
  local d="$1"
  mkdir -p "$d/root" "$d/log" "$d/textfile"
  PATH="$shims:$PATH" PG_BIN="$pgbin" \
    DRILL_ROOT="$d/root" RESTORE_DRILL_LOG_DIR="$d/log" TEXTFILE_DIR="$d/textfile" \
    DRILL_CH_WINDOW='' STELLARINDEX_POSTGRES_DSN='' \
    bash "$DRILL" >"$d/out" 2>&1
  echo $? > "$d/rc"
}

# seed_stale_pass <case-dir>: the previous month's PASS, as node_exporter
# would still be serving it.
seed_stale_pass() {
  mkdir -p "$1/textfile"
  printf 'stellarindex_restore_drill_last_success_unix 1000\nstellarindex_restore_drill_failures 0\n' \
    > "$1/textfile/restore_drill.prom"
}

# ─── 1. capacity floor is derived from the backup, not the constant ──
# 500 G free is above the old MIN_FREE_GB=200 but below what a 600 GiB
# backup needs (600 × 125 % + 50 = 800 G). The drill must REFUSE (exit 2,
# an uncounted precondition) before writing a byte, and must not leave
# evidence or a metric (a refusal is not a drill).
c="$work/c1"; mkdir -p "$c"
FAKE_DF_AVAIL_G=500 FAKE_BACKUP_BYTES=$((600 * 1073741824)) FAKE_RESTORE_RC=1 run_drill "$c"
rc="$(cat "$c/rc")"
if [[ "$rc" == "2" ]] && grep -q "need 800G" "$c/out" && grep -q "refusing" "$c/out"; then
  ok "500G free vs 600G backup: precondition refusal (exit 2, 'need 800G')"
else
  bad "500G free vs 600G backup: expected exit 2 + 'need 800G … refusing', got exit $rc:
$(tail -n 5 "$c/out")"
fi
if [[ ! -e "$c/log/restore-drills.md" && ! -e "$c/textfile/restore_drill.prom" ]]; then
  ok "a capacity refusal writes neither evidence nor metric"
else
  bad "a capacity refusal wrote evidence/metric — refusals must not share the drill's signal"
fi
if [[ -z "$(ls -A "$c/root" 2>/dev/null)" ]]; then
  ok "a capacity refusal creates no datadir"
else
  bad "a capacity refusal left something under DRILL_ROOT: $(ls "$c/root")"
fi

# Same free space, a 300 GiB backup (needs 300 × 125 % + 50 = 425 G):
# must proceed past the precondition (restore then fails by design).
c="$work/c1b"; mkdir -p "$c"
FAKE_DF_AVAIL_G=500 FAKE_BACKUP_BYTES=$((300 * 1073741824)) FAKE_RESTORE_RC=1 run_drill "$c"
if [[ "$(cat "$c/rc")" != "2" ]] && grep -q "capacity: 500G free" "$c/out"; then
  ok "500G free vs 300G backup: proceeds (need 425G) — the floor tracks the backup"
else
  bad "500G free vs 300G backup: expected to proceed, got exit $(cat "$c/rc"):
$(tail -n 5 "$c/out")"
fi

# ─── 2. pgbackrest restore fails: evidence + metric + partial removed ──
c="$work/c2"; mkdir -p "$c"; seed_stale_pass "$c"
FAKE_DF_AVAIL_G=5000 FAKE_BACKUP_BYTES=$((300 * 1073741824)) FAKE_RESTORE_RC=1 run_drill "$c"
rc="$(cat "$c/rc")"
if [[ "$rc" == "1" ]]; then
  ok "restore failure exits with the failure count (1)"
else
  bad "restore failure: expected exit 1, got $rc: $(tail -n 3 "$c/out")"
fi
if [[ -f "$c/log/restore-drills.md" ]] && grep -q "ABORTED at pg_restore" "$c/log/restore-drills.md" \
     && grep -q "^- failures: 1$" "$c/log/restore-drills.md"; then
  ok "restore failure appends an evidence entry ('ABORTED at pg_restore', failures: 1)"
else
  bad "restore failure left NO evidence entry (the drill's only deliverable):
$(cat "$c/log/restore-drills.md" 2>/dev/null || echo '<no file>')"
fi
prom="$c/textfile/restore_drill.prom"
if grep -q "^stellarindex_restore_drill_failures 1$" "$prom" \
     && ! grep -q "^stellarindex_restore_drill_last_success_unix" "$prom"; then
  ok "restore failure rewrites the textfile: failures=1, NO last_success"
else
  bad "restore failure left the previous PASS being scraped:
$(cat "$prom")"
fi
if [[ -z "$(ls -A "$c/root" 2>/dev/null)" ]]; then
  ok "a partial restore is removed, not kept 'for diagnosis'"
else
  bad "a partial restore was left on the pool: $(ls "$c/root")"
fi

# ─── 3. scratch instance never reaches consistency ──────────────────
# Restore succeeds, pg_ctl start fails: evidence + metric as above, but
# the datadir IS the diagnostic here and must be kept.
c="$work/c3"; mkdir -p "$c"; seed_stale_pass "$c"
FAKE_DF_AVAIL_G=5000 FAKE_BACKUP_BYTES=$((300 * 1073741824)) FAKE_RESTORE_RC=0 FAKE_PG_START_RC=1 run_drill "$c"
rc="$(cat "$c/rc")"
if [[ "$rc" == "1" ]]; then
  ok "pg_start failure exits with the failure count (1)"
else
  bad "pg_start failure: expected exit 1, got $rc: $(tail -n 3 "$c/out")"
fi
if grep -q "ABORTED at pg_start" "$c/log/restore-drills.md" 2>/dev/null; then
  ok "pg_start failure appends an evidence entry ('ABORTED at pg_start')"
else
  bad "pg_start failure left NO evidence entry"
fi
prom="$c/textfile/restore_drill.prom"
if grep -q "^stellarindex_restore_drill_failures 1$" "$prom" \
     && ! grep -q "^stellarindex_restore_drill_last_success_unix" "$prom"; then
  ok "pg_start failure rewrites the textfile: failures=1, NO last_success"
else
  bad "pg_start failure left the previous PASS being scraped:
$(cat "$prom")"
fi
if ls -d "$c/root"/pgdata-* >/dev/null 2>&1; then
  ok "a post-restore failure keeps the datadir (it is the evidence)"
else
  bad "a post-restore failure removed the datadir that would have been the diagnostic"
fi

echo "restore-drill-run-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]] || exit 1
