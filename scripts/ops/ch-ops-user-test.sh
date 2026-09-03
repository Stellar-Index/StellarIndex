#!/usr/bin/env bash
# ch-ops-user-test.sh — the shared scripts/ops contracts CI pins under
# stubs. Two of them:
#
#   A. the ClickHouse ops-credential contract for every scripts/ops
#      script that shells out to clickhouse-client (below);
#   B. ch-live-catchup.sh's required live-era floor (#371 F10, at the
#      bottom of this file).
#
# Contract (2026-08-28 Wave A follow-up):
#
#   1. STELLARINDEX_CLICKHOUSE_OPS_USER set ⇒ clickhouse-client runs as
#      that user, with STELLARINDEX_CLICKHOUSE_OPS_PASSWORD handed over
#      via its CLICKHOUSE_USER / CLICKHOUSE_PASSWORD environment — NEVER
#      argv, which `ps` and the journal would show (never-pass-secrets-
#      in-argv). So the assertion is on the stub's environment, and the
#      password must be absent from its argv.
#   2. Unset ⇒ byte-identical: CLICKHOUSE_USER/CLICKHOUSE_PASSWORD stay
#      unset AND the argv clickhouse-client sees is exactly the argv it
#      saw before this contract existed (pinned per script below).
#
# clickhouse-client, ssh and psql are stubbed on PATH, so this runs
# anywhere in about a second and never reaches a real lake.
#
# Run: bash scripts/ops/ch-ops-user-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
OPS_DIR="$PWD/scripts/ops"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

# ─── stubs ──────────────────────────────────────────────────────────
mkdir -p "$TMP/bin" "$TMP/state"
# Records "<user>|<password>|<argv>" then FAILS, so every script bails
# out on its first query and never gets near a mutating statement.
cat > "$TMP/bin/clickhouse-client" <<'STUB'
#!/usr/bin/env bash
printf '%s|%s|%s\n' "${CLICKHOUSE_USER-<unset>}" "${CLICKHOUSE_PASSWORD-<unset>}" "$*" >> "$STUB_OUT"
exit 1
STUB
# ch-backfill-monitor runs its clickhouse-client on the far side of
# ssh: execute the remote command string locally instead.
cat > "$TMP/bin/ssh" <<'STUB'
#!/usr/bin/env bash
exec bash -c "${*: -1}"
STUB
cat > "$TMP/bin/psql" <<'STUB'
#!/usr/bin/env bash
exit 1
STUB
chmod +x "$TMP/bin/"*
export PATH="$TMP/bin:$PATH"

# The monitor's remote side sources the ops env file on the HOST; point
# it at a fixture so the "set" case is exercised end-to-end.
printf 'STELLARINDEX_CLICKHOUSE_OPS_USER=%s\nSTELLARINDEX_CLICKHOUSE_OPS_PASSWORD=%s\n' \
  ops_rw 's3cr3t-pw' > "$TMP/ops-env.set"
: > "$TMP/ops-env.unset"
printf 'ALL WINDOWS COMPLETE\n' > "$TMP/backfill.log"   # terminal marker ⇒ monitor exits its loop

# run <name> <mode set|unset> <script> [args…] — runs the script with the
# stubs, credentials per mode, and leaves the stub record in $REC.
run() {
  local name="$1" mode="$2"; shift 2
  REC="$TMP/rec.$name.$mode"; : > "$REC"
  local -a envs=(
    STUB_OUT="$REC"
    STELLARINDEX_POSTGRES_DSN=postgres://stub
    D2_STATE="$TMP/state/d2.$mode" D3_STATE="$TMP/state/d3.$mode"
    STATE="$TMP/state/st.$mode" LOG="$TMP/backfill.log"
    HOST=stub-host INTERVAL=0 DRIVER_PAT=no-such-driver-$$
    # D2 refuses to start without an explicit destructive-DDL
    # acknowledgement (#286) — it exits BEFORE its first query, so
    # without this the credential contract below never gets a stub
    # invocation to assert on. Acknowledging is safe here: the stubbed
    # clickhouse-client fails the first (read-only) query, so the script
    # bails long before any REPLACE PARTITION or DROP. CH_FLAGS_DIR is
    # redirected so a future code path can never touch the real
    # /var/lib/clickhouse/flags.
    D2_FORCE_DROP=yes CH_FLAGS_DIR="$TMP/flags"
  )
  # TO is the monitor's required range end; the seed script resolves its
  # own TO from the lake, so only the monitor may see it pre-set.
  [ "$name" = backfill-monitor ] && envs+=(TO=1)
  # LIVE_ERA_FROM is ch-live-catchup's required live-era floor (#371 F10);
  # ansible templates it into /etc/default/stellarindex-ops per host. The
  # script now refuses to run without it, so the credential contract below
  # needs it supplied — the "absent" case is asserted on its own further
  # down, not here.
  [ "$name" = live-catchup ] && envs+=(LIVE_ERA_FROM=62894001)
  if [ "$mode" = set ]; then
    envs+=(STELLARINDEX_CLICKHOUSE_OPS_USER=ops_rw STELLARINDEX_CLICKHOUSE_OPS_PASSWORD='s3cr3t-pw'
           OPS_ENV="$TMP/ops-env.set")
  else
    envs+=(OPS_ENV="$TMP/ops-env.unset")
  fi
  env -u STELLARINDEX_CLICKHOUSE_OPS_USER -u STELLARINDEX_CLICKHOUSE_OPS_PASSWORD \
      -u CLICKHOUSE_USER -u CLICKHOUSE_PASSWORD \
      "${envs[@]}" bash "$@" >"$TMP/out.$name.$mode" 2>&1
}

# check <name> <script> <expected argv when unset> [args…]
check() {
  local name="$1" script="$2" argv_unset="$3"; shift 3
  local line

  run "$name" set "$OPS_DIR/$script" "$@"
  line="$(head -n1 "$REC")"
  if [ -z "$line" ]; then
    bad "$script: clickhouse-client was never invoked (stub harness broken?)"
    sed 's/^/       /' "$TMP/out.$name.set"
    return
  fi
  case "$line" in
    "ops_rw|s3cr3t-pw|$argv_unset") ok "$script: OPS_USER set ⇒ CLICKHOUSE_USER/PASSWORD via env, argv unchanged" ;;
    *) bad "$script: OPS_USER set ⇒ expected 'ops_rw|s3cr3t-pw|$argv_unset', got '$line'" ;;
  esac
  case "$line" in
    *"|"*"s3cr3t-pw"*"|"*"s3cr3t-pw"*) bad "$script: password leaked into clickhouse-client argv" ;;
    *) ok "$script: password absent from argv" ;;
  esac

  run "$name" unset "$OPS_DIR/$script" "$@"
  line="$(head -n1 "$REC")"
  if [ "$line" = "<unset>|<unset>|$argv_unset" ]; then
    ok "$script: OPS_USER unset ⇒ byte-identical invocation"
  else
    bad "$script: OPS_USER unset ⇒ expected '<unset>|<unset>|$argv_unset', got '$line'"
  fi
}

echo "ch-ops-user-test: scripts/ops clickhouse-client credential contract"

check live-catchup ch-live-catchup.sh \
  "--port 9300 -q SELECT max(ledger_seq) FROM stellar.ledgers"
check supply-seed ch-supply-flows-seed.sh \
  "--port 9300 -q SELECT max(ledger_seq) FROM stellar.ledgers"
check d2 d2-ordinal-reproject.sh \
  "--port 9300 --max_execution_time 3600 --max_memory_usage 20000000000 --max_bytes_before_external_sort 4000000000 --max_bytes_before_external_group_by 4000000000 --max_threads 10 -q SELECT count() FROM stellar.ledger_entry_changes FINAL WHERE ledger_seq BETWEEN 45000000 AND 45999999" \
  45 45
check d3 d3-lecur-v2-rebuild.sh \
  "--port 9300 --max_execution_time 3600 --max_memory_usage 20000000000 --max_bytes_before_external_sort 4000000000 --max_bytes_before_external_group_by 4000000000 --max_threads 10 -q SELECT max(ledger_seq) FROM stellar.ledger_entry_changes" \
  probe-ordinals
check backfill-monitor ch-backfill-monitor.sh \
  "--port 9300 --query SELECT formatReadableSize(sum(bytes_on_disk)) FROM system.parts WHERE database='stellar' AND active"


# ─── ch-live-catchup's live-era floor contract (#371 F10) ───────────
#
# Second contract in this file, and it lives here rather than in a new
# script because this is the harness CI already runs over scripts/ops
# (.github/workflows/ci.yml, scripts/dev/verify.sh) — a guard nothing
# invokes is not a guard.
#
# ch-live-catchup.sh used to default LIVE_ERA_FROM to r1's mainnet
# backfill ceiling and ansible copied the script verbatim to every
# host, so a different network inherited a floor above its own tip and
# the lake's ONLY self-healer silently scanned an empty range. The
# value is now required, and both ways of supplying a bad one must
# fail BEFORE any ClickHouse query — hence the "never invoked"
# assertion: an unusable floor must not reach the gap-scan SQL it is
# interpolated into.
catchup_refuses() {
  local label="$1" line
  shift
  REC="$TMP/rec.floor.$label"; : > "$REC"
  env -u STELLARINDEX_CLICKHOUSE_OPS_USER -u STELLARINDEX_CLICKHOUSE_OPS_PASSWORD \
      -u CLICKHOUSE_USER -u CLICKHOUSE_PASSWORD -u LIVE_ERA_FROM \
      STUB_OUT="$REC" STELLARINDEX_POSTGRES_DSN=postgres://stub \
      OPS_ENV="$TMP/ops-env.unset" "$@" \
      bash "$OPS_DIR/ch-live-catchup.sh" >"$TMP/out.floor.$label" 2>&1
  local rc=$?
  if [ "$rc" -eq 0 ]; then
    bad "ch-live-catchup.sh: $label ⇒ exited 0; the floor must be refused, not guessed"
  else
    ok "ch-live-catchup.sh: $label ⇒ non-zero exit"
  fi
  if grep -q 'LIVE_ERA_FROM' "$TMP/out.floor.$label"; then
    ok "ch-live-catchup.sh: $label ⇒ names LIVE_ERA_FROM in the failure"
  else
    bad "ch-live-catchup.sh: $label ⇒ failure message does not name LIVE_ERA_FROM"
    sed 's/^/       /' "$TMP/out.floor.$label"
  fi
  line="$(head -n1 "$REC")"
  if [ -z "$line" ]; then
    ok "ch-live-catchup.sh: $label ⇒ no ClickHouse query was issued"
  else
    bad "ch-live-catchup.sh: $label ⇒ queried ClickHouse anyway: '$line'"
  fi
}

catchup_refuses unset
catchup_refuses non-numeric LIVE_ERA_FROM='62894001; DROP'

# …and the sunny path still runs: a valid floor reaches the first query.
REC="$TMP/rec.floor.valid"; : > "$REC"
env -u STELLARINDEX_CLICKHOUSE_OPS_USER -u STELLARINDEX_CLICKHOUSE_OPS_PASSWORD \
    -u CLICKHOUSE_USER -u CLICKHOUSE_PASSWORD \
    STUB_OUT="$REC" STELLARINDEX_POSTGRES_DSN=postgres://stub \
    OPS_ENV="$TMP/ops-env.unset" LIVE_ERA_FROM=62894001 \
    bash "$OPS_DIR/ch-live-catchup.sh" >"$TMP/out.floor.valid" 2>&1
if [ -n "$(head -n1 "$REC")" ]; then
  ok "ch-live-catchup.sh: a valid floor still reaches the lake query"
else
  bad "ch-live-catchup.sh: a valid floor was refused — the guard is too strict"
  sed 's/^/       /' "$TMP/out.floor.valid"
fi

echo "ch-ops-user-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
