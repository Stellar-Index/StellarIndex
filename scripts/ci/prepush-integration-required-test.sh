#!/usr/bin/env bash
set -euo pipefail

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

echo "prepush integration policy self-test: PASS"
