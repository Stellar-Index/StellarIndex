#!/usr/bin/env bash
# Verify the exact committed HEAD before an intentional push. Work happens in
# a disposable clean checkout so formatters and generators cannot repair the
# candidate while judging it.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$repo_root"

profile="${VERIFY_PROFILE:-auto}"
integration="${VERIFY_INTEGRATION:-auto}"
shards="${LOCAL_INT_SHARDS:-2}"
lane_only="${PREPUSH_LANE_ONLY:-0}"

case "$profile" in auto|native|container|full) ;; *) echo "prepush: invalid VERIFY_PROFILE=$profile" >&2; exit 2 ;; esac
case "$integration" in auto|always|never) ;; *) echo "prepush: invalid VERIFY_INTEGRATION=$integration" >&2; exit 2 ;; esac

if [[ -n "$(git status --porcelain)" ]]; then
  echo "prepush: FAIL: working tree is not clean; commit or stash the candidate first" >&2
  git status --short >&2
  exit 1
fi

if [[ "$profile" == "auto" ]]; then
  if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    profile="container"
  else
    profile="native"
  fi
  echo "prepush: auto selected profile=$profile"
fi

./scripts/dev/doctor.sh --profile "$profile"
if [[ "$profile" == "native" ]]; then
  go_bin="$(go env GOPATH)/bin"
  export PATH="$go_bin:$PATH"
fi

base_sha="${BASE_SHA:-}"
if [[ -z "$base_sha" ]]; then
  if git rev-parse -q --verify origin/main >/dev/null 2>&1; then
    base_sha="$(git merge-base origin/main HEAD)"
  else
    base_sha="$(git rev-parse HEAD^)"
  fi
fi
git cat-file -e "${base_sha}^{commit}" 2>/dev/null || {
  echo "prepush: FAIL: comparison base $base_sha is not in local history" >&2
  exit 1
}
echo "prepush: candidate=$(git rev-parse --short=12 HEAD) base=$(git rev-parse --short=12 "$base_sha") profile=$profile"

# Range-sensitive gates must inspect the real history, not the disposable
# clean checkout used for the rest of verification.
echo "=== Push-range contracts ==="
BASE_SHA="$base_sha" ./scripts/ci/lint-baseline-growth.sh
BASE_SHA="$base_sha" ./scripts/ci/lint-replay-plan.sh

if [[ "$profile" == "native" ]]; then
  echo "=== Workflow security ==="
  zizmor --min-confidence medium .github/workflows/
  echo "=== Secrets (real committed history) ==="
  gitleaks detect --no-banner --redact --config .gitleaks.toml
fi

scratch="$(mktemp -d "${TMPDIR:-/tmp}/stellarindex-prepush.XXXXXX")"
source_dir="$scratch/source"
history_dir="$scratch/history"
mkdir -p "$source_dir"
cleanup() { rm -rf "$scratch"; }
trap cleanup EXIT

# git archive judges committed HEAD only and materializes it on the container
# or host's native filesystem. The synthetic repository exists solely for
# generated-artifact diff checks used by verify.sh.
git archive HEAD | tar -x -C "$source_dir"
git -C "$source_dir" init -q
git -C "$source_dir" config user.name stellarindex-prepush
git -C "$source_dir" config user.email prepush@localhost.invalid
git -C "$source_dir" add -A
git -C "$source_dir" commit -q -m "prepush candidate"

if [[ "$profile" == "container" || "$profile" == "full" ]]; then
  # A self-contained clone works for ordinary clones and linked worktrees,
  # without exposing unrelated files from the maintainer's checkout.
  git clone -q --no-local "$repo_root" "$history_dir"
  VERIFY_SOURCE="$source_dir" VERIFY_HISTORY_SOURCE="$history_dir" ./scripts/dev/verify-container.sh
else
  (
    cd "$source_dir"
    go_bin="$(go env GOPATH)/bin"
    export PATH="$go_bin:$PATH"
    BASE_SHA="$(git rev-parse HEAD)"
    export BASE_SHA
    export VERIFY_FAIL_ON_SKIP=1
    for app in web/explorer web/status; do
      [[ -f "$app/pnpm-lock.yaml" ]] || continue
      pnpm --dir "$app" install --frozen-lockfile
    done
    make verify
  )
fi

needs_integration=0
if [[ "$integration" == "always" || "$profile" == "full" ]]; then
  needs_integration=1
elif [[ "$integration" == "auto" ]]; then
  if ./scripts/ci/prepush-integration-required.sh "$base_sha" HEAD; then
    needs_integration=1
  fi
fi

if (( needs_integration )); then
  if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
    echo "prepush: FAIL: this change requires integration tests, but Docker is unavailable" >&2
    exit 1
  fi
  echo "=== Docker-backed integration tests ($shards local shard(s)) ==="
  ./scripts/dev/test-integration-parallel.sh "$shards"
else
  echo "=== Docker-backed integration tests: not required by this diff ==="
fi

if [[ "$lane_only" == "1" ]]; then
  echo "prepush: LINUX LANE PASSED (integration selection was not evaluated)"
else
  echo "prepush: ALL REQUIRED CHECKS PASSED profile=$profile integration=$([[ $needs_integration -eq 1 ]] && echo run || echo not-required)"
fi
