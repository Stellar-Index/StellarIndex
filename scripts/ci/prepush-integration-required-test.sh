#!/usr/bin/env bash
set -euo pipefail

# `git init` honours an INHERITED GIT_DIR ahead of its own `-C`, and a git
# hook exports GIT_DIR/GIT_INDEX_FILE. lint-changed dispatches test scripts,
# and the pre-commit hook runs lint-changed — so without this a fixture's
# init re-initialises the REAL repository: core.bare set on the live
# checkout, fixture commits on main, and git says only "warning: re-init".
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
      GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR

root="$(cd "$(dirname "$0")/../.." && pwd)"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/prepush-policy-test.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

git -C "$tmp" init -q
git -C "$tmp" config user.name test
git -C "$tmp" config user.email test@localhost.invalid
mkdir -p "$tmp/docs" "$tmp/internal/storage" "$tmp/internal/api/v1" "$tmp/migrations"
printf 'base\n' > "$tmp/README.md"
git -C "$tmp" add README.md
git -C "$tmp" commit -q -m base
base="$(git -C "$tmp" rev-parse HEAD)"

printf 'docs\n' > "$tmp/docs/local.md"
git -C "$tmp" add docs/local.md
git -C "$tmp" commit -q -m docs
docs="$(git -C "$tmp" rev-parse HEAD)"
if (cd "$tmp" && "$root/scripts/ci/prepush-integration-required.sh" "$base" "$docs"); then
  echo "prepush integration policy: docs-only range incorrectly required integration" >&2
  exit 1
fi

printf 'query\n' > "$tmp/internal/storage/query.go"
git -C "$tmp" add internal/storage/query.go
git -C "$tmp" commit -q -m storage
storage="$(git -C "$tmp" rev-parse HEAD)"
(cd "$tmp" && "$root/scripts/ci/prepush-integration-required.sh" "$docs" "$storage")

# A handler change reaches the suite: test/integration stands up the real
# router and drives served surfaces, so internal/api counts. This case exists
# because the filter once did not carry it — a commit touching only
# internal/api/v1 changed what /v1/history returns and left an integration
# assertion pinning the old behaviour, which passed locally and failed CI.
printf 'handler\n' > "$tmp/internal/api/v1/handler.go"
git -C "$tmp" add internal/api/v1/handler.go
git -C "$tmp" commit -q -m handler
handler="$(git -C "$tmp" rev-parse HEAD)"
(cd "$tmp" && "$root/scripts/ci/prepush-integration-required.sh" "$storage" "$handler")

printf 'migration\n' > "$tmp/migrations/9999_test.up.sql"
git -C "$tmp" add migrations/9999_test.up.sql
git -C "$tmp" commit -q -m migration
migration="$(git -C "$tmp" rev-parse HEAD)"
(cd "$tmp" && "$root/scripts/ci/prepush-integration-required.sh" "$handler" "$migration")

mkdir -p "$tmp/test/harness"
printf 'harness\n' > "$tmp/test/harness/timescale.go"
git -C "$tmp" add test/harness/timescale.go
git -C "$tmp" commit -q -m harness
harness="$(git -C "$tmp" rev-parse HEAD)"
# The Docker-backed bootstrap lives under test/harness; a change there alone
# must require the integration suite, or the wait strategy it implements can
# regress without any pre-push path compiling it.
(cd "$tmp" && "$root/scripts/ci/prepush-integration-required.sh" "$migration" "$harness")

# The remaining INT_TEST_PKGS directories (Makefile:INT_TEST_PKGS). The suite
# BUILDS AND RUNS these packages, so a change confined to one can break an
# integration-tagged test — and until T424/T449 neither this classifier nor
# ci.yml's preflight filter named them, so such a diff ran the suite in NO
# lane. scripts/ops/fx-history-backfill/generation_test.go is the case that
# matters: it pins an INV-3 money invariant (an operator fx_quotes correction
# must be stamped with a positive derive generation, or the next gen-0 worker
# refresh silently reverts it).
prev="$harness"
for dir in scripts/ops/fx-history-backfill cmd/stellarindex-ops internal/ops/archive; do
  mkdir -p "$tmp/$dir"
  printf 'ops\n' > "$tmp/$dir/main.go"
  git -C "$tmp" add "$dir/main.go"
  git -C "$tmp" commit -q -m "$dir"
  head="$(git -C "$tmp" rev-parse HEAD)"
  if ! (cd "$tmp" && "$root/scripts/ci/prepush-integration-required.sh" "$prev" "$head"); then
    echo "prepush integration policy: a diff confined to $dir did not require integration" >&2
    exit 1
  fi
  prev="$head"
done

# Precision, in the direction that actually costs: widening must NOT have
# swallowed the neighbouring trees. scripts/ci and internal/ops outside
# archive/ are edited constantly and get nothing from a Docker round-trip —
# and a classifier that requires integration for everything is indistinguishable
# from no classifier at all.
mkdir -p "$tmp/scripts/ci" "$tmp/internal/ops"
printf 'ci\n' > "$tmp/scripts/ci/helper.sh"
printf 'runbook\n' > "$tmp/internal/ops/runbook.go"
git -C "$tmp" add scripts/ci/helper.sh internal/ops/runbook.go
git -C "$tmp" commit -q -m neighbours
neighbours="$(git -C "$tmp" rev-parse HEAD)"
if (cd "$tmp" && "$root/scripts/ci/prepush-integration-required.sh" "$prev" "$neighbours"); then
  echo "prepush integration policy: scripts/ci + internal/ops (outside archive/) incorrectly required integration" >&2
  exit 1
fi

# And the original skip direction still holds after the widening: a docs-only
# range must still not drag the Docker suite in.
printf 'more docs\n' > "$tmp/docs/second.md"
git -C "$tmp" add docs/second.md
git -C "$tmp" commit -q -m docs2
docs2="$(git -C "$tmp" rev-parse HEAD)"
if (cd "$tmp" && "$root/scripts/ci/prepush-integration-required.sh" "$neighbours" "$docs2"); then
  echo "prepush integration policy: docs-only range incorrectly required integration after widening" >&2
  exit 1
fi

echo "prepush integration policy self-test: PASS"
