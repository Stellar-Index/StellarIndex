#!/usr/bin/env bash
# prepull-integration-images.sh - make every image the Docker-backed integration suite starts present
# locally BEFORE any test runs, or fail the step loudly.
#
# Why: the suite's testcontainers pull through TESTCONTAINERS_HUB_IMAGE_NAME_PREFIX (a registry
# mirror). A mirror that lacks a tag, or hiccups once, fails the pull inside the first test that
# needs the image; the ClickHouse harness caches that error in a sync.Once, so every later test in
# the shard fails at container create with "No such image".
#
# Per image: pull <prefix><image> with bounded retries; if the mirror cannot supply it, pull the
# un-mirrored name and `docker tag` it to the mirrored name so the substitutor's name resolves.
#
# The image list is read from the Go constants/literals the tests use (no second copy of the tags).
#
# Usage: scripts/ci/prepull-integration-images.sh
# Env:
#   TESTCONTAINERS_HUB_IMAGE_NAME_PREFIX  mirror prefix (e.g. mirror.gcr.io/); empty = no mirror.
#   PREPULL_ATTEMPTS                      attempts per source (default 3).
#   PREPULL_LIST_ONLY=1                   print the derived image list and exit (self-check).
set -euo pipefail

cd "$(dirname "$0")/../.."

PREFIX="${TESTCONTAINERS_HUB_IMAGE_NAME_PREFIX:-}"
ATTEMPTS="${PREPULL_ATTEMPTS:-3}"

die() { echo "prepull-integration-images: FAIL - $*" >&2; exit 1; }

# One extraction per source of truth. Each must yield at least one image.
extract() { # file, ERE for the quoted image string
  grep -ohE "$2" "$1" | tr -d '"' || true
}
images="$(
  {
    extract test/harness/timescale.go 'TimescaleImage = "[^"]+"' | sed 's/^TimescaleImage = //'
    extract test/integration/clickhouse_harness_test.go 'clickhouseImage = "[^"]+"' | sed 's/^clickhouseImage = //'
    extract test/integration/redis_test.go '"redis:[^"]+"'
  } | LC_ALL=C sort -u
)"
for want in timescale/ clickhouse/ redis:; do
  grep -q "^${want}" <<<"$images" || die "no image matching '${want}*' found in the Go sources - did a constant move?"
done
if [ "${PREPULL_LIST_ONLY:-0}" = "1" ]; then printf '%s\n' "$images"; exit 0; fi

command -v docker >/dev/null || die "docker not on PATH"

pull_retry() { # ref
  local i
  for ((i = 1; i <= ATTEMPTS; i++)); do
    if docker pull --quiet "$1" >/dev/null; then return 0; fi
    echo "prepull-integration-images: pull $1 failed (attempt $i/$ATTEMPTS)" >&2
    [ "$i" -lt "$ATTEMPTS" ] && sleep $((i * 10))
  done
  return 1
}

while IFS= read -r img; do
  [ -n "$img" ] || continue
  want="${PREFIX}${img}"
  if pull_retry "$want"; then
    echo "prepull-integration-images: $want ok"
  elif [ -n "$PREFIX" ]; then
    echo "::warning::mirror lacks $want; falling back to docker.io/$img"
    pull_retry "docker.io/$img" || die "cannot obtain $img from the mirror or Docker Hub"
    docker tag "docker.io/$img" "$want"
    echo "prepull-integration-images: $want ok (tagged from docker.io/$img)"
  else
    die "cannot obtain $img"
  fi
  docker image inspect "$want" >/dev/null || die "$want not present after pull"
done <<<"$images"
