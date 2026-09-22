#!/usr/bin/env bash
# config-assertions_test.sh — fixture tests for the Postgres
# max_worker_processes headroom pair (T615).
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
  esac
done
exit 1
EOF
chmod +x "$FAKEBIN/psql"
echo x > "$TMP/pgpass"

pass=0
fail=0

# run <conf-value> <live-value> — one gate invocation; sets OUT to the
# emitted .prom file's content.
run() {
  local conf="$1" live="$2"
  printf 'max_worker_processes      = %s\n' "$conf" > "$TMP/postgresql.conf"
  rm -rf "$TMP/out"
  mkdir -p "$TMP/out"
  PATH="$FAKEBIN:$PATH" \
    TEXTFILE_DIR="$TMP/out" \
    PG_CONF_FILE="$TMP/postgresql.conf" \
    PG_PASSWORD_FILE="$TMP/pgpass" \
    FAKE_PG_MAX_WORKER_PROCESSES="$live" \
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

echo
echo "config-assertions-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
