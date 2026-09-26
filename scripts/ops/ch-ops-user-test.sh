#!/usr/bin/env bash
# ch-ops-user-test.sh — the shared scripts/ops contracts CI pins under
# stubs. Three of them:
#
#   A. the ClickHouse ops-credential contract for every scripts/ops
#      script that shells out to clickhouse-client (below);
#   B. ch-live-catchup.sh's required live-era floor (#371 F10);
#   C. ch-live-catchup.sh failing, not reporting "no holes", when its
#      gap scan errors (at the bottom of this file).
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
    # The destructive-DDL acknowledgement the retired D2 script used to
    # need (#286): supplied so its refusal is proven unconditional.
    # CH_FLAGS_DIR is redirected so no code path can touch the real
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
# d2-ordinal-reproject.sh is retired (#1156): it ranked rows in the
# EntryWalkVersion-1 order. It must refuse before its first query even with
# the destructive-DDL acknowledgement that used to let it proceed.
run d2 set "$OPS_DIR/d2-ordinal-reproject.sh" 45 45
d2_rc=$?
if [ "$d2_rc" -ne 0 ] && [ ! -s "$REC" ] && grep -q 'is retired' "$TMP/out.d2.set"; then
  ok "d2-ordinal-reproject.sh: retired — exits $d2_rc without calling clickhouse-client"
else
  bad "d2-ordinal-reproject.sh: expected a retirement refusal with no clickhouse-client call, got rc=$d2_rc, calls=$(wc -l < "$REC")"
fi
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

# ─── ch-live-catchup's gap-scan failure contract ────────────────────
#
# A gap scan that errors or times out returns no rows, exactly like a
# lake with no holes. It must fail the run (the unit goes failed) and
# must never claim "no holes"; the tip-extend is independent of the scan
# and still runs. Here CH_MAX and TIP resolve, and only the scan fails.
mkdir -p "$TMP/scanbin"
cat > "$TMP/scanbin/clickhouse-client" <<'STUB'
#!/usr/bin/env bash
case "$*" in
  *"SELECT max(ledger_seq) FROM stellar.ledgers"*) echo 62894100 ;;
  *) echo "Code: 159. DB::Exception: Timeout exceeded (TIMEOUT_EXCEEDED)" >&2; exit 159 ;;
esac
STUB
cat > "$TMP/scanbin/psql" <<'STUB'
#!/usr/bin/env bash
echo 62894200
STUB
cat > "$TMP/scanbin/ops" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$STUB_OUT"
STUB
chmod +x "$TMP/scanbin/"*
REC="$TMP/rec.scanfail"; : > "$REC"
env -u STELLARINDEX_CLICKHOUSE_OPS_USER -u STELLARINDEX_CLICKHOUSE_OPS_PASSWORD \
    -u CLICKHOUSE_USER -u CLICKHOUSE_PASSWORD \
    PATH="$TMP/scanbin:$PATH" OPS="$TMP/scanbin/ops" STUB_OUT="$REC" \
    STELLARINDEX_POSTGRES_DSN=postgres://stub LIVE_ERA_FROM=62894001 \
    bash "$OPS_DIR/ch-live-catchup.sh" >"$TMP/out.scanfail" 2>&1
scan_rc=$?
if [ "$scan_rc" -ne 0 ]; then
  ok "ch-live-catchup.sh: failed gap scan ⇒ non-zero exit"
else
  bad "ch-live-catchup.sh: failed gap scan ⇒ exited 0; the unit reports success on an unscanned lake"
fi
if grep -q 'no holes' "$TMP/out.scanfail"; then
  bad "ch-live-catchup.sh: failed gap scan ⇒ claimed 'no holes'"
  sed 's/^/       /' "$TMP/out.scanfail"
else
  ok "ch-live-catchup.sh: failed gap scan ⇒ does not claim 'no holes'"
fi
if grep -q 'TIMEOUT_EXCEEDED' "$TMP/out.scanfail"; then
  ok "ch-live-catchup.sh: failed gap scan ⇒ ClickHouse's error reaches the log"
else
  bad "ch-live-catchup.sh: failed gap scan ⇒ ClickHouse's error was discarded"
fi
if grep -q -- '-from 62894101 -to 62894200 ' "$REC"; then
  ok "ch-live-catchup.sh: failed gap scan ⇒ tip-extend still runs"
else
  bad "ch-live-catchup.sh: failed gap scan ⇒ tip-extend [62894101,62894200] did not run: '$(cat "$REC")'"
fi

# ─── ch-live-catchup heals the FLOOR hole, not just interior ones ───
#
# Third contract. The projector clamps on ContiguousWatermark, which
# stalls a source whose cursor sits in a hole AT the scan floor (its
# min_present arm). The script's interior-only leadInFrame scan could
# not see that hole — the cutover band [LIVE_ERA_FROM, first live
# ledger) — so the stall waited forever on a heal that never came.
#
# The lake stub answers by query shape for a fixed ledger set and the
# ops stub records every ch-backfill range the script asks for.
mkdir -p "$TMP/lake/bin"
cat > "$TMP/lake/bin/clickhouse-client" <<'STUB'
#!/usr/bin/env bash
q="${*: -1}"
case "$q" in
  *leadInFrame*)       if [ -n "$LAKE_GAPS" ]; then printf '%b\n' "$LAKE_GAPS"; fi ;;
  *"min(ledger_seq)"*) [ "$LAKE_MIN" = fail ] && exit 1; echo "$LAKE_MIN" ;;
  *"max(ledger_seq)"*) echo "$LAKE_MAX" ;;
  *) echo "unexpected query: $q" >&2; exit 1 ;;
esac
STUB
cat > "$TMP/lake/bin/psql" <<'STUB'
#!/usr/bin/env bash
echo "$LAKE_TIP"
STUB
cat > "$TMP/lake/bin/ops" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$OPS_REC"
STUB
chmod +x "$TMP/lake/bin/"*

# catchup_heals <label> <min> <interior gaps> <expected rc> <expected ranges…>
# Floor 100, lake max 120, indexer tip 120; ranges are "from-to".
catchup_heals() {
  local label="$1" min="$2" gaps="$3" want_rc="$4" rc got want
  shift 4
  local rec="$TMP/ops.heal.$label"; : > "$rec"
  env -u STELLARINDEX_CLICKHOUSE_OPS_USER -u STELLARINDEX_CLICKHOUSE_OPS_PASSWORD \
      -u CLICKHOUSE_USER -u CLICKHOUSE_PASSWORD \
      PATH="$TMP/lake/bin:$PATH" OPS="$TMP/lake/bin/ops" OPS_REC="$rec" \
      STELLARINDEX_POSTGRES_DSN=postgres://stub LIVE_ERA_FROM=100 \
      LAKE_MAX=120 LAKE_TIP=120 LAKE_MIN="$min" LAKE_GAPS="$gaps" \
      bash "$OPS_DIR/ch-live-catchup.sh" >"$TMP/out.heal.$label" 2>&1
  rc=$?
  got="$(sed -n 's/^ch-backfill .*-from \([0-9]*\) -to \([0-9]*\) .*/\1-\2/p' "$rec" | tr '\n' ' ')"
  want="$(printf '%s ' "$@")"
  if awk '/^ch-backfill / && !/ -write / { f = 1 } END { exit !f }' "$rec"; then
    bad "ch-live-catchup.sh: $label ⇒ a ch-backfill call omits -write, so it only previews (#868)"
  fi
  if [ "$got" = "$want" ] && [ "$rc" -eq "$want_rc" ]; then
    ok "ch-live-catchup.sh: $label ⇒ heals '${want% }' (rc $rc)"
  else
    bad "ch-live-catchup.sh: $label ⇒ expected heals '${want% }' rc $want_rc, got '${got% }' rc $rc"
    sed 's/^/       /' "$TMP/out.heal.$label"
  fi
}

# Present {105..110, 113..120}: floor hole [100,104] AND interior [111,112].
catchup_heals floor-hole 105 '111\t112' 0 100-104 111-112
# Floor hole with no interior hole at all — the scan used to say "no holes".
catchup_heals floor-hole-only 105 '' 0 100-104
# Floor present: interior-only behaviour is unchanged.
catchup_heals floor-present 100 '111\t112' 0 111-112
# The floor probe itself failing must not read as "no floor hole".
catchup_heals floor-probe-fails fail '111\t112' 1 111-112

echo "ch-ops-user-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
