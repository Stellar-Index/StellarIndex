#!/usr/bin/env bash
# Run the same deterministic shards as CI, with a conservative local default.
set -euo pipefail

cd "$(dirname "$0")/../.."

shards="${1:-2}"
[[ "$shards" =~ ^[1-9][0-9]*$ ]] || { echo "usage: $0 [positive-shard-count]" >&2; exit 2; }
docker info >/dev/null

log_dir="$(mktemp -d "${TMPDIR:-/tmp}/stellarindex-integration.XXXXXX")"
pids=()
start="$(date +%s)"
# The log directory survives a FAILING run. It used to be removed
# unconditionally, so the only record of a red shard was the 160 trailing
# lines printed below — and on this suite those are container lifecycle and
# heavy-job chatter from tests that ran AFTER the failure, with the
# `--- FAIL: TestX` line long scrolled past. A reader of a red gate then has
# the fact of a failure and no name for it.
cleanup() {
  if (( failed == 0 )); then
    rm -rf "$log_dir"
  else
    echo "integration: full shard logs kept at $log_dir" >&2
  fi
}
failed=0
trap cleanup EXIT

for ((i = 0; i < shards; i++)); do
  ./scripts/ci/integration-shard.sh "$i" "$shards" >"$log_dir/shard-$i.log" 2>&1 &
  pids+=("$!")
done

for ((i = 0; i < shards; i++)); do
  if ! wait "${pids[$i]}"; then
    echo "integration: shard $i/$shards FAILED" >&2
    # The names FIRST, then the tail. `go test` prints `--- FAIL: TestX`
    # where the failure happened, not at the end, so a tail alone reports
    # whichever tests happened to run last. grep is allowed to match
    # nothing — a shard that died before any test named itself (a build
    # error, a Docker refusal) still has its tail printed below.
    echo "integration: shard $i failing tests + panics:" >&2
    grep -nE "^(--- FAIL|    --- FAIL|panic:|fatal error:)" "$log_dir/shard-$i.log" >&2 || \
      echo "  (none named — the shard failed before any test reported)" >&2
    echo "integration: shard $i last 160 lines:" >&2
    tail -160 "$log_dir/shard-$i.log" >&2
    failed=1
  else
    echo "integration: shard $i/$shards passed"
  fi
done

elapsed=$(( $(date +%s) - start ))
(( failed == 0 )) || { echo "integration: FAIL after ${elapsed}s" >&2; exit 1; }
echo "integration: PASS $shards shard(s) in ${elapsed}s"
