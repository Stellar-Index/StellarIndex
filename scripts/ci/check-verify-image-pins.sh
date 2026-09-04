#!/usr/bin/env bash
# check-verify-image-pins.sh — the verifier image's converter pin must equal
# the script's.
#
# scripts/dev/docs-postman.sh names the openapi-to-postmanv2 release it
# regenerates examples/postman with (CONVERTER_VERSION) and takes a
# pre-installed `openapi2postmanv2` only when its --version prints that
# exact string. docker/verify/Dockerfile installs the converter globally
# from ARG OPENAPI_TO_POSTMAN_VERSION. scripts/dev/verify-container.sh
# threads the script's value through --build-arg, so a container run always
# matches; the ARG default is what a bare `docker build docker/verify` gets,
# and nothing else ties it to the script. Two unlinked constants drift, and
# when they do the image carries a converter the script refuses: every run
# falls back to a network npx resolve and the pre-installed copy is dead
# weight. This gate fails when the two values differ, and fails when it
# cannot read either one — a missing constant must not pass as "no drift".
#
# Env overrides (used by the self-test): DOCKERFILE, DOCS_POSTMAN_SH.
set -euo pipefail

cd "$(dirname "$0")/../.."

DOCKERFILE="${DOCKERFILE:-docker/verify/Dockerfile}"
DOCS_POSTMAN_SH="${DOCS_POSTMAN_SH:-scripts/dev/docs-postman.sh}"

for f in "$DOCKERFILE" "$DOCS_POSTMAN_SH"; do
  if [ ! -f "$f" ]; then
    echo "check-verify-image-pins: FAIL — file not found: $f" >&2
    exit 1
  fi
done

image_pin="$(sed -n 's/^ARG OPENAPI_TO_POSTMAN_VERSION=\(.*\)$/\1/p' "$DOCKERFILE")"
script_pin="$(sed -n 's/^CONVERTER_VERSION="\(.*\)"$/\1/p' "$DOCS_POSTMAN_SH")"

if [ -z "$image_pin" ]; then
  echo "check-verify-image-pins: FAIL — no 'ARG OPENAPI_TO_POSTMAN_VERSION=<version>' line in $DOCKERFILE" >&2
  exit 1
fi
if [ -z "$script_pin" ]; then
  echo "check-verify-image-pins: FAIL — no 'CONVERTER_VERSION=\"<version>\"' line in $DOCS_POSTMAN_SH" >&2
  exit 1
fi
# One definition each; a second line would make "the" pin ambiguous.
case "$image_pin" in *$'\n'*)
  echo "check-verify-image-pins: FAIL — ARG OPENAPI_TO_POSTMAN_VERSION is defined more than once in $DOCKERFILE" >&2
  exit 1 ;;
esac
case "$script_pin" in *$'\n'*)
  echo "check-verify-image-pins: FAIL — CONVERTER_VERSION is defined more than once in $DOCS_POSTMAN_SH" >&2
  exit 1 ;;
esac

if [ "$image_pin" != "$script_pin" ]; then
  echo "check-verify-image-pins: FAIL — openapi-to-postmanv2 pin differs: $DOCKERFILE has ARG OPENAPI_TO_POSTMAN_VERSION=$image_pin, $DOCS_POSTMAN_SH has CONVERTER_VERSION=\"$script_pin\"" >&2
  echo "  Change CONVERTER_VERSION in $DOCS_POSTMAN_SH, regenerate examples/postman, and set the Dockerfile ARG default to the same value." >&2
  exit 1
fi

echo "check-verify-image-pins: OK — openapi-to-postmanv2 $script_pin in both $DOCKERFILE and $DOCS_POSTMAN_SH"
