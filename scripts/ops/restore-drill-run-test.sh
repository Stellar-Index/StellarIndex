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
#  3. (2026-09-04, restore-drill-offsite.timer) Two timers now drive the
#     script — repo1 and repo2 — and each run rewrote ONE textfile whole,
#     so a clean repo2 run erased a failed repo1 verdict and no series
#     said which copy it proved. The metric is now per-repo (one file per
#     repo, a `repo` label on every series); case 4 drives a repo2 abort
#     and checks repo1's file is untouched.
#
#  4. (2026-09-04) The one-drill-at-a-time lock was entirely untested —
#     the `flock` shim returned 0 unconditionally — and `exec 9>$LOCK`
#     under `set -e` exited 1, this script's code for ONE FAILED CHECK,
#     so an unwritable lock path would have been recorded as the backup
#     failing a check. Cases 6 and 7 drive a HELD lock and an UNOPENABLE
#     lock file; case 8 drives a postgres left on the scratch port by a
#     killed run, which no lock covers because the lock dies with the
#     shell that held it.
#
# Nothing else can see these: restore-drill-test.sh is static, and the
# script needs root + pgbackrest + a Postgres to run for real. So this
# test puts fake `id`, `sudo`, `df`, `chown`, `pgbackrest`, `psql`,
# `flock` and a fake $PG_BIN on PATH and runs the actual script end to
# end until the staged failure. Only the seams the drill already exposes
# as env overrides are used (DRILL_ROOT, DRILL_LOCK, DRILL_REPO,
# RESTORE_DRILL_LOG_DIR, TEXTFILE_DIR, PG_BIN); nothing in the script is
# test-only.
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
# util-linux flock is absent on macOS, so the lock needs a shim — and an
# unconditional `exit 0` would leave the lock entirely untested, which is
# how its refusal path stayed unexercised. FLOCK_RESULT drives it: 0 (the
# default) is "this run took the lock", 1 is "another drill holds it".
cat > "$shims/flock" <<'SH'
#!/usr/bin/env bash
exit "${FLOCK_RESULT:-0}"
SH
# The scratch-port pre-flight probes $PG_BIN/pg_isready. 2 = no response,
# i.e. the port is free, which is what every other case wants.
cat > "$pgbin/pg_isready" <<'SH'
#!/usr/bin/env bash
exit "${FAKE_PG_ISREADY_RC:-2}"
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
# WAL_DRAIN_TIMEOUT=0: no staged case is meant to reach the verification
# phase, but a regression that lets one through (a precondition that
# stops refusing) must then FAIL its assertion in seconds, not sit in the
# archive-drain loop for its default hour against a psql shim that never
# answers.
run_drill() {
  local d="$1"
  mkdir -p "$d/root" "$d/log" "$d/textfile"
  PATH="$shims:$PATH" PG_BIN="$pgbin" \
    DRILL_ROOT="$d/root" DRILL_LOCK="${FAKE_DRILL_LOCK:-$d/lock}" RESTORE_DRILL_LOG_DIR="$d/log" TEXTFILE_DIR="$d/textfile" \
    DRILL_CH_WINDOW='' STELLARINDEX_POSTGRES_DSN='' WAL_DRAIN_TIMEOUT=0 \
    bash "$DRILL" >"$d/out" 2>&1
  echo $? > "$d/rc"
}

# seed_stale_pass <case-dir>: the previous month's repo1 PASS, as
# node_exporter would still be serving it.
seed_stale_pass() {
  mkdir -p "$1/textfile"
  printf 'stellarindex_restore_drill_last_success_unix{repo="1"} 1000\nstellarindex_restore_drill_failures{repo="1"} 0\n' \
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
if grep -q '^stellarindex_restore_drill_failures{repo="1"} 1$' "$prom" \
     && ! grep -q "^stellarindex_restore_drill_last_success_unix" "$prom"; then
  ok "restore failure rewrites the textfile: failures{repo=\"1\"}=1, NO last_success"
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
if grep -q '^stellarindex_restore_drill_failures{repo="1"} 1$' "$prom" \
     && ! grep -q "^stellarindex_restore_drill_last_success_unix" "$prom"; then
  ok "pg_start failure rewrites the textfile: failures{repo=\"1\"}=1, NO last_success"
else
  bad "pg_start failure left the previous PASS being scraped:
$(cat "$prom")"
fi
if ls -d "$c/root"/pgdata-* >/dev/null 2>&1; then
  ok "a post-restore failure keeps the datadir (it is the evidence)"
else
  bad "a post-restore failure removed the datadir that would have been the diagnostic"
fi

# ─── 4. a repo2 drill records under its own file + label ───────────
# The off-site timer runs this same script with DRILL_REPO=2. Its verdict
# must land in restore_drill_repo2.prom with repo="2" on every series, and
# repo1's file — last month's PASS — must be exactly as it was: a shared
# file would have been rewritten without the repo1 series, which is how a
# failed repo1 drill could read as clean after a clean repo2 drill.
c="$work/c4"; mkdir -p "$c"; seed_stale_pass "$c"
DRILL_REPO=2 FAKE_DF_AVAIL_G=5000 FAKE_BACKUP_BYTES=$((300 * 1073741824)) FAKE_RESTORE_RC=1 run_drill "$c"
rc="$(cat "$c/rc")"
if [[ "$rc" == "1" ]]; then
  ok "repo2 restore failure exits with the failure count (1)"
else
  bad "repo2 restore failure: expected exit 1, got $rc: $(tail -n 3 "$c/out")"
fi
prom2="$c/textfile/restore_drill_repo2.prom"
if [[ -f "$prom2" ]] && grep -q '^stellarindex_restore_drill_failures{repo="2"} 1$' "$prom2" \
     && ! grep -q "^stellarindex_restore_drill_last_success_unix" "$prom2"; then
  ok "a repo2 run writes restore_drill_repo2.prom: failures{repo=\"2\"}=1, NO last_success"
else
  bad "a repo2 run did not write its own labelled textfile:
$(cat "$prom2" 2>/dev/null || echo '<no file>')"
fi
if grep -q '^stellarindex_restore_drill_last_success_unix{repo="1"} 1000$' "$c/textfile/restore_drill.prom" \
     && grep -q '^stellarindex_restore_drill_failures{repo="1"} 0$' "$c/textfile/restore_drill.prom"; then
  ok "a repo2 run leaves repo1's textfile (last month's PASS) untouched"
else
  bad "a repo2 run rewrote repo1's textfile — the verdicts are no longer independent:
$(cat "$c/textfile/restore_drill.prom")"
fi
if grep -q "restore drill (repo2)" "$c/log/restore-drills.md" 2>/dev/null; then
  ok "a repo2 run's evidence entry names the repo"
else
  bad "a repo2 run's evidence entry does not name repo2"
fi

# A repo number that is not one: a label value and a file name are built
# from it, so the drill must refuse before touching anything.
c="$work/c5"; mkdir -p "$c"
DRILL_REPO='2; rm -rf /' FAKE_DF_AVAIL_G=5000 FAKE_BACKUP_BYTES=$((300 * 1073741824)) run_drill "$c"
if [[ "$(cat "$c/rc")" == "2" ]] && grep -q "DRILL_REPO must be" "$c/out" \
     && [[ ! -e "$c/log/restore-drills.md" ]] && [[ -z "$(ls -A "$c/textfile" 2>/dev/null)" ]]; then
  ok "a non-numeric DRILL_REPO is a precondition refusal (exit 2, nothing written)"
else
  bad "a non-numeric DRILL_REPO was not refused cleanly: exit $(cat "$c/rc"): $(tail -n 3 "$c/out")"
fi

# ─── 6. a HELD lock is a refusal, not a failed check ────────────────
# Two timers drive this script and a hand-run can land beside either; two
# drills would start their scratch instances on the same port and each
# would size its capacity check against free space the other is about to
# take. A held lock is a REFUSAL — exit 2, nothing written — never a
# counted failure: "another drill is running" is not a fact about the
# backup. The capacity numbers here would sail through, so only the lock
# can refuse.
c="$work/c6"; mkdir -p "$c"; seed_stale_pass "$c"
FLOCK_RESULT=1 FAKE_DF_AVAIL_G=5000 FAKE_BACKUP_BYTES=$((300 * 1073741824)) FAKE_RESTORE_RC=0 run_drill "$c"
rc="$(cat "$c/rc")"
if [[ "$rc" == "2" ]] && grep -q "another restore drill holds" "$c/out"; then
  ok "a held lock is a precondition refusal (exit 2), not a counted failure"
else
  bad "a held lock: expected exit 2 + 'another restore drill holds', got exit $rc:
$(tail -n 5 "$c/out")"
fi
if [[ ! -e "$c/log/restore-drills.md" ]] \
     && grep -q '^stellarindex_restore_drill_last_success_unix{repo="1"} 1000$' "$c/textfile/restore_drill.prom" \
     && [[ -z "$(ls -A "$c/root" 2>/dev/null)" ]]; then
  ok "a lock refusal writes no evidence, rewrites no metric and creates no datadir"
else
  bad "a lock refusal touched the evidence log, the previous run's metric or DRILL_ROOT:
$(cat "$c/textfile/restore_drill.prom" 2>/dev/null)"
fi

# ─── 7. the lock file cannot be OPENED ──────────────────────────────
# Read-only mount, a directory in the way, a namespace that does not
# expose /run/lock. Under `set -e` a bare `exec 9>` exits 1 — this
# script's code for ONE FAILED CHECK — so an unwritable lock path would
# have been read as "the drill ran and the backup failed a check". It is
# a refusal like every other: exit 2, nothing written.
c="$work/c7"; mkdir -p "$c"
FAKE_DRILL_LOCK="$c/no-such-dir/lock" FAKE_DF_AVAIL_G=5000 FAKE_BACKUP_BYTES=$((300 * 1073741824)) run_drill "$c"
rc="$(cat "$c/rc")"
if [[ "$rc" == "2" ]] && grep -q "could not open the lock file" "$c/out"; then
  ok "an unopenable lock file is a precondition refusal (exit 2), not a failed check (exit 1)"
else
  bad "an unopenable lock file: expected exit 2 + 'could not open the lock file', got exit $rc:
$(tail -n 5 "$c/out")"
fi
if [[ ! -e "$c/log/restore-drills.md" ]] && [[ -z "$(ls -A "$c/textfile" 2>/dev/null)" ]]; then
  ok "an unopenable lock file writes neither evidence nor metric"
else
  bad "an unopenable lock file wrote evidence/metric — refusals must not share the drill's signal"
fi

# ─── 8. the scratch port, which the lock does not cover ─────────────
# A run killed after pg_start leaves a daemonised postgres on
# DRILL_PG_PORT and takes its lock down with it, so the next drill takes
# the lock cleanly and would restore several hundred GB before failing at
# pg_start on "address already in use" — counted as a verification
# failure of the backup. The pre-flight refuses first: exit 2, before the
# capacity check and the restore.
c="$work/c8"; mkdir -p "$c"; seed_stale_pass "$c"
FAKE_PG_ISREADY_RC=0 FAKE_DF_AVAIL_G=5000 FAKE_BACKUP_BYTES=$((300 * 1073741824)) FAKE_RESTORE_RC=0 run_drill "$c"
rc="$(cat "$c/rc")"
if [[ "$rc" == "2" ]] && grep -q "already answers on 5499" "$c/out"; then
  ok "a postgres left on the scratch port is a precondition refusal (exit 2) before the restore"
else
  bad "an orphaned scratch instance: expected exit 2 + 'already answers on 5499', got exit $rc:
$(tail -n 5 "$c/out")"
fi
if [[ -z "$(ls -A "$c/root" 2>/dev/null)" ]] && [[ ! -e "$c/log/restore-drills.md" ]] \
     && grep -q '^stellarindex_restore_drill_last_success_unix{repo="1"} 1000$' "$c/textfile/restore_drill.prom"; then
  ok "the port refusal restores nothing and leaves the previous verdict alone"
else
  bad "the port refusal ran past the pre-flight: $(ls "$c/root" 2>/dev/null)"
fi

# ─── 9. emit_metric: no rendered line may lack a value ──────────────
#
# THE DEFECT (2026-09-10 class, pre-existing). The throughput series was
# written as
#   echo "…_ledgers_per_second${lbl} $(echo "scale=2; …" | bc)"
# and `bc` is a separate Debian package that nothing this drill installs
# depends on. Absent (or erroring), the command substitution is EMPTY
# and the line is a metric NAME WITH NO VALUE. node_exporter does not
# skip such a line — it rejects the WHOLE file — so restore_drill.prom
# would vanish entirely on precisely the runs that measured a re-derive,
# taking stellarindex_restore_drill_failures and _last_success_unix (the
# drill's only evidence that the backups restore) with it. That is the
# r1 2026-09-10 shape, which nothing alerted on for ~20 minutes.
#
# emit_metric is driven directly, extracted from the SHIPPED bytes: the
# CH stage it guards runs only after a full successful restore against a
# live Postgres, so no end-to-end fixture can reach it. Same idiom as
# data-freshness-test.sh, which extracts its probe region for the same
# reason.
echo "restore-drill-run-test: emit_metric renders no valueless line"
EMIT="$work/emit_metric.sh"
{
  echo 'set -uo pipefail'
  # shellcheck disable=SC2016  # emitted into the harness verbatim
  echo 'note() { echo "restore-drill: $*" >&2; }'
  awk '/^emit_metric\(\) \{$/ { p = 1 } p { print } p && /^\}$/ { exit }' "$DRILL"
  echo 'emit_metric'
} > "$EMIT"
if grep -q 'ledgers_per_second' "$EMIT" && grep -q 'restore_drill.prom' "$EMIT"; then
  ok "extracted emit_metric() carries the throughput series and the textfile name"
else
  bad "emit_metric() extraction produced nothing usable — the marker drifted"
fi

emitbin="$work/emitbin"; mkdir -p "$emitbin"
# `bc` absent: the shell's own "command not found" is rc 127 with an
# empty stdout, which is exactly what this models.
printf '#!/usr/bin/env bash\necho "bc: command not found" >&2\nexit 127\n' > "$emitbin/bc"
chmod +x "$emitbin/bc"

# `bc` PRESENT. The happy path must supply its own bc rather than borrow
# the host's: the pinned Linux verify lane does not ship bc, so relying on
# the host passed on macOS and failed in the container — the test was
# asserting a property of the machine, not of the script.
#
# This is a real calculator, not a canned answer. The drill emits
#   scale=2; <window> / (<secs> + 0.0001)
# so a hard-coded reply would keep passing if emit_metric stopped doing
# the arithmetic at all, which is most of what this case exists to catch.
okbin="$work/okbin"; mkdir -p "$okbin"
cat > "$okbin/bc" <<'BC'
#!/usr/bin/env bash
python3 -c 'import sys, re
e = sys.stdin.read()
m = re.match(r"\s*scale=(\d+)\s*;\s*(.+)", e, re.S)
scale = int(m.group(1)) if m else 2
body = (m.group(2) if m else e).strip()
if not re.fullmatch(r"[0-9+\-*/(). \t\r\n]+", body):
    sys.exit(1)
print(format(eval(body), "." + str(scale) + "f"))'
BC
chmod +x "$okbin/bc"

emit() { # emit <case> [PATH-prefix] → $PROM_OUT, $RC, $ERR
  local d="$work/emit-$1"; shift
  rm -rf "$d"; mkdir -p "$d"
  PROM_OUT="$d/restore_drill.prom"
  [[ -z "${SEED_PREVIOUS:-}" ]] || printf '%s\n' "$SEED_PREVIOUS" > "$PROM_OUT"
  ERR="$(PATH="${1:+$1:}$PATH" TEXTFILE_DIR="$d" DRILL_REPO=1 fail_count=0 \
         ch_secs=5 ch_rc=0 DRILL_CH_WINDOW=100000 bash "$EMIT" 2>&1 >/dev/null)"
  RC=$?
}
prom_field() { awk -v want="$1" '$1 == want { print $2 }' "$PROM_OUT"; }

# (a) bc present — the measured number is published, and the file parses.
emit healthy "$okbin"
if [[ -n "$(prom_field 'stellarindex_restore_drill_ch_rederive_ledgers_per_second{repo="1"}')" ]]; then
  ok "with bc present the throughput series is published"
else
  bad "throughput series missing on the happy path: $(cat "$PROM_OUT")"
fi
if python3 scripts/ci/lint_textfile_exposition.py --check-file "$PROM_OUT" >/dev/null 2>&1; then
  ok "the happy-path textfile parses as exposition"
else
  bad "happy path does not parse: $(python3 scripts/ci/lint_textfile_exposition.py --check-file "$PROM_OUT" 2>&1)"
fi

# (b) bc absent — the ONE series that cannot be computed is omitted and
#     every other family in the file survives.
emit nobc "$emitbin"
if python3 scripts/ci/lint_textfile_exposition.py --check-file "$PROM_OUT" >/dev/null 2>&1; then
  ok "with bc absent the textfile still parses (node_exporter keeps it)"
else
  bad "bc absent produced a file node_exporter would reject whole:
$(python3 scripts/ci/lint_textfile_exposition.py --check-file "$PROM_OUT" 2>&1)"
fi
if [[ -z "$(prom_field 'stellarindex_restore_drill_ch_rederive_ledgers_per_second{repo="1"}')" ]]; then
  ok "bc absent: the throughput series is omitted, not written without a value"
else
  bad "bc absent still wrote a throughput sample: $(cat "$PROM_OUT")"
fi
if [[ "$(prom_field 'stellarindex_restore_drill_failures{repo="1"}')" == "0" \
      && "$(prom_field 'stellarindex_restore_drill_ch_rederive_seconds{repo="1"}')" == "5" ]]; then
  ok "bc absent: failures and ch_rederive_seconds still publish (the file is not lost)"
else
  bad "bc absent took other families with it: $(cat "$PROM_OUT")"
fi
if [[ "$ERR" == *"omitting stellarindex_restore_drill_ch_rederive_ledgers_per_second"* ]]; then
  ok "bc absent is named in the journal"
else
  bad "bc absent was silent: $ERR"
fi
if [[ "$RC" -eq 0 ]]; then
  ok "a metric-writer fault does not change the drill's exit code (which counts BACKUP failures)"
else
  bad "emit_metric returned $RC — that would be read as a failed check of the backup"
fi

# (c) bc answers with something that is not a number.
printf '#!/usr/bin/env bash\necho "syntax error"\nexit 0\n' > "$emitbin/bc"
emit badbc "$emitbin"
if python3 scripts/ci/lint_textfile_exposition.py --check-file "$PROM_OUT" >/dev/null 2>&1 \
   && [[ -z "$(prom_field 'stellarindex_restore_drill_ch_rederive_ledgers_per_second{repo="1"}')" ]]; then
  ok "a non-numeric bc answer is dropped, not published"
else
  bad "a non-numeric bc answer reached the textfile: $(cat "$PROM_OUT")"
fi

# (d) the publication guard itself: a value composed elsewhere in the
#     render goes non-numeric. `date` is stubbed because last_success_unix
#     is the one other series built from an external command's stdout.
mkdir -p "$work/baddate"
printf '#!/usr/bin/env bash\necho SET\n' > "$work/baddate/date"
chmod +x "$work/baddate/date"
emit baddate "$work/baddate"
if python3 scripts/ci/lint_textfile_exposition.py --check-file "$PROM_OUT" >/dev/null 2>&1; then
  ok "the publication guard keeps the file parseable when a value goes non-numeric"
else
  bad "an unparseable value reached the published file:
$(python3 scripts/ci/lint_textfile_exposition.py --check-file "$PROM_OUT" 2>&1)"
fi
if [[ -z "$(prom_field 'stellarindex_restore_drill_last_success_unix{repo="1"}')" \
      && "$(prom_field 'stellarindex_restore_drill_unparseable_lines{repo="1"}')" == "1" \
      && "$(prom_field 'stellarindex_restore_drill_failures{repo="1"}')" == "0" ]]; then
  ok "the one bad line is withheld and counted; the rest of the file publishes"
else
  bad "withholding did not behave: $(cat "$PROM_OUT")"
fi
if [[ "$ERR" == *"withholding unparseable exposition line"* && "$ERR" == *"1 unparseable line(s) withheld"* ]]; then
  ok "the withheld line is named in the journal and the count summarised"
else
  bad "the withholding was not announced: $ERR"
fi

# (e) a validator that cannot run publishes nothing and leaves the
#     previous verdict exactly as it was.
mkdir -p "$work/noawk"
printf '#!/usr/bin/env bash\nexit 1\n' > "$work/noawk/awk"
chmod +x "$work/noawk/awk"
SEED_PREVIOUS='stellarindex_restore_drill_failures{repo="1"} 0'
emit noawk "$work/noawk"
if [[ "$(cat "$PROM_OUT")" == "$SEED_PREVIOUS" ]] && [[ "$ERR" == *"refusing to publish unvalidated bytes"* ]] \
   && [[ "$RC" -eq 0 ]]; then
  ok "a validator that cannot run refuses, keeps the previous file, and says so"
else
  bad "rc=$RC err='$ERR' published='$(cat "$PROM_OUT")'"
fi
unset SEED_PREVIOUS
leftovers=$(find "$work/emit-noawk" -type f ! -name 'restore_drill.prom' | wc -l | tr -d ' ')
if [[ "$leftovers" == "0" ]]; then
  ok "a refusal leaves no temp file behind"
else
  bad "a refusal left $leftovers temp file(s)"
fi

echo "restore-drill-run-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]] || exit 1
