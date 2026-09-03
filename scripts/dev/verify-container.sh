#!/usr/bin/env bash
# Run the deterministic Linux-compatible gate in the pinned verifier image.
set -euo pipefail

cd "$(dirname "$0")/../.."

source_dir="${VERIFY_SOURCE:-$PWD}"
history_dir="${VERIFY_HISTORY_SOURCE:-$PWD}"
image="${VERIFY_IMAGE:-stellarindex/verify:local}"

[[ -d "$source_dir" ]] || { echo "verify-container: source not found: $source_dir" >&2; exit 2; }
[[ -e "$history_dir/.git" ]] || { echo "verify-container: history repo not found: $history_dir" >&2; exit 2; }
docker info >/dev/null

echo "verify-container: building $image for native Docker architecture"
docker build --pull=false -t "$image" docker/verify

docker run --rm \
  --mount "type=bind,src=$source_dir,dst=/workspace" \
  --mount "type=bind,src=$history_dir,dst=/history,readonly" \
  --mount type=volume,src=stellarindex-verify-go-build,dst=/root/.cache/go-build \
  --mount type=volume,src=stellarindex-verify-go-mod,dst=/go/pkg/mod \
  --mount type=volume,src=stellarindex-verify-pnpm,dst=/pnpm/store \
  --mount type=volume,src=stellarindex-verify-explorer-modules,dst=/workspace/web/explorer/node_modules \
  --mount type=volume,src=stellarindex-verify-status-modules,dst=/workspace/web/status/node_modules \
  -e GOMODCACHE=/go/pkg/mod \
  -e PNPM_HOME=/pnpm \
  -e PNPM_STORE_DIR=/pnpm/store \
  "$image"
