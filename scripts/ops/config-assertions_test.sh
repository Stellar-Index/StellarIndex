#!/usr/bin/env bash
# config-assertions_test.sh — fixture tests for the Postgres
# max_worker_processes headroom pair (T615), the continuous-aggregate
# refresh-policy check, the trades compression-policy scheduled check and
# the ClickHouse destructive-DDL size guard's effective value (T616).
#
# Pins the property T615 was about: max_worker_processes is
# postmaster-level (postgresql.conf.j2's own comment), so an ansible
# apply that rewrites the file does nothing to the RUNNING server until
# it restarts. A file-only grep cannot see that gap; the live SHOW must
# disagree with the file while a restart is pending. Before this fix,
# config-assertions.sh emitted no metric for this at all — this test
# fails against that version because the assertion name it greps for
# does not exist yet.
#
# Uses a fake `psql` on PATH and overridable PG_CONF_FILE /
# PG_PASSWORD_FILE (same override pattern as TEXTFILE_DIR) so this runs
# without root or a live Postgres.
#
# Run: bash scripts/ops/config-assertions_test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
GATE="$PWD/scripts/ops/config-assertions.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

FAKEBIN="$TMP/bin"
mkdir -p "$FAKEBIN"
cat > "$FAKEBIN/psql" <<'EOF'
#!/usr/bin/env bash
for a in "$@"; do
  case "$a" in
    *"SHOW max_worker_processes"*) echo "${FAKE_PG_MAX_WORKER_PROCESSES:-}"; exit 0 ;;
    *"SHOW idle_in_transaction_session_timeout"*) echo "${FAKE_PG_IDLE_IN_TXN_TIMEOUT:-}"; exit 0 ;;
    *"timescaledb_information.continuous_aggregates"*)
      [ "${FAKE_PG_DOWN:-0}" = 1 ] && exit 2
      echo "${FAKE_PG_CAGGS_WITHOUT_POLICY:-0}"; exit 0 ;;
    *"hashtext('usd-volume-restamp:trades')"*)
      [ "${FAKE_PG_DOWN:-0}" = 1 ] && exit 2
      echo "${FAKE_PG_TRADES_POLICY_OK:-1}"; exit 0 ;;
  esac
done
exit 1
EOF
chmod +x "$FAKEBIN/psql"
echo x > "$TMP/pgpass"
# Fake runuser: the no-password-file fallback. Drops "-u postgres --" and
# runs the rest (the fake psql), so FAKE_RUNUSER_DOWN=1 models failed peer auth.
cat > "$FAKEBIN/runuser" <<'EOF'
#!/usr/bin/env bash
[ "${FAKE_RUNUSER_DOWN:-0}" = 1 ] && exit 1
[ "$1 $2 $3" = "-u postgres --" ] || exit 1
shift 3
touch "$RUNUSER_MARK"
exec "$@"
EOF
chmod +x "$FAKEBIN/runuser"

# Fake curl: answers only the drop-guard query, with FAKE_CH_DROP_GUARD
# (printf %b escapes); every other query fails like an unreachable lake.
cat > "$FAKEBIN/curl" <<'EOF'
#!/usr/bin/env bash
for a in "$@"; do
  case "$a" in
    *"system.server_settings"*"max_table_size_to_drop"*)
      [ "${FAKE_CH_DOWN:-0}" = 1 ] && exit 7
      printf '%b' "${FAKE_CH_DROP_GUARD:-}"; exit 0 ;;
  esac
done
exit 7
EOF
chmod +x "$FAKEBIN/curl"
mkdir -p "$TMP/ch-config"
cat > "$FAKEBIN/ss" <<'EOF'
#!/usr/bin/env bash
[ "${FAKE_SS_DOWN:-0}" = 1 ] && exit 1
printf '%b' "${FAKE_SS:-}"
EOF
chmod +x "$FAKEBIN/ss"
printf 'server:\n  http_listen_address: 127.0.0.1\n  grpc_listen_address: 127.0.0.1\n' > "$TMP/loki-pinned.yml"
printf 'server:\n  http_listen_port: 3100\n' > "$TMP/loki-unpinned.yml"

pass=0
fail=0

# run <conf-value> <live-value> [idle-conf-value] [idle-live-value] —
# one gate invocation; sets OUT to the emitted .prom file's content.
# idle-* default to the correct 30min pair so callers pinning only the
# max_worker_processes shape don't also spuriously fail the idle-in-transaction checks.
run() {
  local conf="$1" live="$2" idle_conf="${3:-30min}" idle_live="${4:-30min}"
  printf 'max_worker_processes      = %s\n' "$conf" > "$TMP/postgresql.conf"
  [ "$idle_conf" = "ABSENT" ] || printf 'idle_in_transaction_session_timeout = %s\n' "$idle_conf" >> "$TMP/postgresql.conf"
  rm -rf "$TMP/out"
  mkdir -p "$TMP/out"
  PATH="$FAKEBIN:$PATH" \
    TEXTFILE_DIR="$TMP/out" \
    PG_CONF_FILE="$TMP/postgresql.conf" \
    PG_PASSWORD_FILE="${PGPASS:-$TMP/pgpass}" \
    RUNUSER_MARK="$TMP/runuser.mark" \
    FAKE_RUNUSER_DOWN="${RUNUSER_DOWN:-0}" \
    STELLAR_TOML="${TOML:-$TMP/absent.toml}" \
    FAKE_PG_MAX_WORKER_PROCESSES="$live" \
    FAKE_PG_IDLE_IN_TXN_TIMEOUT="$idle_live" \
    FAKE_PG_CAGGS_WITHOUT_POLICY="${CAGGS_MISSING:-0}" \
    FAKE_PG_TRADES_POLICY_OK="${TRADES_POLICY_OK:-1}" \
    FAKE_PG_DOWN="${PG_DOWN:-0}" \
    CH_CONFIG_DIR="${CH_DIR:-$TMP/ch-config}" \
    FAKE_CH_DROP_GUARD="${CH_GUARD:-}" \
    FAKE_CH_DOWN="${CH_DOWN:-0}" \
    LOKI_CONFIG="${LOKI_CFG:-$TMP/absent-loki.yml}" \
    FAKE_SS="${SS_OUT:-}" \
    FAKE_SS_DOWN="${SS_DOWN:-0}" \
    bash "$GATE" >/dev/null 2>&1
  OUT="$(cat "$TMP/out/config_assertions.prom" 2>/dev/null)"
}

# expect_metric <name> <assertion> <want-value>
expect_metric() {
  local name="$1" assertion="$2" want="$3" line got
  line="$(grep -F "assertion=\"$assertion\"" <<<"$OUT")"
  if [ -z "$line" ]; then
    echo "FAIL: $name — no metric for assertion=$assertion" >&2
    fail=$((fail + 1))
    return
  fi
  got="${line##* }"
  if [ "$got" != "$want" ]; then
    echo "FAIL: $name — assertion=$assertion is $got, want $want" >&2
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

# Fully applied: file says 32, live server already restarted onto it.
run 32 32
expect_metric 'both applied -> codified ok' pg_max_worker_processes_codified 1
expect_metric 'both applied -> live ok' pg_max_worker_processes_live 1

# The T615 shape: ansible rendered the file but nobody restarted
# Postgres yet, so the running server is still at the old default.
run 32 8
expect_metric 'restart pending -> codified still ok (file is right)' pg_max_worker_processes_codified 1
expect_metric 'restart pending -> live catches the gap' pg_max_worker_processes_live 0

# Reverted file (an ansible run without the fix, or a hand revert).
run 8 8
expect_metric 'reverted file -> codified catches it' pg_max_worker_processes_codified 0

# idle_in_transaction_session_timeout, same codified-vs-applied
# pairing. Unlike max_worker_processes this GUC is reload-only, so
# "codified but not live" means the reload never ran, not "awaiting a
# restart" — pinned here as its own case rather than folded into the
# name above.
run 32 32 30min 30min
expect_metric 'idle timeout applied -> codified ok' pg_idle_in_transaction_timeout_codified 1
expect_metric 'idle timeout applied -> live ok' pg_idle_in_transaction_timeout_live 1

# Rendered but never reloaded: the file says 30min, the running server
# is still on whatever it booted with (0 = disabled, Postgres's own
# default).
run 32 32 30min 0
expect_metric 'idle timeout codified, reload pending -> codified still ok' pg_idle_in_transaction_timeout_codified 1
expect_metric 'idle timeout codified, reload pending -> live catches the gap' pg_idle_in_transaction_timeout_live 0

# The unfixed shape: the GUC is absent from the file entirely.
run 32 32 ABSENT 0
expect_metric 'idle timeout absent from file -> codified catches it' pg_idle_in_transaction_timeout_codified 0

# ── Every continuous aggregate has a refresh policy ──────────────────
# The fake answers the assertion's count of aggregates with no refresh
# job. Every CAGG covered -> ok; one policy dropped -> the check fails
# (the timescale-jobs probe emits nothing for that view, so this is the
# only signal); Postgres unreachable -> fails closed.
run 32 32
expect_metric 'every cagg has a refresh policy -> ok' caggs_have_refresh_policy 1
CAGGS_MISSING=1 run 32 32
expect_metric 'one cagg policy dropped -> caught' caggs_have_refresh_policy 0
PG_DOWN=1 run 32 32
expect_metric 'postgres unreachable -> fails closed' caggs_have_refresh_policy 0

# ── The trades compression policy is scheduled ──────────────────────
# The fake answers the count of trades policies that are scheduled, or
# paused while a restamp run holds its lock. 0 is a policy left paused by
# a killed run (or no policy at all).
run 32 32
expect_metric 'trades policy scheduled or paused by a live run -> ok' trades_compression_policy_scheduled 1
TRADES_POLICY_OK=0 run 32 32
expect_metric 'trades policy paused with no run holding the lock -> caught' trades_compression_policy_scheduled 0
PG_DOWN=1 run 32 32
expect_metric 'postgres unreachable -> trades policy check fails closed' trades_compression_policy_scheduled 0

# ── No password file (test nets): local postgres superuser fallback ──
# Readable file keeps the password path; absent file uses runuser; both
# unavailable still fails rather than skipping.
PG_LIVE='pg_max_worker_processes_live pg_idle_in_transaction_timeout_live caggs_have_refresh_policy trades_compression_policy_scheduled'
rm -f "$TMP/runuser.mark"; run 32 32
if [ ! -e "$TMP/runuser.mark" ]; then echo "ok: password file readable -> runuser not used"; pass=$((pass + 1)); else echo "FAIL: runuser used despite readable password file" >&2; fail=$((fail + 1)); fi
rm -f "$TMP/runuser.mark"; PGPASS="$TMP/no-such-pass" run 32 32
for a in $PG_LIVE; do expect_metric "no password file -> fallback ok ($a)" "$a" 1; done
if [ -e "$TMP/runuser.mark" ]; then echo "ok: fallback went through runuser"; pass=$((pass + 1)); else echo "FAIL: runuser fallback not taken" >&2; fail=$((fail + 1)); fi
PGPASS="$TMP/no-such-pass" CAGGS_MISSING=1 run 32 32
expect_metric 'fallback still catches a missing cagg policy' caggs_have_refresh_policy 0
PGPASS="$TMP/no-such-pass" RUNUSER_DOWN=1 run 32 32
for a in $PG_LIVE; do expect_metric "no password file and runuser fails -> fail ($a)" "$a" 0; done

# ── supply_reserve_accounts_nonempty is pubnet-only ─────────────────
printf 'network = "testnet"\n[supply]\nsdf_reserve_accounts = [\n]\n' > "$TMP/testnet.toml"
printf 'network = "pubnet"\n[supply]\nsdf_reserve_accounts = [\n]\n' > "$TMP/pubnet-empty.toml"
printf 'network = "pubnet"\n[supply]\nsdf_reserve_accounts = [\n  "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",\n]\n' > "$TMP/pubnet-full.toml"
TOML="$TMP/testnet.toml" run 32 32
if grep -qF 'config_assertion_skipped{assertion="supply_reserve_accounts_nonempty"} 1' <<<"$OUT" \
  && ! grep -qF 'config_assertion_ok{assertion="supply_reserve_accounts_nonempty"}' <<<"$OUT"; then
  echo "ok: testnet empty reserve list -> skipped"; pass=$((pass + 1))
else
  echo "FAIL: testnet empty reserve list not skipped" >&2; fail=$((fail + 1))
fi
TOML="$TMP/pubnet-empty.toml" run 32 32
expect_metric 'pubnet empty reserve list -> enforced' supply_reserve_accounts_nonempty 0
TOML="$TMP/pubnet-full.toml" run 32 32
expect_metric 'pubnet populated reserve list -> ok' supply_reserve_accounts_nonempty 1
run 32 32
expect_metric 'unreadable config -> treated as pubnet, enforced' supply_reserve_accounts_nonempty 0

# ── ClickHouse drop guard (T616) ────────────────────────────────────
# The ansible verify task asserts the EFFECTIVE limits once, at apply
# time. These pin the hourly re-assertion that catches the r1 shape — a
# later-sorting hand-written config.d file (or a hand edit) raising a
# limit to 1 TiB with no ansible run. The query is ORDER BY name.
GIB50=53687091200
TIB1=1099511627776
guard() { CH_GUARD="max_partition_size_to_drop\t$1\nmax_table_size_to_drop\t$2\n"; }

guard "$GIB50" "$GIB50"; run 32 32
expect_metric 'drop guard at the pinned 50 GiB -> ok' ch_drop_guard_live 1

guard 10737418240 10737418240; run 32 32
expect_metric 'drop guard stricter than pinned -> ok' ch_drop_guard_live 1

guard "$TIB1" "$GIB50"; run 32 32
expect_metric 'partition limit raised to 1 TiB (the r1 shape) -> fail' ch_drop_guard_live 0

guard "$GIB50" "$TIB1"; run 32 32
expect_metric 'table limit raised to 1 TiB -> fail' ch_drop_guard_live 0

guard 0 "$GIB50"; run 32 32
expect_metric 'partition limit 0 (unlimited) -> fail' ch_drop_guard_live 0

guard "$GIB50" 0; run 32 32
expect_metric 'table limit 0 (unlimited) -> fail' ch_drop_guard_live 0

guard "$GIB50" 99999999999999999999; run 32 32
expect_metric 'value past 64-bit shell arithmetic -> fail, not wrap' ch_drop_guard_live 0

CH_GUARD="max_table_size_to_drop\t$GIB50\n"; run 32 32
expect_metric 'one setting missing from the answer -> fail' ch_drop_guard_live 0

CH_GUARD=""; run 32 32
expect_metric 'empty answer -> fail' ch_drop_guard_live 0

guard "$GIB50" "$GIB50"; CH_DOWN=1; run 32 32; CH_DOWN=0
expect_metric 'ClickHouse unreachable -> fail (unknown is not safe)' ch_drop_guard_live 0

# A host with no ClickHouse config dir has no lake to guard: an explicit
# _skipped series, never an _ok sample (which would page forever).
guard "$TIB1" "$TIB1"; CH_DIR="$TMP/no-such-dir"; run 32 32; unset CH_DIR
if grep -qF 'stellarindex_config_assertion_skipped{assertion="ch_drop_guard_live"} 1' <<<"$OUT" \
  && ! grep -qF 'stellarindex_config_assertion_ok{assertion="ch_drop_guard_live"}' <<<"$OUT"; then
  echo "ok: no ClickHouse -> skipped, not failed"; pass=$((pass + 1))
else
  echo "FAIL: no ClickHouse -> want only a _skipped series for ch_drop_guard_live" >&2
  fail=$((fail + 1))
fi

# The textfile must reach the collector whole and readable: at the rename
# it is already mode 644 and sits in the target's directory (a same-fs
# rename, not a cross-fs copy). A shimmed mv records both at that moment.
MVBIN="$TMP/mvbin"
mkdir -p "$MVBIN"
cat > "$MVBIN/mv" <<'EOF'
#!/usr/bin/env bash
src="${@: -2:1}" dst="${@: -1}"
if [ -n "$(find "$src" -perm 644 2>/dev/null)" ] && [ "$(dirname "$src")" = "$(dirname "$dst")" ]; then
  echo ok >> "$MV_LOG"
else
  echo bad >> "$MV_LOG"
fi
exec /bin/mv "$@"
EOF
chmod +x "$MVBIN/mv"
: > "$TMP/mv.log"
guard "$GIB50" "$GIB50"; PATH="$MVBIN:$PATH" MV_LOG="$TMP/mv.log" run 32 32
if [ "$(cat "$TMP/mv.log")" = "ok" ] && [ "$(ls -A "$TMP/out")" = "config_assertions.prom" ]; then
  echo "ok: textfile renamed into place already 644, from TEXTFILE_DIR"; pass=$((pass + 1))
else
  echo "FAIL: textfile rename not atomic+readable (mv: $(cat "$TMP/mv.log"); out: $(ls -A "$TMP/out"))" >&2
  fail=$((fail + 1))
fi

# ── Loki loopback bind ──────────────────────────────────────────────
LO='LISTEN 0 4096 127.0.0.1:3100 0.0.0.0:*\nLISTEN 0 4096 127.0.0.1:9096 0.0.0.0:*\nLISTEN 0 128 0.0.0.0:22 0.0.0.0:*\n'
WILD='LISTEN 0 4096 *:3100 *:*\nLISTEN 0 4096 *:9096 *:*\n'
run 32 32
if grep -q 'config_assertion_skipped{assertion="loki_loopback_bound"}' <<<"$OUT"; then echo 'ok: no loki config -> skipped'; pass=$((pass + 1)); else echo 'FAIL: loki skip not emitted' >&2; fail=$((fail + 1)); fi
LOKI_CFG="$TMP/loki-pinned.yml" SS_OUT="$LO" run 32 32
expect_metric 'loki pinned and bound to loopback -> ok' loki_loopback_bound 1
LOKI_CFG="$TMP/loki-pinned.yml" SS_OUT="$WILD" run 32 32
expect_metric 'config pinned but running Loki still on * (restart pending) -> caught' loki_loopback_bound 0
LOKI_CFG="$TMP/loki-unpinned.yml" SS_OUT="$LO" run 32 32
expect_metric 'config not pinned -> caught' loki_loopback_bound 0
LOKI_CFG="$TMP/loki-pinned.yml" SS_DOWN=1 run 32 32
expect_metric 'ss unavailable -> fails closed' loki_loopback_bound 0

# Lockstep: the script's default ceiling IS the role's pinned value. A
# role change without the script (or vice versa) either fails every
# host forever or silently accepts a raised limit.
DEFAULTS=configs/ansible/roles/archival-node/defaults/main.yml
script_ceiling="$(sed -nE 's/^CH_DROP_GUARD_MAX_BYTES="\$\{CH_DROP_GUARD_MAX_BYTES:-([0-9]+)\}"$/\1/p' "$GATE")"
for v in clickhouse_max_table_size_to_drop clickhouse_max_partition_size_to_drop; do
  role="$(sed -nE "s/^${v}:[[:space:]]*([0-9]+).*/\1/p" "$DEFAULTS")"
  if [ -n "$role" ] && [ "$role" = "$script_ceiling" ]; then
    echo "ok: $v ($role) matches the script's ceiling"; pass=$((pass + 1))
  else
    echo "FAIL: $v is '$role' in $DEFAULTS but the script ceiling is '$script_ceiling'" >&2
    fail=$((fail + 1))
  fi
done

echo
echo "config-assertions-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
