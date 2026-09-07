#!/usr/bin/env bash
# check-change-class-test.sh — fixture tests for the CI path-filter
# decision core (scripts/ci/check-change-class.sh, mirrors ci.yml's
# `preflight` job's path-filter rules).
#
# The two load-bearing cases: a docs-only diff
# must classify as "skip the integration shards" and an
# internal/storage diff must classify as "do not skip" — get the second
# one wrong and the shard matrix (the only real coverage for the
# 2026-07-01 sponsors/markets/blend regression class) silently stops
# running on the changes that need it most.
#
# Run: bash scripts/ci/check-change-class-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
CHECK="$PWD/scripts/ci/check-change-class.sh"

pass=0
fail=0
asserts=0

# run <class> <file> [<file> ...] — via argv (no stdin involved)
run() {
  local class="$1"; shift
  OUT="$(bash "$CHECK" "$class" "$@" 2>&1)"
  RC=$?
}

# run_stdin <class> <newline-separated files>
run_stdin() {
  local class="$1" files="$2"
  OUT="$(printf '%s\n' "$files" | bash "$CHECK" "$class" 2>&1)"
  RC=$?
}

expect() {
  local name="$1" want_rc="$2"
  asserts=$((asserts + 1))
  if [ "$RC" -eq "$want_rc" ]; then
    echo "ok: $name"
    pass=$((pass + 1))
  else
    echo "FAIL: $name — exit $RC, want $want_rc" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
  fi
}

# ── The two named scenarios ─────────────────────────────────────────

run integration "docs/architecture/lexicon.md" "CHANGELOG.md" "docs/contributing/local-verification.md"
expect "docs-only diff: integration class does NOT match (shards may be skipped)" 1

run integration "internal/storage/pricebook.go"
expect "internal/storage diff: integration class MATCHES (shards must run)" 0

# ── Every listed integration-triggering location ────────────────────

run integration "internal/pipeline/router.go"
expect "internal/pipeline/** triggers integration" 0

run integration "internal/sources/kraken/decode.go"
expect "internal/sources/** triggers integration" 0

run integration "internal/api/v1/handlers.go"
expect "internal/api/** triggers integration" 0

run integration "migrations/0160_add_column.up.sql"
expect "migrations/** triggers integration" 0

run integration "test/integration/markets_test.go"
expect "test/integration/** triggers integration" 0

run integration "go.mod"
expect "go.mod triggers integration" 0

# ── Precision: a .go file OUTSIDE the named integration subtrees must
# still run the go class but must NOT trip the integration class — a
# too-broad rule pays the Docker round-trip for changes the shard
# matrix was never built to cover.
run integration "internal/platform/logging.go"
expect "internal/platform (not storage/pipeline/sources/api) does NOT trigger integration" 1

run go "internal/platform/logging.go"
expect "internal/platform DOES trigger the go class" 0

run go "cmd/stellarindex-api/main.go"
expect "cmd/**/*.go triggers the go class" 0

run go "go.sum"
expect "go.sum triggers the go class" 0

run go "docs/architecture/lexicon.md"
expect "a markdown file does NOT trigger the go class" 1

# ── web / ansible classes ────────────────────────────────────────────

run web "web/explorer/src/app/page.tsx"
expect "web/** triggers the web class" 0

run web "openapi/stellar-index.v1.yaml"
expect "openapi/** triggers the web class" 0

run web "docs/reference/api/README.md"
expect "docs/reference/api (not web/ or openapi/) does NOT trigger the web class" 1

run ansible "configs/ansible/roles/monitoring/tasks/main.yml"
expect "configs/ansible/** triggers the ansible class" 0

run ansible "configs/prometheus/rules.r1/alerts.yml"
expect "configs/prometheus (not configs/ansible) does NOT trigger the ansible class" 1

# ── stdin path (the real `git diff --name-only | check-change-class.sh`
# call shape used in ci.yml) ─────────────────────────────────────────

run_stdin integration "$(printf 'CHANGELOG.md\ndocs/foo.md\n')"
expect "stdin: docs-only diff does not trigger integration" 1

run_stdin integration "$(printf 'internal/storage/x.go\ninternal/platform/y.go\n')"
expect "stdin: a mixed diff containing one storage file DOES trigger integration" 0

# ── Fail-closed on malformed calls ──────────────────────────────────

OUT="$(bash "$CHECK" 2>&1)"; RC=$?
expect "no class argument at all → usage error, not a silent skip" 2

OUT="$(bash "$CHECK" not-a-real-class "internal/storage/x.go" 2>&1)"; RC=$?
expect "unknown class name → usage error" 2

OUT="$(: | bash "$CHECK" integration 2>&1)"; RC=$?
expect "empty diff (zero files) → usage error, never read as 'nothing changed, skip'" 2

echo
echo "check-change-class-test: ${pass} passed, ${fail} failed, ${asserts} assertions requested"
if [ "$asserts" -lt 20 ]; then
  echo "check-change-class-test: FAIL — only ${asserts} assertions ran; cases have been lost" >&2
  exit 1
fi
[ "$fail" -eq 0 ] || exit 1
