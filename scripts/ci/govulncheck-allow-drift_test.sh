#!/usr/bin/env bash
# govulncheck-allow-drift_test.sh — pins the lib/pq version cited in
# govulncheck-allow.txt's header comment to go.mod's actual pin (T520).
#
# The allowlist's accepted-risk rationale names an exact lib/pq version and
# calls it "the Postgres driver (ADR-0006)". Both claims can silently drift
# from go.mod (a version bump, a driver migration) with nothing to catch it
# since the comment is prose, not code. This pins the version half of that
# claim so a future drift reds this test instead of surviving as a stale
# comment.
#
# Run: bash scripts/ci/govulncheck-allow-drift_test.sh
set -euo pipefail
cd "$(dirname "$0")/../.."

GOMOD_LINE="$(grep -m1 -oE 'github\.com/lib/pq v[0-9.]+' go.mod)"
GOMOD_VERSION="${GOMOD_LINE##* }"
ALLOW_LINE="$(grep -m1 -oE 'lib/pq@v[0-9.]+' scripts/ci/govulncheck-allow.txt)"
ALLOW_VERSION="${ALLOW_LINE#lib/pq@}"

if [[ -z "$GOMOD_VERSION" || -z "$ALLOW_VERSION" ]]; then
  echo "FAIL: could not extract a lib/pq version from go.mod or govulncheck-allow.txt" >&2
  exit 1
fi

if [[ "$GOMOD_VERSION" != "$ALLOW_VERSION" ]]; then
  echo "FAIL: govulncheck-allow.txt claims lib/pq@${ALLOW_VERSION} but go.mod pins ${GOMOD_VERSION}" >&2
  exit 1
fi

echo "PASS: govulncheck-allow.txt version (${ALLOW_VERSION}) matches go.mod (${GOMOD_VERSION})"
