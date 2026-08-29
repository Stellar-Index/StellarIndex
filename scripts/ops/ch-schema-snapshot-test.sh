#!/usr/bin/env bash
# ch-schema-snapshot-test.sh — fixture tests for the ClickHouse lake's
# only backup (ADR-0043 §2.1).
#
# The whole point of this snapshot is to be TRUSTWORTHY: it is the only
# copy of the lake's DDL, and the alert that watches it
# (stellarindex_ch_schema_snapshot_stale) reads exactly one signal —
# whether `last_success_unix` was refreshed. So the properties that
# matter are not "does it write a file" but:
#
#   1. a full capture writes every table's DDL and stamps success;
#   2. a database that answers with ZERO tables is REFUSED — an empty
#      snapshot overwriting a good one turns "we have a backup" into a
#      lie on the exact day the database was unreachable;
#   3. a PARTIAL capture (one SHOW CREATE errored) does NOT stamp
#      success — half a schema presented as fresh is the failure this
#      whole ADR was written after, and the staleness alert must fire.
#
# ClickHouse is stubbed with a fake `curl` on PATH, so this runs
# anywhere in about a second.
#
# Run: bash scripts/ops/ch-schema-snapshot-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SCRIPT="$PWD/scripts/ops/ch-schema-snapshot.sh"
[[ -r "$SCRIPT" ]] || { echo "ch-schema-snapshot-test: missing $SCRIPT" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

# ─── stub ClickHouse ────────────────────────────────────────────────
mkdir -p "$TMP/bin"
cat > "$TMP/bin/curl" <<'STUB'
#!/usr/bin/env bash
# Fake ClickHouse HTTP endpoint. Reads the query from --data-binary and
# answers from MOCK_TABLES / MOCK_SHOW_CREATE_FAILS.
q=""
while [[ $# -gt 0 ]]; do
  if [[ "$1" == "--data-binary" ]]; then q="$2"; shift 2; else shift; fi
done
case "$q" in
  *"SELECT name FROM system.tables"*)
    for t in ${MOCK_TABLES:-}; do echo "$t"; done ;;
  *"SHOW CREATE TABLE"*)
    for bad_t in ${MOCK_SHOW_CREATE_FAILS:-}; do
      [[ "$q" == *"\`$bad_t\`"* ]] && exit 22   # curl -f: HTTP error
    done
    echo "CREATE TABLE stellar.x (ledger_seq UInt32) ENGINE = ReplacingMergeTree ORDER BY ledger_seq" ;;
  *"FROM system.parts"*)    echo -e "table\tpartition\trows\tbytes\nledgers\t63\t1\t1" ;;
  *"FROM system.settings"*) echo -e "name\tvalue\nmax_threads\t4" ;;
  *"version()"*)            echo -e "version()\tuptime()\n24.3.1\t1000" ;;
  *"FROM system.tables"*)   echo -e "name\tengine\nledgers\tReplacingMergeTree" ;;
  *) echo "" ;;
esac
exit 0
STUB
chmod +x "$TMP/bin/curl"

# run_snapshot <case-name> — one isolated invocation.
run_snapshot() {
  local name="$1"
  rm -rf "$TMP/$name"
  mkdir -p "$TMP/$name/out" "$TMP/$name/textfile"
  PATH="$TMP/bin:$PATH" \
  OUT_DIR="$TMP/$name/out" \
  TEXTFILE_DIR="$TMP/$name/textfile" \
  BACKFILL_STATE="$TMP/$name/nonexistent-state" \
  STELLARINDEX_POSTGRES_DSN="" \
  SNAPSHOT_MC_TARGET="${MOCK_MC_TARGET:-}" \
  bash "$SCRIPT" >"$TMP/$name/stdout" 2>"$TMP/$name/stderr"
  echo "$?"
}
prom() { cat "$TMP/$1/textfile/ch_schema_snapshot.prom" 2>/dev/null; }
schema_of() { cat "$TMP/$1/out"/*/schema.sql 2>/dev/null; }

# ─── 1. full capture ────────────────────────────────────────────────
export MOCK_TABLES="ledgers transactions contract_events"
export MOCK_SHOW_CREATE_FAILS=""
rc="$(run_snapshot full)"

[[ "$rc" == "0" ]] \
  && ok "full capture exits 0" \
  || bad "full capture exited $rc (stderr: $(tail -2 "$TMP/full/stderr"))"

got_creates="$(grep -c '^CREATE TABLE' <<<"$(schema_of full)")"
[[ "$got_creates" == "3" ]] \
  && ok "full capture wrote DDL for all 3 tables" \
  || bad "full capture wrote $got_creates CREATE statements, want 3"

grep -q 'CREATE DATABASE IF NOT EXISTS stellar;' <<<"$(schema_of full)" \
  && ok "schema.sql is replayable from scratch (CREATE DATABASE first)" \
  || bad "schema.sql has no CREATE DATABASE — replaying it onto a bare server fails"

grep -q '^stellarindex_ch_schema_snapshot_last_success_unix [0-9]' <<<"$(prom full)" \
  && ok "full capture stamps last_success_unix (the staleness alert's only input)" \
  || bad "full capture did not stamp last_success_unix"

grep -q '^stellarindex_ch_schema_snapshot_tables 3$' <<<"$(prom full)" \
  && ok "full capture reports the table count it actually captured" \
  || bad "table-count gauge wrong: $(grep snapshot_tables <<<"$(prom full)")"

# ─── 2. zero tables is REFUSED ──────────────────────────────────────
export MOCK_TABLES=""
rc="$(run_snapshot empty)"

[[ "$rc" != "0" ]] \
  && ok "a zero-table answer exits non-zero ($rc)" \
  || bad "a zero-table answer exited 0 — an empty snapshot would be banked as a backup"

if grep -q 'last_success_unix' <<<"$(prom empty)"; then
  bad "a zero-table answer stamped success — the staleness alert would read a
       vanished schema as a fresh backup, which is the exact failure ADR-0043
       exists to prevent"
else
  ok "a zero-table answer does NOT stamp last_success_unix"
fi

# ─── 3. partial capture does not claim success ──────────────────────
export MOCK_TABLES="ledgers transactions contract_events"
export MOCK_SHOW_CREATE_FAILS="transactions"
rc="$(run_snapshot partial)"

[[ "$rc" != "0" ]] \
  && ok "a failed SHOW CREATE exits non-zero ($rc)" \
  || bad "a failed SHOW CREATE exited 0 — a schema missing a table would look complete"

if grep -q 'last_success_unix' <<<"$(prom partial)"; then
  bad "a partial capture stamped success — the lake would be 'protected' by a
       schema that is missing a table, with no signal"
else
  ok "a partial capture does NOT stamp last_success_unix (alert fires)"
fi

# ─── 4. offsite gauge: configured vs not, independent of push success ──
# backup-restore-2: the offsite staleness alert's absent-series branch is
# gated on offsite_configured, so it MUST be emitted every run (1 iff a
# target is set) — a push that never succeeded fires; an acked
# local-only host stays silent.
grep -q '^stellarindex_ch_schema_snapshot_offsite_configured 0$' <<<"$(prom full)" \
  && ok "no offsite target → offsite_configured 0 (acked local-only stays silent)" \
  || bad "no offsite target but offsite_configured != 0: $(grep offsite <<<"$(prom full)")"

export MOCK_TABLES="ledgers transactions contract_events"
export MOCK_SHOW_CREATE_FAILS=""
# $TMP/bin has no `mc`, so the push fails deterministically (exit 2).
rc="$(MOCK_MC_TARGET="offsite/stellarindex-backups/ch-schema" run_snapshot offsite)"

[[ "$rc" == "2" ]] \
  && ok "configured target + failed push exits 2" \
  || bad "configured target + failed push exited $rc, want 2"

grep -q '^stellarindex_ch_schema_snapshot_offsite_configured 1$' <<<"$(prom offsite)" \
  && ok "offsite target set → offsite_configured 1 even though the push failed" \
  || bad "offsite_configured missing/wrong with a target set: $(grep offsite <<<"$(prom offsite)")"

if grep -q 'offsite_last_success_unix [0-9]' <<<"$(prom offsite)"; then
  bad "a failed push stamped offsite_last_success — the offsite alert would read it as delivered"
else
  ok "a failed push does NOT stamp offsite_last_success (absent branch fires)"
fi

echo "ch-schema-snapshot-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]] || exit 1
