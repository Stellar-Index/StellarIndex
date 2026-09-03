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
cleanup() { rm -rf "$log_dir"; }
trap cleanup EXIT

for ((i = 0; i < shards; i++)); do
  ./scripts/ci/integration-shard.sh "$i" "$shards" >"$log_dir/shard-$i.log" 2>&1 &
  pids+=("$!")
done

failed=0
for ((i = 0; i < shards; i++)); do
  if ! wait "${pids[$i]}"; then
    echo "integration: shard $i/$shards FAILED" >&2
    tail -160 "$log_dir/shard-$i.log" >&2
    failed=1
  else
    echo "integration: shard $i/$shards passed"
  fi
done

elapsed=$(( $(date +%s) - start ))
(( failed == 0 )) || { echo "integration: FAIL after ${elapsed}s" >&2; exit 1; }
echo "integration: PASS $shards shard(s) in ${elapsed}s"
