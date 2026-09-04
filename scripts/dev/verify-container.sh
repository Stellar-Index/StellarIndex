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

# The Postman converter rides in the image at the version docs-postman.sh
# pins; that script is the single source, so the build arg is read from it.
# An empty value would make npm install the latest release, so refuse it.
converter_version="$(sed -n 's/^CONVERTER_VERSION="\(.*\)"$/\1/p' scripts/dev/docs-postman.sh)"
[[ -n "$converter_version" ]] || { echo "verify-container: CONVERTER_VERSION not found in scripts/dev/docs-postman.sh" >&2; exit 2; }

echo "verify-container: building $image for native Docker architecture (openapi-to-postmanv2 $converter_version)"
docker build --pull=false --build-arg "OPENAPI_TO_POSTMAN_VERSION=$converter_version" -t "$image" docker/verify

# Five named volumes persist between runs: the Go build cache, Go modules,
# the pnpm store and both node_modules trees. `make prepush-clean` removes
# every stellarindex-verify-* volume named here.
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
  -e npm_config_store_dir=/pnpm/store \
  "$image"
