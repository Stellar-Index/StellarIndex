#!/usr/bin/env bash
# config-apply-gate-test.sh — fixture tests for the deploy-time
# config-apply gate.
#
# config-apply-gate.sh is what stops a binary-only deploy from silently
# shipping a feature whose config half never landed (2026-08-25
# declared-peg + rules.d). Its SURFACES list IS the gate: a config path
# it does not name is a path it will never flag. That list is pinned
# here rather than assumed, because on 2026-08-28 a node_exporter probe
# + its systemd units shipped as inline `content:` blocks in
# tasks/10-observability.yml — under tasks/, which the list did not
# cover — and only a hand check noticed the gate would have passed it.
#
#   - a release that changed no surface passes;
#   - a diff under roles/archival-node/templates/ fails un-acknowledged
#     and passes with config_acknowledged=true;
#   - a diff under roles/archival-node/tasks/ (the 2026-08-28 hole)
#     fails un-acknowledged;
#   - a diff under roles/archival-node/files/ fails un-acknowledged;
#   - (audit deploy-ansible-gate-4, 2026-08-28) a diff under
#     roles/archival-node/defaults/, handlers/, inventory/, a sibling
#     role, configs/healthchecks/, or one of the repo scripts the role
#     copies onto the host fails un-acknowledged — each of these renders
#     or lands on the host and none is applied by deploy-binary.yml;
#   - a diff that only touches Go source, or only the deploy playbook
#     the deploy itself runs, is NOT a config surface;
#   - a skip-ahead deploy diffs against the host-reported baseline (3rd
#     arg) — ancestry alone misses config changed in the skipped tags;
#   - an unresolvable version or baseline, i.e. a diff the gate cannot
#     see, FAILS rather than passing as "no changes" (fail-closed);
#   - a first release (no prior tag) skips rather than failing.
#
# Since 2026-09-07 it also covers the two OTHER refusals the deploy path
# gained, because they share this file's registration in ci.yml and
# verify.sh: the gate's three-way classification (comment-only /
# already-applied / substantive) and deploy.yml's per-region binary
# manifest. Each has its own banner below.
#
# Run: bash scripts/ci/config-apply-gate-test.sh
set -uo pipefail

# `git init` honours an INHERITED GIT_DIR ahead of its own `-C`, and a git
# hook exports GIT_DIR/GIT_INDEX_FILE. lint-changed dispatches test scripts,
# and the pre-commit hook runs lint-changed — so without this a fixture's
# init re-initialises the REAL repository: core.bare set on the live
# checkout, fixture commits on main, and git says only "warning: re-init".
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
      GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR

cd "$(dirname "$0")/../.." || exit 1
GATE="$PWD/scripts/ci/config-apply-gate.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# The container lane materialises the tree with `git archive HEAD | tar -x`, which
# carries no .git at all, so tag-anchored corroboration cannot run there. That is an
# environment fact, not a passing result: the classifier's own behaviour is pinned by
# the fixture assertions in both directions, and any shortfall is counted and printed.
# Probe for TAGS, not for a .git: prepush's container lane clones with
# `git clone --no-local`, which yields a repository carrying no tags at all, so
# "a git dir exists" answers the wrong question and reports history=yes over a
# tree where no release tag can resolve. If some release tags exist but the ones
# these assertions name do not, that is a real failure and still fails.
has_history=no
release_tags=""
if git rev-parse --git-dir >/dev/null 2>&1; then
  # No pipe into head: `git tag --list | head` is an early-exit consumer under
  # pipefail, which lint-shell-sigpipe rejects. Capture and test the string.
  release_tags="$(git tag --list 'v[0-9]*' 2>/dev/null || true)"
fi
[ -n "$release_tags" ] && has_history=yes
skipped=0
pass=0
fail=0

# mkrepo — a throwaway repo with one tagged release (v0.1.0) carrying a
# representative file on every surface, so a later edit reads as a
# change to that surface rather than a brand-new path.
mkrepo() {
  rm -rf "$TMP/repo"
  mkdir -p "$TMP/repo"
  (
    cd "$TMP/repo" || exit 1
    git init -q .
    git config user.email t@t.invalid
    git config user.name t
    git config commit.gpgsign false
    git config tag.gpgsign false
    mkdir -p configs/ansible/roles/archival-node/{templates,tasks,files,defaults,handlers} \
      configs/ansible/roles/prometheus/templates configs/ansible/inventory \
      configs/ansible/playbooks configs/healthchecks scripts/ops scripts/dev \
      configs/prometheus/rules.r1 deploy/monitoring/rules deploy/systemd \
      deploy/clickhouse internal/x
    printf 'v1\n' > configs/ansible/roles/archival-node/templates/stellarindex.toml.j2
    printf 'v1\n' > configs/ansible/roles/archival-node/tasks/10-observability.yml
    printf 'v1\n' > configs/ansible/roles/archival-node/files/node-healthcheck.sh
    printf 'v1\n' > configs/ansible/roles/archival-node/defaults/main.yml
    printf 'v1\n' > configs/ansible/roles/archival-node/handlers/main.yml
    printf 'v1\n' > configs/ansible/roles/prometheus/templates/prometheus.yml.j2
    printf 'v1\n' > configs/ansible/inventory/r1.yml
    printf 'v1\n' > configs/ansible/playbooks/deploy-binary.yml
    printf 'v1\n' > configs/healthchecks/smoke.sh
    printf 'v1\n' > scripts/ops/config-assertions.sh
    printf 'v1\n' > scripts/dev/r1-smoke.sh
    printf 'v1\n' > deploy/systemd/stellarindex-api.service
    printf 'package x\n' > internal/x/x.go
    # A ClickHouse surface with a real statement in it, so a later edit
    # reads as a CHANGE to an existing object rather than a new file.
    cat > deploy/clickhouse/schema.sql <<'FIXTURE_SQL'
-- Fixture tier-1 schema.
CREATE TABLE IF NOT EXISTS stellar.ledgers
(
    ledger_seq UInt32,
    close_time DateTime
)
ENGINE = MergeTree
ORDER BY ledger_seq;
FIXTURE_SQL
    # A surface file whose comment convention is NOT known.
    printf 'v1\n' > configs/healthchecks/notes.txt
    git add -A
    git commit -qm "base"
    git tag v0.1.0
  )
}

# releaseAs <tag> <path> — edit <path>, commit, tag <tag>.
releaseAs() {
  (
    cd "$TMP/repo" || exit 1
    printf '%s\n' "$1" > "$2"
    git add -A
    git commit -qm "change $2"
    git tag "$1"
  )
}

# release <path> — edit <path>, commit, tag v0.2.0 (the deploying version).
release() {
  releaseAs v0.2.0 "$1"
}

# appendAs <tag> <path> — append stdin to <path>, commit, tag <tag>. The
# classification cases need a diff of a KNOWN shape (comments only, a
# whole new statement, a line inserted mid-body), which overwriting a
# one-line fixture cannot produce.
appendAs() {
  local tag="$1" path="$2"
  (
    cd "$TMP/repo" || exit 1
    cat >> "$path"
    git add -A
    git commit -qm "append $path"
    git tag "$tag"
  )
}

# editAs <tag> <path> <sed-expr> — apply <sed-expr> to <path>, commit, tag.
editAs() {
  (
    cd "$TMP/repo" || exit 1
    sed -i.bak -E "$3" "$2" && rm -f "$2.bak"
    git add -A
    git commit -qm "edit $2"
    git tag "$1"
  )
}

# runGate <version> [ack] → sets RC + OUT. GITHUB_STEP_SUMMARY is pointed
# at a scratch file so the summary block does not pollute OUT.
runGate() {
  OUT="$(cd "$TMP/repo" && GITHUB_STEP_SUMMARY="$TMP/summary" bash "$GATE" "$1" "${2:-false}" "${3:-}" "${4:-}" 2>&1)"
  RC=$?
}

# expect <name> <want-rc> [want-substring]
expect() {
  local name="$1" want_rc="$2" want_sub="${3:-}"
  if [[ "$RC" -ne "$want_rc" ]]; then
    echo "FAIL: $name — rc=$RC want=$want_rc"
    echo "$OUT" | sed -n '1,12s/^/    | /p'
    fail=$((fail + 1))
    return
  fi
  if [[ -n "$want_sub" && "$OUT" != *"$want_sub"* ]]; then
    echo "FAIL: $name — output missing: $want_sub"
    echo "$OUT" | sed -n '1,12s/^/    | /p'
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

# --- 1. no surface changed passes -----------------------------------
mkrepo
release internal/x/x.go
runGate v0.2.0
expect "Go-only release is not a config surface" 0 "no config-surface changes"

# --- 2. templates/ change fails un-acked, passes acked --------------
mkrepo
release configs/ansible/roles/archival-node/templates/stellarindex.toml.j2
runGate v0.2.0
expect "templates/ change fails un-acknowledged" 1 "changed 1 config surface(s)"
runGate v0.2.0 true
expect "templates/ change passes with config_acknowledged=true" 0 "config-apply acknowledged"

# --- 3. THE 2026-08-28 HOLE: tasks/ carries inline config ----------
mkrepo
release configs/ansible/roles/archival-node/tasks/10-observability.yml
runGate v0.2.0
expect "tasks/ change fails un-acknowledged" 1 "changed 1 config surface(s)"

# --- 4. files/ carries the role's scripts ---------------------------
mkrepo
release configs/ansible/roles/archival-node/files/node-healthcheck.sh
runGate v0.2.0
expect "files/ change fails un-acknowledged" 1 "changed 1 config surface(s)"

# --- 5. a non-ansible surface (systemd) is still covered -----------
mkrepo
release deploy/systemd/stellarindex-api.service
runGate v0.2.0
expect "deploy/systemd/ change fails un-acknowledged" 1 "changed 1 config surface(s)"

# --- 6. first release (no prior tag) skips ---------------------------
mkrepo
runGate v0.1.0
expect "no previous tag skips" 0 "skipping config-drift gate"

# --- 7. deploy-ansible-gate-4: surfaces the role renders/copies that the
#        list did not name. One fixture per added entry; a defaults-only
#        release (e.g. a galexie_ledgers_per_file bump that re-renders
#        galexie.toml) used to pass as "binary deploy is complete".
for p in \
  configs/ansible/roles/archival-node/defaults/main.yml \
  configs/ansible/roles/archival-node/handlers/main.yml \
  configs/ansible/roles/prometheus/templates/prometheus.yml.j2 \
  configs/ansible/inventory/r1.yml \
  configs/healthchecks/smoke.sh \
  scripts/ops/config-assertions.sh \
  scripts/dev/r1-smoke.sh; do
  mkrepo
  release "$p"
  runGate v0.2.0
  expect "$p change fails un-acknowledged" 1 "changed 1 config surface(s)"
done

# --- 8. the deploy playbook is what the deploy RUNS — applied, not dead;
#        listing it would make every deploy-mechanics change a false ack.
mkrepo
release configs/ansible/playbooks/deploy-binary.yml
runGate v0.2.0
expect "playbooks/deploy-binary.yml change is not a config surface" 0 "no config-surface changes"

# --- 9. skip-ahead: host is on v0.1.0, v0.2.0 changed defaults/, we deploy
#        v0.3.0 (Go-only). Ancestry diffs v0.2.0..v0.3.0 and sees nothing;
#        the host-reported baseline sees the v0.2.0 config change.
mkrepo
releaseAs v0.2.0 configs/ansible/roles/archival-node/defaults/main.yml
releaseAs v0.3.0 internal/x/x.go
runGate v0.3.0 false v0.1.0
expect "skip-ahead deploy fails against host baseline" 1 "changed 1 config surface(s)"
expect "skip-ahead names the host baseline" 1 "baseline: v0.1.0 (host-reported live version)"
runGate v0.3.0 true v0.1.0
expect "skip-ahead passes with config_acknowledged=true" 0 "config-apply acknowledged"
runGate v0.3.0
expect "ancestry baseline is printed when no host version is given" 0 "baseline: v0.2.0 (previous release tag by ancestry"

# --- 10. fail-closed: a diff the gate cannot see is an error, not a green.
mkrepo
release internal/x/x.go
runGate vNOPE
expect "unresolvable version fails closed" 1 "does not resolve to a commit"
runGate v0.2.0 false vNOPE
expect "unresolvable host baseline fails closed" 1 "does not resolve to a commit"

# --- 11. the [applied] argument: a self-applying surface is retired
# from the gate WITHOUT weakening it for anything else.
#
# configs/prometheus/rules.r1/ became self-applying (apply-rules.sh runs
# in the deploy job, and VERIFIES against /api/v1/rules rather than just
# copying). The risk of that exemption is that it is written too broadly
# and silently clears surfaces nobody applied — which would restore the
# exact 2026-09-01 failure the gate exists to catch. So the cases that
# matter are the negative ones: the exemption must not leak.
mkrepo
release configs/prometheus/rules.r1/storage.yml
runGate v0.2.0 false "" "configs/prometheus/rules.r1/"
expect "an auto-applied rules change needs no operator ack" 0 "applied and verified automatically"

# The same change WITHOUT the applied arg (e.g. a region whose deploy
# does not run the applier) must still gate. Passing here would mean the
# exemption is unconditional.
mkrepo
release configs/prometheus/rules.r1/storage.yml
runGate v0.2.0
expect "the same change still gates when nothing applied it" 1 "changed 1 config surface(s)"

# The exemption must not cover a DIFFERENT surface. rules.r1 being
# auto-applied says nothing about the multi-host rule tree, the ansible
# role, or alertmanager.
mkrepo
release deploy/monitoring/rules/storage.yml
runGate v0.2.0 false "" "configs/prometheus/rules.r1/"
expect "the exemption does not cover deploy/monitoring/rules/" 1 "changed 1 config surface(s)"

mkrepo
release configs/ansible/roles/archival-node/templates/stellarindex.toml.j2
runGate v0.2.0 false "" "configs/prometheus/rules.r1/"
expect "the exemption does not cover the ansible role" 1 "changed 1 config surface(s)"

# A release touching BOTH an applied and an unapplied surface must still
# fail — and report only the surface that is actually outstanding. This
# is the mixed case that a naive "any applied ⇒ pass" would get wrong.
mkrepo
releaseAs v0.2.0 configs/prometheus/rules.r1/storage.yml
releaseAs v0.3.0 configs/ansible/roles/archival-node/templates/stellarindex.toml.j2
runGate v0.3.0 false v0.1.0 "configs/prometheus/rules.r1/"
expect "a mixed release still gates on the surface nobody applied" 1 "changed 1 config surface(s)"
# The gate must ATTRIBUTE each surface correctly, not merely fail: the
# auto-applied one under the "applied and VERIFIED" banner, and the
# outstanding count must be 1 (the ansible template), not 2. A gate that
# failed with a count of 2 would be telling the operator to go apply a
# surface this deploy already applied and proved live.
if [[ "$OUT" != *"VERIFIED automatically"* ]]; then
  echo "FAIL: mixed release did not report the auto-applied surface as applied"
  echo "$OUT" | sed -n '1,8s/^/    | /p'
  fail=$((fail + 1))
elif [[ "$OUT" != *"changed 1 config surface(s)"* ]]; then
  echo "FAIL: mixed release miscounted the outstanding surfaces (want exactly 1)"
  echo "$OUT" | sed -n '1,8s/^/    | /p'
  fail=$((fail + 1))
elif [[ "$OUT" == *"VERIFIED automatically"*"stellarindex.toml.j2"* ]]; then
  echo "FAIL: the auto-applied banner claimed a surface this deploy never applied"
  echo "$OUT" | sed -n '1,8s/^/    | /p'
  fail=$((fail + 1))
else
  echo "ok: mixed release attributes each surface to the right column"
  pass=$((pass + 1))
fi

# The applied prefix is an anchored PATH PREFIX, not a substring. A
# caller passing a loose token must not exempt a sibling tree: "rules"
# must not clear deploy/monitoring/rules/. Without anchoring, grep -F
# matches mid-path and the exemption silently widens to a surface
# nothing applied.
mkrepo
release deploy/monitoring/rules/storage.yml
runGate v0.2.0 false "" "rules"
expect "a loose prefix does not exempt a sibling rule tree" 1 "changed 1 config surface(s)"


# ═══ Three-way classification: comment-only / applied / substantive ═══
#
# Until 2026-09-07 the gate knew only "changed" and "unchanged", so every
# diff was an operator decision. Two of that day's four failed deploys were
# spent discovering that a deploy/clickhouse/*.sql diff was comment-only.
# The rule generalised from that — "comment-only, so acknowledge" — is
# FALSE: v0.61.1..v0.62.0 added CREATE TABLE stellar.account_creators_ops
# to account_creators_rollup.sql AND tier1_schema.sql, and the
# acknowledgement was right only because the DDL had been applied by hand
# first. Both halves are pinned here: the comment-only diff must pass
# WITHOUT an acknowledgement, and the DDL diff dressed in the same comments
# must still block.

# --- 12. a comment-only diff is not an operator decision ---------------
mkrepo
appendAs v0.2.0 deploy/clickhouse/schema.sql <<'SQL'

-- Why this table is partitioned the way it is: one 1M-ledger range per
-- part, so a walked pass costs one partition rather than the archive.
SQL
runGate v0.2.0
expect "a comment-only ClickHouse diff passes with no acknowledgement" 0 "COMMENT-ONLY"

mkrepo
appendAs v0.2.0 configs/ansible/roles/archival-node/tasks/10-observability.yml <<'YML'

# Why this probe exists, and what it would mean if it stopped reporting.
YML
runGate v0.2.0
expect "a comment-only ansible-tasks diff passes (the '#' marker)" 0 "COMMENT-ONLY"

# --- 13. THE 2026-09-07 TRAP: DDL wearing a comment-only costume -------
#
# The shape of the real v0.61.1..v0.62.0 diff: several paragraphs of new
# comment and, at the end of them, a CREATE TABLE. Anyone applying
# "comment-only" as a rule of thumb acknowledges this and ships the schema
# change unapplied.
mkrepo
appendAs v0.2.0 deploy/clickhouse/schema.sql <<'SQL'

-- WHY THE CYCLE WALKS PARTITIONS. As one statement the aggregation held
-- two things at once whose sizes are set by different populations, and
-- the pair summed past the budget.
CREATE TABLE IF NOT EXISTS stellar.account_creators_ops
(
    creator String,
    created String,
    ledger  UInt32
)
ENGINE = MergeTree
ORDER BY (creator, ledger);
SQL
runGate v0.2.0
expect "DDL introduced under new comments is SUBSTANTIVE and still blocks" 1 "changed 1 config surface(s)"
runGate v0.2.0 true
expect "the same DDL passes only when the operator acknowledges it" 0 "config-apply acknowledged"

# --- 14. mixed release: one comment-only, one substantive --------------
#
# The count must be the SUBSTANTIVE one, not the total: telling an operator
# to go apply a comment is how a real one gets skimmed past.
mkrepo
appendAs v0.2.0 deploy/clickhouse/schema.sql <<'SQL'

-- A note, and nothing else.
SQL
appendAs v0.3.0 configs/ansible/roles/archival-node/templates/stellarindex.toml.j2 <<'TOML'
[pricing_guard]
declared_pegs = ["AUDD"]
TOML
runGate v0.3.0 false v0.1.0
expect "a mixed release blocks on the substantive surface only" 1 "changed 1 config surface(s)"
if [[ "$OUT" == *"COMMENT-ONLY"*"schema.sql"* ]]; then
  echo "ok: the comment-only half is reported as comment-only, not as outstanding"
  pass=$((pass + 1))
else
  echo "FAIL: the comment-only half was not attributed to the comment-only column"
  echo "$OUT" | sed -n '1,12s/^/    | /p'
  fail=$((fail + 1))
fi

# --- 15. an unrecognised file type is substantive, comments or not -----
mkrepo
appendAs v0.2.0 configs/healthchecks/notes.txt <<'TXT'
# this looks like a comment, but nothing here knows that it is one
TXT
runGate v0.2.0
expect "a file type with no known comment convention stays substantive" 1 "changed 1 config surface(s)"

# --- 16. --payload: the classifier the callers share -------------------
mkrepo
appendAs v0.2.0 deploy/clickhouse/schema.sql <<'SQL'

-- Only a comment.
SQL
payload="$(cd "$TMP/repo" && bash "$GATE" --payload v0.1.0 v0.2.0 deploy/clickhouse/schema.sql)"
if [[ -z "$payload" ]]; then
  echo "ok: --payload is empty for a comment-only diff"; pass=$((pass + 1))
else
  echo "FAIL: --payload reported a comment-only diff as substantive: $payload"; fail=$((fail + 1))
fi

mkrepo
appendAs v0.2.0 deploy/clickhouse/schema.sql <<'SQL'

-- A comment, then a statement.
CREATE TABLE IF NOT EXISTS stellar.account_creators_ops (creator String) ENGINE = MergeTree ORDER BY creator;
SQL
payload="$(cd "$TMP/repo" && bash "$GATE" --payload v0.1.0 v0.2.0 deploy/clickhouse/schema.sql)"
if [[ "$payload" == *"CREATE TABLE"* ]]; then
  echo "ok: --payload reports the statement and not the comments around it"; pass=$((pass + 1))
else
  echo "FAIL: --payload missed the statement: '$payload'"; fail=$((fail + 1))
fi

# --- 17. --ddl-objects: what "already applied" may be asked about ------
#
# Object existence proves a diff applied ONLY when the diff is whole new
# statements. The cases below are the boundary, and case (b) is the one
# that would silently certify an unapplied change: adding a column inside
# a `CREATE TABLE IF NOT EXISTS` leaves the object present either way, and
# re-running the file would not add the column.

# (a) a whole new statement — verifiable, and it names the object.
mkrepo
appendAs v0.2.0 deploy/clickhouse/schema.sql <<'SQL'

CREATE TABLE IF NOT EXISTS stellar.account_creators_ops
(
    creator String
)
ENGINE = MergeTree
ORDER BY creator;
SQL
if objects="$(cd "$TMP/repo" && bash "$GATE" --ddl-objects v0.1.0 v0.2.0 deploy/clickhouse/schema.sql)" \
   && [[ "$objects" == "stellar.account_creators_ops" ]]; then
  echo "ok: --ddl-objects names the object a whole new CREATE adds"; pass=$((pass + 1))
else
  echo "FAIL: --ddl-objects on a whole new CREATE gave '$objects'"; fail=$((fail + 1))
fi

# (b) a column added inside an existing CREATE TABLE IF NOT EXISTS.
mkrepo
editAs v0.2.0 deploy/clickhouse/schema.sql 's/^    ledger_seq UInt32,$/    ledger_seq UInt32,\n    tx_count   UInt32,/'
if objects="$(cd "$TMP/repo" && bash "$GATE" --ddl-objects v0.1.0 v0.2.0 deploy/clickhouse/schema.sql)"; then
  echo "FAIL: --ddl-objects certified a column added inside an existing CREATE TABLE ('$objects') — the object exists either way, so existence proves nothing"
  fail=$((fail + 1))
else
  echo "ok: a column added mid-body is NOT verifiable by object existence"; pass=$((pass + 1))
fi

# (c) an ALTER is not verifiable by existence either.
mkrepo
appendAs v0.2.0 deploy/clickhouse/schema.sql <<'SQL'

ALTER TABLE stellar.ledgers ADD COLUMN tx_count UInt32;
SQL
if (cd "$TMP/repo" && bash "$GATE" --ddl-objects v0.1.0 v0.2.0 deploy/clickhouse/schema.sql) >/dev/null 2>&1; then
  echo "FAIL: --ddl-objects certified an ALTER"; fail=$((fail + 1))
else
  echo "ok: an ALTER is not verifiable by object existence"; pass=$((pass + 1))
fi

# (d) a removed statement is not verifiable — existence cannot see a drop.
mkrepo
editAs v0.2.0 deploy/clickhouse/schema.sql '/^ORDER BY ledger_seq;$/d'
if (cd "$TMP/repo" && bash "$GATE" --ddl-objects v0.1.0 v0.2.0 deploy/clickhouse/schema.sql) >/dev/null 2>&1; then
  echo "FAIL: --ddl-objects certified a diff that REMOVED a substantive line"; fail=$((fail + 1))
else
  echo "ok: a diff that removes a substantive line is not verifiable"; pass=$((pass + 1))
fi

# --- 18. the [applied] channel carries that evidence -------------------
#
# deploy.yml's ClickHouse step passes a file path here only after every
# object the diff creates was found in system.tables. The gate must then
# clear that file — and must NOT clear a sibling nobody verified.
mkrepo
appendAs v0.2.0 deploy/clickhouse/schema.sql <<'SQL'

CREATE TABLE IF NOT EXISTS stellar.account_creators_ops (creator String) ENGINE = MergeTree ORDER BY creator;
SQL
runGate v0.2.0 false "" "deploy/clickhouse/schema.sql"
expect "a DDL surface proven present on the host needs no acknowledgement" 0 "VERIFIED automatically"
mkrepo
appendAs v0.2.0 deploy/clickhouse/schema.sql <<'SQL'

CREATE TABLE IF NOT EXISTS stellar.account_creators_ops (creator String) ENGINE = MergeTree ORDER BY creator;
SQL
appendAs v0.3.0 deploy/systemd/stellarindex-api.service <<'UNIT'
Restart=always
UNIT
runGate v0.3.0 false v0.1.0 "deploy/clickhouse/schema.sql"
expect "verified DDL does not clear a systemd unit nobody can check" 1 "changed 1 config surface(s)"

# --- 19. THE REAL RANGE: v0.61.1 → v0.62.0 -----------------------------
#
# The fixtures above are shaped like the incident; this IS the incident.
# Run against the repository's own tags so the classification is pinned to
# real content rather than to a reconstruction of it.
if git rev-parse -q --verify 'v0.61.1^{commit}' >/dev/null \
   && git rev-parse -q --verify 'v0.62.0^{commit}' >/dev/null; then
  real_payload="$(bash "$GATE" --payload v0.61.1 v0.62.0 deploy/clickhouse/account_sponsors_rollup.sql)"
  if [[ -z "$real_payload" ]]; then
    echo "ok: account_sponsors_rollup.sql really is comment-only over v0.61.1..v0.62.0"; pass=$((pass + 1))
  else
    echo "FAIL: account_sponsors_rollup.sql classified substantive over the real range"; fail=$((fail + 1))
  fi
  real_bad=0
  for f in deploy/clickhouse/account_creators_rollup.sql deploy/clickhouse/tier1_schema.sql; do
    [[ -n "$(bash "$GATE" --payload v0.61.1 v0.62.0 "$f")" ]] || real_bad=1
    objs="$(bash "$GATE" --ddl-objects v0.61.1 v0.62.0 "$f")" || real_bad=1
    [[ "$objs" == "stellar.account_creators_ops" ]] || real_bad=1
  done
  if [[ "$real_bad" -eq 0 ]]; then
    echo "ok: both real DDL files are substantive AND name stellar.account_creators_ops for the host check"
    pass=$((pass + 1))
  else
    echo "FAIL: the real v0.61.1..v0.62.0 DDL files did not classify as substantive-and-verifiable"
    fail=$((fail + 1))
  fi
else
  if [ "$has_history" = no ]; then
    echo "NOTE: v0.61.1 / v0.62.0 unreachable — no git history in this tree, so the real-range corroboration did not run here"
    skipped=$((skipped + 1))
  else
    echo "FAIL: v0.61.1 / v0.62.0 do not resolve here, so the real-range assertions did NOT run (git fetch --tags origin)"
    fail=$((fail + 1))
  fi
fi

# ─── The CALLER must pass the baseline ──────────────────────────────
#
# Every case above exercises the SCRIPT, which has always handled a 3rd
# argument correctly. The defect was that .github/workflows/deploy.yml
# called it with TWO — so the gate diffed against "the previous release
# tag by ancestry" instead of the host's live version. That is only the
# same thing when the fleet is exactly one release behind, and
# deployed-versions.md states plainly that a tag cut does not imply the
# fleet moved to it.
#
# Measured on real tags: the 2-arg form reports "no config-surface
# changes between v0.47.1 and v0.47.2 — the binary deploy is complete"
# and exits 0, while the 3-arg form against a real host baseline of
# v0.45.0 finds THIRTEEN changed config surfaces and exits 1 (wave-D
# LID-5).
#
# A script-level test cannot catch a caller-level omission, so check the
# caller.
check_caller() {
  local desc="$1" cond="$2"
  if [[ "$cond" == "ok" ]]; then
    printf 'ok: %s\n' "$desc"; pass=$((pass + 1))
  else
    printf 'FAIL: %s\n' "$desc"; fail=$((fail + 1))
  fi
}

# The cases above run inside a throwaway fixture repo, so resolve the
# workflow from THIS script's own location, not the working directory.
WF="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/.github/workflows/deploy.yml"
if [[ ! -f "$WF" ]]; then
  check_caller "deploy workflow present" "no"
else
  # EXECUTED, not merely mentioned — the distinction check-verify-parity.sh's
  # own extractor makes. deploy.yml also NAMES the script in an `env:`
  # assignment (the ClickHouse evidence step calls it through "$GATE" for
  # --payload / --ddl-objects), and that line carries no positional
  # arguments at all, so a first-match-anywhere read would count zero and
  # report the caller broken while it is correct.
  call=$(grep -hE '(bash|\./)[^#]*config-apply-gate\.sh' "$WF" | grep -vE '^\s*#' | sed -n 1p)
  argc=$(printf '%s' "$call" | sed -E 's|.*config-apply-gate\.sh||' | grep -o '"\$[A-Z_]*"' | wc -l | tr -d ' ')
  if [[ -n "$call" && "$argc" -ge 3 ]]; then
    check_caller "deploy.yml passes the host baseline as the 3rd argument" "ok"
  else
    check_caller "deploy.yml passes the host baseline as the 3rd argument (got $argc args — without it a catch-up or skip-ahead deploy gets a false 'the binary deploy is complete')" "no"
  fi
  # ORDERING, not mere presence. The check below used to be two
  # independent greps for `id: baseline` and `deployed-versions`, and its
  # message asserted the baseline is read BEFORE the playbook — which
  # nothing verified. Moving the baseline step below the deploy step
  # would have kept this green while making the gate permanently
  # vacuous: the "live" version would be the version just deployed, so
  # the diff range collapses to nothing and every config change passes.
  bl_line=$(grep -n 'id: baseline' "$WF" | sed -n 1p | cut -d: -f1)
  pb_line=$(grep -nE 'name:.*(Run deploy playbook|deploy playbook)' "$WF" | sed -n 1p | cut -d: -f1)
  if [[ -n "$bl_line" && -n "$pb_line" && "$bl_line" -lt "$pb_line" ]] \
     && grep -q 'deployed-versions' "$WF"; then
    check_caller "deploy.yml reads the host's live version BEFORE the playbook runs (baseline@L$bl_line < playbook@L$pb_line)" "ok"
  else
    check_caller "deploy.yml reads the host's live version before the playbook runs (baseline@L${bl_line:-none} playbook@L${pb_line:-none} — if the baseline is read AFTER the deploy it equals the version just deployed and the gate is vacuous)" "no"
  fi

  # The sidecars carry NO trailing newline (ansible copy `content:`), so
  # the read must supply one per file. `cat` mashes six versions into a
  # single token that a `^`-only anchor matches, which failed the gate
  # closed on every deploy and skipped the post-deploy smoke step.
  #
  # Asserted as a PROPERTY, not one spelling: the read must run `awk 1`
  # and must not `cat` the directory. #427 replaced the single-line
  # `awk 1 /var/lib/.../stellarindex-*` with a loop that skips the
  # migrate sidecar and calls `awk 1 "$f"` per file — same guarantee,
  # different text, and the old literal grep failed it.
  if grep -q 'deployed-versions' "$WF" \
     && grep -qE '(^|[^-[:alnum:]])awk 1( |")' "$WF" \
     && ! grep -qE '(^|[^[:alnum:]])cat [^|]*deployed-versions' "$WF"; then
    check_caller "deploy.yml reads the sidecars newline-safely (awk 1, not cat)" "ok"
  else
    check_caller "deploy.yml reads the sidecars newline-safely (found a bare 'cat' over deployed-versions/*, or no 'awk 1' at all — the files have no trailing newline, so N binaries concatenate into one garbage token)" "no"
  fi

  # And the version regex must be anchored at BOTH ends, so a mashed or
  # otherwise malformed sidecar is rejected rather than passed through
  # as if it were a version.
  if grep -qE "grep -E '\^v\[0-9\]\+\\\.\[0-9\]\+\\\.\[0-9\]\+\(-\[0-9A-Za-z\.\]\+\)\?\\\$'" "$WF"; then
    check_caller "deploy.yml's version filter is end-anchored" "ok"
  else
    check_caller "deploy.yml's version filter is end-anchored (a '^'-only anchor matches a concatenation like v0.1.0v0.2.0)" "no"
  fi
fi

# ═══ deploy.yml's per-region binary manifest ═════════════════════════
#
# The workflow carried ONE default binary list for three regions that run
# different unit sets, and nothing declared the difference to it. Both of
# 2026-09-07's binary-set failures follow:
#
#   - the pubnet six-binary default dispatched at futurenet, which has no
#     stellarindex-aggregator unit. deploy-one-binary.yml restarts
#     <binary>.service and then requires `systemctl is-active`, so the
#     health probe failed and the binary rolled back. The residue is still
#     on that host as /usr/local/bin/stellarindex-aggregator.failed-v0.62.0.
#   - a four-of-six set at r1, which left stellarindex-migrate and
#     stellarindex-sla-probe two releases behind and tripped
#     stellarindex_binary_version_skew AFTER everything was live.
#
# The two steps that refuse them are extracted from the workflow and run,
# exactly as deploy-inputs-test.sh and deploy-baseline-test.sh do, so what
# is under test is the shipped script and not a twin of it.

if ! python3 - "$WF" "$TMP/binset.sh" "$TMP/hostset.sh" <<'PY'
import sys, yaml
wf = yaml.safe_load(open(sys.argv[1]))
want = {"binset": sys.argv[2], "hostset": sys.argv[3]}
found = set()
for job in wf["jobs"].values():
    for step in job.get("steps", []):
        sid = step.get("id")
        if sid in want:
            open(want[sid], "w").write(step["run"])
            found.add(sid)
sys.exit(0 if found == set(want) else 1)
PY
then
  echo "FAIL: could not extract deploy.yml's binset/hostset steps — the refusals below did NOT run"
  fail=$((fail + 1))
fi

TAB=$'\t'
FIXTURE_MANIFEST="$TMP/region-binaries.tsv"
SIX="stellarindex-indexer,stellarindex-aggregator,stellarindex-api,stellarindex-sla-probe,stellarindex-ops,stellarindex-migrate"
FIVE="stellarindex-indexer,stellarindex-api,stellarindex-sla-probe,stellarindex-ops,stellarindex-migrate"
{
  printf '# fixture\n'
  printf 'r1%s10.0.0.1%sroot%s-%s%s%s-%s2026-09-07T00:00:00Z\n' "$TAB" "$TAB" "$TAB" "$TAB" "$SIX" "$TAB" "$TAB"
  printf 'testnet%s10.0.0.2%sroot%sroot@jump%s%s%s-%s2026-09-07T00:00:00Z\n' "$TAB" "$TAB" "$TAB" "$TAB" "$SIX" "$TAB" "$TAB"
  printf 'futurenet%s10.0.0.3%sroot%sroot@jump%s%s%sstellarindex-aggregator:unit-not-found%s2026-09-07T00:00:00Z\n' "$TAB" "$TAB" "$TAB" "$TAB" "$FIVE" "$TAB" "$TAB"
} > "$FIXTURE_MANIFEST"

# run_binset <region> <binaries-csv> [manifest] → BS_RC / BS_OUT / BS_GH
run_binset() {
  : > "$TMP/binset_out"
  BS_OUT="$(REGION="$1" VERSION=v0.63.0 BINARIES="$2" MANIFEST="${3:-$FIXTURE_MANIFEST}" \
            GITHUB_OUTPUT="$TMP/binset_out" bash "$TMP/binset.sh" 2>&1)"
  BS_RC=$?
  BS_GH="$(cat "$TMP/binset_out")"
}

gh_value() { # gh_value <key> — read one key out of the captured GITHUB_OUTPUT
  awk -F= -v k="$1" '$1 == k { sub(/^[^=]*=/, ""); print }' <<<"$BS_GH"
}

# --- 20. THE FUTURENET FAILURE ----------------------------------------
run_binset futurenet "$SIX"
if [[ "$BS_RC" -ne 0 ]]; then
  echo "FAIL: the pubnet default at futurenet was refused outright (rc=$BS_RC); the aggregator's absence there is a declared, reviewed fact and the other five must still deploy"
  fail=$((fail + 1))
elif [[ "$(gh_value binaries)" != "$FIVE" ]]; then
  echo "FAIL: futurenet's effective set is '$(gh_value binaries)', want '$FIVE' — the six-binary default must be FILTERED, not obeyed"
  fail=$((fail + 1))
elif [[ "$BS_OUT" != *"::warning::skipping stellarindex-aggregator at futurenet"* ]]; then
  echo "FAIL: the aggregator was dropped at futurenet without a warning naming it: $BS_OUT"
  fail=$((fail + 1))
elif [[ "$BS_OUT" != *"unit-not-found"* ]]; then
  echo "FAIL: the skip did not carry the manifest's recorded reason"
  fail=$((fail + 1))
else
  echo "ok: the pubnet six-binary default at futurenet deploys five and says which one it skipped"
  pass=$((pass + 1))
fi

# The effective set must reach the steps that act on it. A filter nothing
# downstream reads is the same bug with extra output.
for consumer in "Download release binaries" "Verify release signature, then checksums" "Run deploy playbook" "Summary"; do
  if python3 - "$WF" "$consumer" <<'PY'
import sys, yaml
wf = yaml.safe_load(open(sys.argv[1]))
for job in wf["jobs"].values():
    for step in job.get("steps", []):
        if step.get("name") == sys.argv[2]:
            sys.exit(0 if "steps.binset.outputs.binaries" in str(step.get("env", {})) else 1)
sys.exit(1)
PY
  then
    echo "ok: '$consumer' consumes the effective set, not the raw request"
    pass=$((pass + 1))
  else
    echo "FAIL: '$consumer' still reads inputs.binaries — the filtered set never reaches it"
    fail=$((fail + 1))
  fi
done

# --- 21. a binary the region's row has no opinion about is REFUSED -----
run_binset futurenet "stellarindex-indexer,stellarindex-newthing"
if [[ "$BS_RC" -eq 0 ]]; then
  echo "FAIL: a binary absent from BOTH the region's set and its not-deployable list was accepted"
  fail=$((fail + 1))
elif [[ "$BS_OUT" == *"::error::"*"stellarindex-newthing"* ]]; then
  echo "ok: a binary the manifest has no opinion about is refused, by name"
  pass=$((pass + 1))
else
  echo "FAIL: refused for the wrong reason: $BS_OUT"
  fail=$((fail + 1))
fi

# --- 22. testnet's aggregator: PRESENT but disabled --------------------
#
# Measured 2026-09-07: testnet's stellarindex-aggregator is
# UnitFileState=disabled with ActiveState=active — off at boot, running
# now, and deployed there by this workflow at v0.62.0. futurenet's is
# LoadState=not-found. Deployability is unit PRESENCE; an enablement test
# would drop testnet's and re-create the skew this exists to prevent.
run_binset testnet "$SIX"
if [[ "$BS_RC" -eq 0 && "$(gh_value binaries)" == "$SIX" ]]; then
  echo "ok: testnet keeps all six — a disabled-but-present unit is deployable"
  pass=$((pass + 1))
else
  echo "FAIL: testnet's set came back rc=$BS_RC '$(gh_value binaries)', want all six"
  fail=$((fail + 1))
fi

# --- 23. a partial set is recorded, not silently accepted --------------
run_binset r1 "stellarindex-indexer,stellarindex-aggregator,stellarindex-api,stellarindex-ops"
if [[ "$BS_RC" -ne 0 ]]; then
  echo "FAIL: a partial dispatch was refused offline (rc=$BS_RC); whether it leaves skew depends on the host's sidecars, which this step cannot read"
  fail=$((fail + 1))
elif [[ "$(gh_value omitted)" != *"stellarindex-sla-probe"* || "$(gh_value omitted)" != *"stellarindex-migrate"* ]]; then
  echo "FAIL: the four-of-six r1 dispatch did not record both omitted binaries (got '$(gh_value omitted)')"
  fail=$((fail + 1))
else
  echo "ok: the four-of-six r1 dispatch names both binaries it would leave behind"
  pass=$((pass + 1))
fi

# --- 24. a region with no row, and a missing manifest, both refuse -----
run_binset r2 "$SIX"
if [[ "$BS_RC" -ne 0 && "$BS_OUT" == *"--refresh-manifest"* ]]; then
  echo "ok: a region with no manifest row refuses and names the command that derives one"
  pass=$((pass + 1))
else
  echo "FAIL: an unknown region did not refuse with a remedy (rc=$BS_RC)"
  fail=$((fail + 1))
fi
run_binset r1 "$SIX" "$TMP/no-such-manifest.tsv"
if [[ "$BS_RC" -ne 0 ]]; then
  echo "ok: a missing manifest refuses rather than falling back to the default list"
  pass=$((pass + 1))
else
  echo "FAIL: a missing manifest was tolerated — the default list would have been obeyed again"
  fail=$((fail + 1))
fi

# --- 25. the CHECKED-IN manifest, against the CHECKED-IN default -------
#
# Everything above runs on a fixture. This runs the real row for each real
# region against the workflow's real `binaries` default, so a manifest edit
# that breaks a region is caught here rather than on a dispatch.
REAL_MANIFEST="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/scripts/dev/region-binaries.tsv"
real_default="$(python3 - "$WF" <<'PY'
import sys, yaml
wf = yaml.safe_load(open(sys.argv[1]))
on = wf.get("on", wf.get(True))
print(on["workflow_dispatch"]["inputs"]["binaries"]["default"])
PY
)"
if [[ -z "$real_default" ]]; then
  echo "FAIL: could not read deploy.yml's binaries default"; fail=$((fail + 1))
else
  real_bad=0
  for r in r1 testnet futurenet; do
    run_binset "$r" "$real_default" "$REAL_MANIFEST"
    [[ "$BS_RC" -eq 0 ]] || { echo "  the shipped default is refused at ${r}: $BS_OUT"; real_bad=1; }
    [[ -n "$(gh_value binaries)" ]] || real_bad=1
  done
  run_binset futurenet "$real_default" "$REAL_MANIFEST"
  if [[ "$real_bad" -eq 0 && "$(gh_value binaries)" != *"stellarindex-aggregator"* ]]; then
    echo "ok: the shipped default dispatches at all three regions and drops the aggregator only at futurenet"
    pass=$((pass + 1))
  else
    echo "FAIL: the checked-in manifest does not resolve the checked-in default for every region"
    fail=$((fail + 1))
  fi
  # Every binary either side of the manifest names must be a real cmd/
  # directory, which is deploy.yml's own validation: a row naming a retired
  # binary would refuse every dispatch at that region.
  missing_cmd=""
  while IFS=$'\t' read -r region _ _ _ set_csv not_dep _; do
    case "$region" in ''|'#'*) continue ;; esac
    for b in $(tr ',' ' ' <<<"$set_csv") $(tr ',' ' ' <<<"${not_dep%%:*}"); do
      case "$b" in ''|'-') continue ;; esac
      b="${b%%:*}"
      [[ -d "cmd/$b" ]] || missing_cmd="${missing_cmd}${region}:${b} "
    done
  done < "$REAL_MANIFEST"
  if [[ -z "$missing_cmd" ]]; then
    echo "ok: every binary the manifest names has a cmd/ directory"
    pass=$((pass + 1))
  else
    echo "FAIL: the manifest names binaries with no cmd/ directory: $missing_cmd"
    fail=$((fail + 1))
  fi
  # And every region the workflow offers must have a row, or its first
  # dispatch refuses.
  offered_regions="$(python3 - "$WF" <<'PY'
import sys, yaml
wf = yaml.safe_load(open(sys.argv[1]))
on = wf.get("on", wf.get(True))
for o in on["workflow_dispatch"]["inputs"]["region"]["options"]:
    print(o)
PY
)"
  missing_row=""
  while IFS= read -r r; do
    [[ -n "$r" ]] || continue
    awk -F'\t' -v r="$r" '$1 == r { found = 1 } END { exit found ? 0 : 1 }' "$REAL_MANIFEST" \
      || missing_row="${missing_row}${r} "
  done <<<"$offered_regions"
  if [[ -z "$missing_row" ]]; then
    echo "ok: every region deploy.yml offers has a manifest row"
    pass=$((pass + 1))
  else
    echo "FAIL: deploy.yml offers regions the manifest does not cover: $missing_row"
    fail=$((fail + 1))
  fi
fi

# ─── The host reconciliation: staleness, and the skew refusal ────────
mkdir -p "$TMP/bin"

# fake_host_ssh <exit-code> <tsv-table>
fake_host_ssh() {
  cat > "$TMP/bin/ssh" <<EOF
#!/usr/bin/env bash
cat <<'OUT'
$2
OUT
exit $1
EOF
  chmod +x "$TMP/bin/ssh"
}

REAL_PLAYBOOK="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/configs/ansible/playbooks/deploy-binary.yml"

# run_hostset <effective-csv> <omitted-space> <region-set-space> <not-deployable-space>
run_hostset() {
  HS_OUT="$(PATH="$TMP/bin:$PATH" REGION=r1 VERSION=v0.63.0 \
            BINARIES="$1" OMITTED="$2" REGION_SET="$3" NOT_DEPLOYABLE="$4" \
            DEPLOY_HOST=10.0.0.1 DEPLOY_USER=root DEPLOY_JUMP="" \
            MANIFEST="$FIXTURE_MANIFEST" DEPLOY_PLAYBOOK="$REAL_PLAYBOOK" \
            bash "$TMP/hostset.sh" 2>&1)"
  HS_RC=$?
}

FULL_SET="stellarindex-indexer stellarindex-aggregator stellarindex-api stellarindex-sla-probe stellarindex-ops stellarindex-migrate"
# The CLI binaries have no unit anywhere — r1's ops and migrate really do
# report LoadState=not-found — so the table below is what a healthy r1
# looks like, and any rule that judged them by unit state would fail here.
HEALTHY_R1="stellarindex-indexer${TAB}loaded${TAB}v0.63.0
stellarindex-aggregator${TAB}loaded${TAB}v0.63.0
stellarindex-api${TAB}loaded${TAB}v0.63.0
stellarindex-sla-probe${TAB}loaded${TAB}v0.63.0
stellarindex-ops${TAB}not-found${TAB}v0.63.0
stellarindex-migrate${TAB}not-found${TAB}v0.63.0"

fake_host_ssh 0 "$HEALTHY_R1"
run_hostset "$(tr ' ' ',' <<<"$FULL_SET")" "" "$FULL_SET" ""
if [[ "$HS_RC" -eq 0 ]]; then
  echo "ok: a healthy full dispatch passes, and the unit-less CLI binaries are not judged by unit state"
  pass=$((pass + 1))
else
  echo "FAIL: a healthy r1 full dispatch was refused: $HS_OUT"
  fail=$((fail + 1))
fi

# --- 26. THE FOUR-OF-SIX r1 FAILURE -----------------------------------
BEHIND_R1="${HEALTHY_R1//stellarindex-sla-probe${TAB}loaded${TAB}v0.63.0/stellarindex-sla-probe${TAB}loaded${TAB}v0.61.1}"
BEHIND_R1="${BEHIND_R1//stellarindex-migrate${TAB}not-found${TAB}v0.63.0/stellarindex-migrate${TAB}not-found${TAB}v0.28.1}"
fake_host_ssh 0 "$BEHIND_R1"
run_hostset "stellarindex-indexer,stellarindex-aggregator,stellarindex-api,stellarindex-ops" \
            "stellarindex-sla-probe stellarindex-migrate" "$FULL_SET" ""
if [[ "$HS_RC" -eq 0 ]]; then
  echo "FAIL: the four-of-six dispatch was accepted while sla-probe and migrate sit at older versions — that is the skew the probe caught AFTER everything went live"
  fail=$((fail + 1))
elif [[ "$HS_OUT" == *"stellarindex-sla-probe=v0.61.1"* && "$HS_OUT" == *"stellarindex-migrate=v0.28.1"* ]]; then
  echo "ok: a partial dispatch that would leave skew is refused, naming each stale binary and its version"
  pass=$((pass + 1))
else
  echo "FAIL: refused without naming the stale binaries: $HS_OUT"
  fail=$((fail + 1))
fi

# --- 27. the skew RECOVERY path still works ---------------------------
#
# deploy.yml's own served-path smoke tells the operator to "re-run this
# deploy naming the stale binary". A blanket refusal of every partial set
# would have broken the remedy it prints.
fake_host_ssh 0 "$HEALTHY_R1"
run_hostset "stellarindex-api" \
            "stellarindex-indexer stellarindex-aggregator stellarindex-sla-probe stellarindex-ops stellarindex-migrate" \
            "$FULL_SET" ""
if [[ "$HS_RC" -eq 0 && "$HS_OUT" == *"already on v0.63.0"* ]]; then
  echo "ok: a single-binary re-run is accepted when every omitted peer is already current"
  pass=$((pass + 1))
else
  echo "FAIL: the documented skew-recovery dispatch was refused (rc=$HS_RC): $HS_OUT"
  fail=$((fail + 1))
fi

# --- 28. a stale manifest is DETECTED, in both directions -------------
STALE_A="${HEALTHY_R1//stellarindex-aggregator${TAB}loaded/stellarindex-aggregator${TAB}not-found}"
fake_host_ssh 0 "$STALE_A"
run_hostset "$(tr ' ' ',' <<<"$FULL_SET")" "" "$FULL_SET" ""
if [[ "$HS_RC" -ne 0 && "$HS_OUT" == *"--refresh-manifest"* ]]; then
  echo "ok: a unit the manifest claims deployable but the host does not have refuses, naming the re-derive command"
  pass=$((pass + 1))
else
  echo "FAIL: a vanished unit passed reconciliation (rc=$HS_RC) — this is exactly the futurenet aggregator, and the deploy would roll it back"
  fail=$((fail + 1))
fi

fake_host_ssh 0 "$HEALTHY_R1"
run_hostset "stellarindex-indexer,stellarindex-api,stellarindex-sla-probe,stellarindex-ops,stellarindex-migrate" \
            "" "stellarindex-indexer stellarindex-api stellarindex-sla-probe stellarindex-ops stellarindex-migrate" \
            "stellarindex-aggregator:unit-not-found"
if [[ "$HS_RC" -ne 0 && "$HS_OUT" == *"--refresh-manifest"* ]]; then
  echo "ok: a unit the manifest calls undeployable but the host now loads refuses too — the row is stale the other way"
  pass=$((pass + 1))
else
  echo "FAIL: a unit that reappeared on the host was silently skipped (rc=$HS_RC) — it would be left behind at every release"
  fail=$((fail + 1))
fi

# --- 29. an unread host is not a green -------------------------------
fake_host_ssh 255 ""
run_hostset "$(tr ' ' ',' <<<"$FULL_SET")" "" "$FULL_SET" ""
if [[ "$HS_RC" -ne 0 ]]; then
  echo "ok: an ssh failure refuses rather than assuming the manifest still holds"
  pass=$((pass + 1))
else
  echo "FAIL: an unreadable host passed reconciliation"
  fail=$((fail + 1))
fi

# --- 30. enablement is never consulted --------------------------------
#
# Structural, because the behavioural cases cannot prove an absence:
# testnet's aggregator is UnitFileState=disabled and must still deploy, so
# the step must read LoadState and nothing else.
if grep -qE 'is-enabled|UnitFileState' "$TMP/hostset.sh"; then
  echo "FAIL: the reconciliation reads unit ENABLEMENT — testnet's aggregator is disabled and active, and excluding it re-creates the skew this exists to prevent"
  fail=$((fail + 1))
else
  echo "ok: deployability is read as unit presence (LoadState), never enablement"
  pass=$((pass + 1))
fi

# ─── The evidence step and the gate, as one loop ─────────────────────
#
# deploy.yml's ClickHouse step WRITES the [applied] argument and the gate
# READS it. Either half looks right alone; what matters is that the
# writer's output, fed to the reader, clears exactly the surfaces the host
# actually carries and no others. Run over the REAL v0.61.1..v0.62.0 range
# against a fake host, so the diff is the incident's own and only the
# host's answer is simulated.
if ! python3 - "$WF" "$TMP/chddl.sh" <<'PY'
import sys, yaml
wf = yaml.safe_load(open(sys.argv[1]))
for job in wf["jobs"].values():
    for step in job.get("steps", []):
        if step.get("id") == "ch_ddl":
            open(sys.argv[2], "w").write(step["run"])
            sys.exit(0)
sys.exit(1)
PY
then
  echo "FAIL: could not extract deploy.yml's ch_ddl step — the evidence half did NOT run"
  fail=$((fail + 1))
elif ! git rev-parse -q --verify 'v0.62.0^{commit}' >/dev/null; then
  if [ "$has_history" = no ]; then
    echo "NOTE: v0.62.0 unreachable — no git history in this tree, so the evidence-loop corroboration did not run here"
    skipped=$((skipped + 1))
  else
    echo "FAIL: v0.62.0 does not resolve here, so the evidence loop did NOT run (git fetch --tags origin)"
    fail=$((fail + 1))
  fi
else
  # run_chddl <fake-ssh-exit> <fake-ssh-stdout> → CH_RC / CH_OUT / CH_APPLIED
  run_chddl() {
    fake_host_ssh "$1" "$2"
    : > "$TMP/chddl_out"
    CH_OUT="$(PATH="$TMP/bin:$PATH" VERSION=v0.62.0 BASELINE=v0.61.1 \
              DEPLOY_HOST=10.0.0.1 DEPLOY_USER=root DEPLOY_JUMP="" \
              GATE=scripts/ci/config-apply-gate.sh GITHUB_OUTPUT="$TMP/chddl_out" \
              bash "$TMP/chddl.sh" 2>&1)"
    CH_RC=$?
    CH_APPLIED="$(sed -E 's/^applied=//' "$TMP/chddl_out")"
  }

  run_chddl 0 "stellar.account_creators_ops
stellar.ledgers"
  if [[ "$CH_APPLIED" == *"account_creators_rollup.sql"* && "$CH_APPLIED" == *"tier1_schema.sql"* ]]; then
    echo "ok: with the object present, the evidence step certifies both files that create it"
    pass=$((pass + 1))
  else
    echo "FAIL: the evidence step certified '$CH_APPLIED' over the real range: $CH_OUT"
    fail=$((fail + 1))
  fi
  # It must not claim the comment-only sibling — the gate clears that on
  # its own reasoning, and a certification would assert a check nobody ran.
  if [[ "$CH_APPLIED" != *"account_sponsors_rollup.sql"* ]]; then
    echo "ok: the evidence step does not certify the comment-only sibling"
    pass=$((pass + 1))
  else
    echo "FAIL: the evidence step certified a file it never asked the host about"
    fail=$((fail + 1))
  fi

  # THE LOOP: hand that output to the gate exactly as the workflow does.
  OUT="$(GITHUB_STEP_SUMMARY="$TMP/summary" bash "$GATE" v0.62.0 false v0.61.1 "$CH_APPLIED" 2>&1)"
  RC=$?
  expect "the gate clears the real range on the evidence the step produced" 0 "VERIFIED automatically"

  # And without it, the same range must still block: the pass above must
  # come from the evidence, not from the classification.
  OUT="$(GITHUB_STEP_SUMMARY="$TMP/summary" bash "$GATE" v0.62.0 false v0.61.1 "" 2>&1)"
  RC=$?
  expect "the same range blocks when nothing proved the DDL applied" 1 "changed 2 config surface(s)"

  # The object missing on the host is the case the whole check exists for.
  run_chddl 0 "stellar.ledgers"
  if [[ -z "$CH_APPLIED" ]]; then
    echo "ok: an object the host does NOT have certifies nothing"
    pass=$((pass + 1))
  else
    echo "FAIL: certified '$CH_APPLIED' while the host lacked the object it creates"
    fail=$((fail + 1))
  fi

  # And it must not redden the job to say so: a host with no ClickHouse is a
  # normal state, and the gate keeping the surface is the whole response.
  run_chddl 255 ""
  if [[ -z "$CH_APPLIED" && "$CH_RC" -eq 0 ]]; then
    echo "ok: a host that could not be asked certifies nothing, quietly (fail-closed)"
    pass=$((pass + 1))
  else
    echo "FAIL: unreachable host gave applied='$CH_APPLIED' rc=$CH_RC"
    fail=$((fail + 1))
  fi
fi

echo
echo "config-apply-gate-test: $pass passed, $fail failed, $skipped corroboration(s) skipped (history=$has_history)"
[[ "$fail" -eq 0 ]]
