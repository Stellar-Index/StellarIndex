#!/usr/bin/env bash
# lint-ch-apply-scope-test.sh — fixture tests for the ClickHouse apply-scope
# gate (scripts/ci/lint-ch-apply-scope.sh).
#
# The gate exists because the archival-node role applied `deploy/clickhouse/
# *.sql` by glob wherever clickhouse_apply_schema is true (testnet.yml,
# futurenet.yml), so every fresh test-net provision ran the OPERATOR's
# migration artifacts unattended — including one whose header said "NOT
# auto-applied by any bootstrap" and one that says "FREEZE-GATED: do NOT
# run". The visible cost was two v2 cut-over tables plus their MVs, built as
# exact duplicates of their v1 tables on hosts that will never cut over:
# double MV write work and a second copy of the same rows, forever.
#
# So the verdicts pinned here are the ACTUAL shapes of that defect and of the
# ways it could come back, not invented ones:
#
#   - the REAL repository tree passes (the gate is not vacuously green);
#   - the PRE-FIX shape — no scope markers, no allow-list — is CAUGHT;
#   - a NEW unclassified file dropped into the directory is CAUGHT: this is
#     the property a "put migrations in a subdirectory" convention cannot
#     give, because dropping a file in the wrong directory fails OPEN;
#   - listing an OPERATOR artifact in the allow-list is CAUGHT — that is the
#     original defect, one file at a time;
#   - a fresh-host file MISSING from the allow-list is CAUGHT — the
#     opposite failure, an object a fresh host would never create;
#   - an operator artifact creating an object no fresh-host file declares is
#     CAUGHT (a new fresh-host table cannot hide inside a migration), and
#     declaring it si-cutover-object clears it;
#   - a mirror whose DDL has DRIFTED from the fresh-host copy is CAUGHT —
#     this is what makes "apply tier1_schema.sql alone" provable rather than
#     incidental;
#   - codifying a si-cutover-object in the fresh-host file is CAUGHT: a
#     completed cut-over renames v2 onto the base name, so codifying it
#     makes SUCCESS read as drift forever;
#   - a fresh-host file carrying an ALTER, or a CREATE without IF NOT
#     EXISTS, is CAUGHT — an unattended apply gets re-runnable DDL only;
#   - tier1_schema.sql not first in the list is CAUGHT (it CREATEs the
#     database);
#   - reintroducing the glob in the task file is CAUGHT, while the task
#     file's PROSE about the removed glob is not;
#   - an empty directory FAILS rather than passing vacuously.
#
# Run: bash scripts/ci/lint-ch-apply-scope-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-ch-apply-scope.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# run <root> — runs the gate over a fixture root; echoes output, returns exit.
run() {
  CH_DIR="$1/deploy/clickhouse" TASK_FILE="$1/task.yml" bash "$LINT" 2>&1
}

indent() { printf '%s\n' "         ${1//$'\n'/$'\n'         }"; }

# clean <desc> <root>
clean() {
  local desc="$1" root="$2" out got
  out="$(run "$root")"; got=$?
  if [ "$got" -eq 0 ]; then
    echo "  ok   $desc"; pass=$((pass + 1))
  else
    echo "  FAIL $desc (exit $got, want 0)"; indent "$out"
    fail=$((fail + 1))
  fi
}

# catches <desc> <root> <needle> — must fail AND say why, so a pass-by-
# accident on some other rule cannot masquerade as coverage.
catches() {
  local desc="$1" root="$2" needle="$3" out got
  out="$(run "$root")"; got=$?
  if [ "$got" -eq 0 ]; then
    echo "  FAIL $desc (exit 0, want non-zero)"; fail=$((fail + 1)); return
  fi
  # Substring test in-shell, not `… | grep -q`: #475 — an early-exit pipe
  # consumer under pipefail is a coin flip. The needle is quoted inside the
  # pattern, so its `*` and `?` are literal.
  case "$out" in
    *"$needle"*) : ;;
    *) echo "  FAIL $desc (failed, but no finding matching: $needle)"
       indent "$out"
       fail=$((fail + 1)); return ;;
  esac
  echo "  ok   $desc"; pass=$((pass + 1))
}

# ── Fixture builders ──────────────────────────────────────────────────────
# A minimal but REAL-SHAPED tree: one founding DDL declaring two objects, one
# operator mirror of one of them, and one operator cut-over artifact.
mk() { # mk <name> -> echoes the root
  local root="$TMP/$1"
  mkdir -p "$root/deploy/clickhouse"
  cat > "$root/task.yml" <<'YAML'
- name: Declare the fresh-host ClickHouse apply set
  ansible.builtin.set_fact:
    clickhouse_fresh_host_schema:
      - tier1_schema.sql

- name: Copy the fresh-host ClickHouse schema files
  ansible.builtin.copy:
    src: "{{ playbook_dir }}/../../../deploy/clickhouse/{{ item }}"
    dest: /tmp/ch-schema/
  loop: "{{ clickhouse_fresh_host_schema }}"
YAML
  cat > "$root/deploy/clickhouse/tier1_schema.sql" <<'SQL'
-- si-apply-scope: fresh-host
-- The founding DDL.
CREATE DATABASE IF NOT EXISTS stellar;

CREATE TABLE IF NOT EXISTS stellar.ledgers
(
    ledger_seq UInt32
)
ENGINE = MergeTree
ORDER BY ledger_seq;

CREATE TABLE IF NOT EXISTS stellar.ops_by_source
(
    source_account String
)
ENGINE = ReplacingMergeTree
ORDER BY source_account;
SQL
  cat > "$root/deploy/clickhouse/ops_by_source.sql" <<'SQL'
-- si-apply-scope: operator
-- The r1 hand-apply + backfill runbook. Same DDL as tier1's.
CREATE TABLE IF NOT EXISTS stellar.ops_by_source
(
    source_account String
)
ENGINE = ReplacingMergeTree
ORDER BY source_account;
SQL
  cat > "$root/deploy/clickhouse/ledgers_v2.sql" <<'SQL'
-- si-apply-scope: operator
-- si-cutover-object: stellar.ledgers_v2
-- The transient cut-over half; renamed onto stellar.ledgers at the end.
CREATE TABLE IF NOT EXISTS stellar.ledgers_v2
(
    ledger_seq UInt32
)
ENGINE = MergeTree
ORDER BY ledger_seq;
SQL
  echo "$root"
}

echo "lint-ch-apply-scope-test: fixture verdicts"

# 0. The real tree. If this ever goes red the gate is describing a repo that
#    does not exist, which is the failure mode a fixture-only suite hides.
if bash "$LINT" >/dev/null 2>&1; then
  echo "  ok   the real repository tree passes"; pass=$((pass + 1))
else
  echo "  FAIL the real repository tree does NOT pass"; bash "$LINT" 2>&1 | sed 's/^/         /'
  fail=$((fail + 1))
fi

# 1. Baseline fixture is clean.
BASE="$(mk base)"
clean "a classified tree with a matching allow-list passes" "$BASE"

# 2. THE PRE-FIX SHAPE: no markers anywhere, no allow-list, glob in the task.
R="$(mk prefix)"
for f in "$R"/deploy/clickhouse/*.sql; do
  grep -v '^-- si-' "$f" > "$f.tmp" && mv "$f.tmp" "$f"
done
cat > "$R/task.yml" <<'YAML'
- name: Copy ClickHouse schema files
  ansible.builtin.copy:
    src: "{{ item }}"
    dest: /tmp/ch-schema/
  with_fileglob:
    - "{{ playbook_dir }}/../../../deploy/clickhouse/*.sql"
YAML
catches "the pre-fix shape (unclassified files, no allow-list) is caught" "$R" \
  "declares no \`-- si-apply-scope:\` in its header"

# 3. A NEW file dropped in, unclassified. Fails CLOSED — the property a
#    directory convention cannot give.
R="$(mk newfile)"
cat > "$R/deploy/clickhouse/next_months_migration.sql" <<'SQL'
-- Somebody's new operator artifact, dropped in without a scope.
ALTER TABLE stellar.ledgers ADD COLUMN IF NOT EXISTS extra UInt8 DEFAULT 0;
SQL
catches "a new unclassified artifact is caught" "$R" "next_months_migration.sql declares no"

# 4. THE ORIGINAL DEFECT, one file at a time: an operator artifact listed in
#    the fresh-host allow-list.
R="$(mk listed_operator)"
sed -i.bak 's/      - tier1_schema.sql/      - tier1_schema.sql\n      - ledgers_v2.sql/' "$R/task.yml"
catches "an operator artifact in the allow-list is caught" "$R" \
  "names ledgers_v2.sql, whose si-apply-scope is 'operator'"

# 5. The opposite: a fresh-host file the allow-list forgot.
R="$(mk unlisted_fresh)"
sed -i.bak 's/^-- si-apply-scope: operator$/-- si-apply-scope: fresh-host/' \
  "$R/deploy/clickhouse/ops_by_source.sql"
catches "a fresh-host file missing from the allow-list is caught" "$R" \
  "is si-apply-scope: fresh-host but is NOT in clickhouse_fresh_host_schema"

# 6. A genuinely-new fresh-host object hiding inside an operator artifact.
R="$(mk hidden_object)"
cat >> "$R/deploy/clickhouse/ops_by_source.sql" <<'SQL'

CREATE TABLE IF NOT EXISTS stellar.brand_new_thing
(
    k String
)
ENGINE = MergeTree
ORDER BY k;
SQL
catches "an operator-only object no fresh-host file declares is caught" "$R" \
  "creates stellar.brand_new_thing, which no fresh-host file creates"

# 6b. …and declaring it a cut-over half clears it.
sed -i.bak 's/^-- si-apply-scope: operator$/-- si-apply-scope: operator\n-- si-cutover-object: stellar.brand_new_thing/' \
  "$R/deploy/clickhouse/ops_by_source.sql"
clean "declaring that object si-cutover-object clears it" "$R"

# 7. DRIFT between a mirror and the founding DDL. This is what makes
#    "apply tier1_schema.sql alone" provable instead of incidental.
R="$(mk drift)"
sed -i.bak 's/^    source_account String$/    source_account String,\n    added_later UInt8 DEFAULT 0/' \
  "$R/deploy/clickhouse/ops_by_source.sql"
catches "a mirror drifting from the founding DDL is caught" "$R" \
  "has DRIFTED from the fresh-host one"

# 8. Codifying a cut-over target in the founding DDL.
R="$(mk codified_cutover)"
cat >> "$R/deploy/clickhouse/tier1_schema.sql" <<'SQL'

CREATE TABLE IF NOT EXISTS stellar.ledgers_v2
(
    ledger_seq UInt32
)
ENGINE = MergeTree
ORDER BY ledger_seq;
SQL
catches "codifying a cut-over target in the founding DDL is caught" "$R" \
  "but a fresh-host file also creates it"

# 9. A mutating verb in a fresh-host file.
R="$(mk fresh_alter)"
cat >> "$R/deploy/clickhouse/tier1_schema.sql" <<'SQL'

ALTER TABLE stellar.ledgers DROP INDEX idx_gone;
SQL
catches "an ALTER in a fresh-host file is caught" "$R" \
  "is fresh-host but contains non-CREATE statement(s)"

# 10. A non-re-runnable CREATE in a fresh-host file.
R="$(mk fresh_no_ine)"
cat >> "$R/deploy/clickhouse/tier1_schema.sql" <<'SQL'

CREATE TABLE stellar.not_rerunnable
(
    k String
)
ENGINE = MergeTree
ORDER BY k;
SQL
catches "a fresh-host CREATE without IF NOT EXISTS is caught" "$R" \
  "without IF NOT EXISTS"

# 11. tier1 not first — the database would not exist yet.
R="$(mk order)"
sed -i.bak 's/^-- si-apply-scope: operator$/-- si-apply-scope: fresh-host/' \
  "$R/deploy/clickhouse/ops_by_source.sql"
cat > "$R/task.yml" <<'YAML'
- name: Declare the fresh-host ClickHouse apply set
  ansible.builtin.set_fact:
    clickhouse_fresh_host_schema:
      - ops_by_source.sql
      - tier1_schema.sql
YAML
catches "tier1_schema.sql not first in the allow-list is caught" "$R" \
  "not tier1_schema.sql"

# 12. The glob, reintroduced.
R="$(mk reglob)"
cat >> "$R/task.yml" <<'YAML'

- name: Copy ClickHouse schema files
  ansible.builtin.copy:
    src: "{{ item }}"
    dest: /tmp/ch-schema/
  with_fileglob:
    - "{{ playbook_dir }}/../../../deploy/clickhouse/*.sql"
YAML
# CH_DIR is the repo's own relative path here on purpose: rule 7 looks for a
# live "<CH_DIR>/*" in the task file, which is exactly the shipped glob's text.
if out="$(CH_DIR=deploy/clickhouse TASK_FILE="$R/task.yml" bash "$LINT" 2>&1)"; then
  echo "  FAIL reintroducing the glob in the task file is NOT caught (exit 0)"
  indent "$out"; fail=$((fail + 1))
else
  case "$out" in
    *"has a live deploy/clickhouse/* glob"*)
      echo "  ok   reintroducing the glob in the task file is caught"; pass=$((pass + 1)) ;;
    *)
      echo "  FAIL reintroducing the glob failed the gate, but on some other rule"
      indent "$out"; fail=$((fail + 1)) ;;
  esac
fi

# 12b. …but the task file's PROSE about the removed glob is not flagged. The
#      shipped task file explains the defect at length; a gate that fires on
#      its own rationale is a gate somebody deletes.
R="$(mk globprose)"
cat > "$R/task.yml" <<'YAML'
# This used to copy deploy/clickhouse/*.sql with with_fileglob and execute
# every file it found. That directory is not a bootstrap manifest.
- name: Declare the fresh-host ClickHouse apply set
  ansible.builtin.set_fact:
    clickhouse_fresh_host_schema:
      - tier1_schema.sql
YAML
clean "prose describing the removed glob is not flagged" "$R"

# 13. An empty directory must FAIL, not pass vacuously.
R="$TMP/empty"
mkdir -p "$R/deploy/clickhouse"
cp "$BASE/task.yml" "$R/task.yml"
out="$(run "$R")"; got=$?
case "${got}:${out}" in
  0:*) echo "  FAIL an empty schema directory passed vacuously (exit 0)"; fail=$((fail + 1)) ;;
  *"gate must not pass vacuously"*)
    echo "  ok   an empty schema directory fails rather than passing vacuously"; pass=$((pass + 1)) ;;
  *) echo "  FAIL an empty schema directory failed, but not on the vacuity guard (exit $got)"
     indent "$out"; fail=$((fail + 1)) ;;
esac

echo "lint-ch-apply-scope-test: ${pass} passed, ${fail} failed (of $((pass + fail)) checks)"
[ "$fail" -eq 0 ]
