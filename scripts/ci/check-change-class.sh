#!/usr/bin/env bash
# check-change-class.sh — the CI path-filter decision core
# (mirrors ci.yml's `preflight` job's path-filter rules).
#
# WHY THIS EXISTS SEPARATELY FROM THE dorny/paths-filter STEP IN ci.yml.
# The filter that decides whether integration-test-shard/build/test/vuln/
# fuzz-smoke/web-explorer/web-status/ansible-check RUN AT ALL lives inside
# a third-party action's JS glob engine, which cannot be exercised offline
# without Docker/`act` — exactly the thing this repo's fixers are told
# never to reach for. So the RULE (which prefixes/extensions belong to
# which change class) is restated here, in five-line grep functions any
# CI run or laptop can execute in milliseconds, and `preflight` in ci.yml
# runs BOTH this script and the paths-filter action against the same diff
# and fails loudly if they disagree — so a change to one that is not
# mirrored in the other is caught the same day, not discovered as a
# silently-wedged or silently-skipped job weeks later.
#
# THE STAKES OF GETTING THIS WRONG IN EITHER DIRECTION. Too narrow and a
# storage/pipeline/sources/api change ships without the integration shards
# that are its only real coverage.
# Too broad and every docs/scripts-only change pays a 20-minute Docker
# round-trip for zero additional signal — the problem this filter exists
# to remove per the plan's measured numbers.
#
# WHICH internal/ PACKAGES THE integration CLASS NAMES. The suite imports
# 40+ internal/ packages; a PR runs the shards only for storage, schema,
# the ops subcommands the suite drives (internal/ops/chops through
# chops.Run, whose ClickHouse derives have no other real coverage), the
# served API surface (internal/api/v1/**) and the
# INT_TEST_PKGS directories, whose `//go:build integration` tests nothing
# else executes. The nightly full run covers the rest.
# test/controlwiring/integration_class_direct_imports_test.go fails when the
# suite directly imports a package that is neither in this class nor listed
# there as nightly-only with a reason, and when this regex and ci.yml's
# filter disagree on any tracked path.
# test/controlwiring/integration_pkgs_trigger_evidence_test.go fails if any
# INT_TEST_PKGS directory or go.mod is missing from this regex, from
# ci.yml's filter, or from prepush-integration-required.sh.
#
# Classes (must stay identical in substance to the `filters:` block in
# .github/workflows/ci.yml's preflight job):
#   integration  — internal/storage/**, internal/platform/postgresstore/**,
#                  internal/ops/{archive,chops}/**, internal/lendinginterest/**,
#                  internal/api/v1/**,
#                  cmd/stellarindex-ops/**, cmd/stellarindex-api/internal/wiring/**,
#                  migrations/**,
#                  scripts/ops/**, test/integration/**, test/harness/**,
#                  test/fixtures/**, go.mod, go.sum, Makefile,
#                  scripts/ci/integration-shard.sh, scripts/ci/lint-migrations.sh,
#                  .github/workflows/ci.yml, deploy/clickhouse/*.sql,
#                  configs/ansible/roles/redis-sentinel/templates/users.acl.j2,
#                  configs/ansible/roles/archival-node/tasks/10-observability.yml
#                  (the shard script and the integration tests read these
#                  directly)
#   go           — any *.go file, go.mod, go.sum, openapi/** (Go spec-parity
#                  tests read the spec directly), and the go:embed inputs
#                  (internal/sources/external/binance/pairs.yaml,
#                  internal/sources/external/forex/circulation_data.csv,
#                  internal/currency/data/seed.yaml,
#                  internal/incidents/data/**) and any testdata/** dir —
#                  none of those are *.go files but each is read by a Go
#                  test (test/controlwiring/rlt046_ci_class_trigger_test.go
#                  walks every //go:embed and fails on an uncovered input),
#                  plus Makefile, .github/workflows/ci.yml and
#                  scripts/ci/{check-change-class,prepush-integration-required}.sh
#                  (the trigger-class guard's inputs; it runs only in `test`),
#                  and scripts/dev/third-party-notices.py (its go||web gated
#                  step in repo-gates). Not every input a Go test reads:
#                  migrations/, configs/, deploy/, docs/ and test/ do not set
#                  `go`. The unit tests that read them still run because
#                  ci.yml's resolve step runs affected-go-pkgs on every diff,
#                  whatever `go` says.
#   web         — web/**, openapi/**, CHANGELOG.md, top-level files of
#                  docs/{adr,architecture,blog,operations}/,
#                  internal/incidents/data/**,
#                  internal/storage/timescale/cursors.go, the explorer
#                  postbuild scripts, check-redirects-cap.sh and
#                  .github/workflows/ci.yml (all read by explorer tests,
#                  its build or the web steps)
#   ansible      — configs/ansible/**, configs/prometheus/**,
#                  deploy/monitoring/** (clickhouse-exporter-test.sh reads
#                  the latter two and only that job runs it),
#                  docs/operations/self-hosting.md (ansible-api-env-test.sh)
#
# Usage:
#   git diff --name-only "$BASE" "$HEAD" | check-change-class.sh <class>
#   check-change-class.sh <class> <file> [<file> ...]
#
# Exit 0  — at least one changed file belongs to <class> (job should RUN).
# Exit 1  — no changed file belongs to <class> (job may be SKIPPED).
# Exit 2  — usage error (unknown class, or no files supplied at all — an
#           empty diff naming zero files is a caller bug, not "skip
#           everything", and must not be read as a quiet all-clear).
set -euo pipefail

class_integration() {
  grep -E '^(internal/storage/|internal/platform/postgresstore/|internal/ops/archive/|internal/ops/chops/|internal/lendinginterest/|internal/api/v1/|cmd/stellarindex-ops/|cmd/stellarindex-api/internal/wiring/|migrations/|scripts/ops/|test/(integration|harness|fixtures)/)|^go\.mod$|^go\.sum$|^Makefile$|^scripts/ci/integration-shard\.sh$|^scripts/ci/lint-migrations\.sh$|^\.github/workflows/ci\.yml$|^deploy/clickhouse/[^/]+\.sql$|^configs/ansible/roles/redis-sentinel/templates/users\.acl\.j2$|^configs/ansible/roles/archival-node/tasks/10-observability\.yml$'
}

class_go() {
  # openapi/** is a Go-test dependency (internal/api/v1's
  # handler_spec_fields_test.go and its spec-parity siblings read the spec
  # file directly), not just a web one — a diff confined to it must still
  # trigger the go-test job. Mirrored in ci.yml's preflight `go` filter.
  # The five data paths are go:embed inputs and testdata/ is Go's test
  # fixture convention: not *.go files, but each is read by a Go test.
  # The next four are the trigger-class guard's inputs, read only by it;
  # third-party-notices.py runs in a go||web gated repo-gates step.
  grep -E '(^|/)[^/]+\.go$|^go\.mod$|^go\.sum$|^openapi/|^internal/sources/external/binance/pairs\.yaml$|^internal/sources/external/forex/circulation_data\.csv$|^internal/currency/data/seed\.yaml$|^internal/wasmaudit/audited_wasm\.json$|^internal/incidents/data/|(^|/)testdata/|^Makefile$|^\.github/workflows/ci\.yml$|^scripts/ci/check-change-class\.sh$|^scripts/ci/prepush-integration-required\.sh$|^scripts/dev/third-party-notices\.py$'
}

class_web() {
  # Mirrored in ci.yml's preflight `web` filter; the docs loaders read
  # top-level files only.
  grep -E '^(web/|openapi/|internal/incidents/data/)|^CHANGELOG\.md$|^docs/(adr|architecture|blog|operations)/[^/]+$|^internal/storage/timescale/cursors\.go$|^scripts/ci/explorer-(file-budget|seo-lint|openapi-check|shell-fallback-check)\.sh$|^scripts/ci/check-redirects-cap\.sh$|^\.github/workflows/ci\.yml$'
}

class_ansible() {
  # configs/prometheus/** and deploy/monitoring/** are read by
  # clickhouse-exporter-test.sh, which only the ansible-check job runs —
  # so a diff confined to either set must set ansible=true. The self-host
  # guide is read by ansible-api-env-test.sh, also run only there.
  grep -E '^(configs/ansible/|configs/prometheus/|deploy/monitoring/)|^docs/operations/self-hosting\.md$'
}

usage() {
  echo "usage: check-change-class.sh <integration|go|web|ansible> [<file> ...]" >&2
  exit 2
}

[ "$#" -ge 1 ] || usage
CLASS="$1"
shift

case "$CLASS" in
  integration | go | web | ansible) ;;
  *) usage ;;
esac

# Always fully drain stdin, whether or not $# supplied files on argv, so
# an upstream `git diff | check-change-class.sh ...` never sees a broken
# pipe (lint-shell-sigpipe.sh's exact class of defect) when argv already
# satisfied the match and this script could otherwise exit before
# reading the rest of the producer's output.
STDIN_FILES=""
if [ ! -t 0 ]; then
  STDIN_FILES="$(cat)"
fi

ALL_FILES=""
if [ "$#" -gt 0 ]; then
  ALL_FILES="$(printf '%s\n' "$@")"
fi
if [ -n "$STDIN_FILES" ]; then
  ALL_FILES="${ALL_FILES}${ALL_FILES:+$'\n'}${STDIN_FILES}"
fi

if [ -z "$ALL_FILES" ]; then
  echo "check-change-class: FAIL — no changed files supplied; an empty diff must not be read as a class decision" >&2
  exit 2
fi

case "$CLASS" in
  integration) printf '%s\n' "$ALL_FILES" | class_integration >/dev/null && exit 0 ;;
  go) printf '%s\n' "$ALL_FILES" | class_go >/dev/null && exit 0 ;;
  web) printf '%s\n' "$ALL_FILES" | class_web >/dev/null && exit 0 ;;
  ansible) printf '%s\n' "$ALL_FILES" | class_ansible >/dev/null && exit 0 ;;
esac
exit 1
