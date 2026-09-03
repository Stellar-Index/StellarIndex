#!/usr/bin/env bash
# Fast, read-only feedback for the edit loop. This intentionally omits the
# expensive race, integration, vulnerability, monitoring and frontend-build
# lanes; make prepush owns the push decision.
set -euo pipefail

cd "$(dirname "$0")/../.."
go_bin="$(go env GOPATH)/bin"
export PATH="$go_bin:$PATH"

./scripts/dev/doctor.sh --profile portable

check_tracked_go_format() {
  local label="$1"
  shift
  local output
  output="$(git ls-files -z -- '*.go' | xargs -0 "$@")"
  if [[ -n "$output" ]]; then
    echo "check: $label failed:" >&2
    echo "$output" >&2
    exit 1
  fi
}

echo "=== Format (read-only) ==="
check_tracked_go_format gofumpt gofumpt -l
check_tracked_go_format goimports goimports -l -local github.com/Stellar-Index/StellarIndex

echo "=== Focused repository contracts ==="
./scripts/ci/lint-docs.sh
./scripts/ci/lint-doc-links.sh
./scripts/ci/lint-imports.sh
./scripts/ci/lint-i128.sh
./scripts/ci/lint-migrations.sh

echo "=== Unit tests (without race; fast loop only) ==="
go test -timeout 2m ./...

echo "check: PASS (portable profile; run make prepush before pushing)"
