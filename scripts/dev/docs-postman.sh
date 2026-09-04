#!/usr/bin/env bash
# Regenerate the Postman collection from openapi/stellar-index.v1.yaml.
#
# Writes to examples/postman/stellar-index.postman_collection.json
# — the user-facing canonical path (referenced by README.md, and the
# file users actually download from the repo).
#
# An earlier revision wrote to docs/reference/api/postman-collection.json
# (a gitignored docs-site path) and left the tracked user-facing copy
# drifting silently. Now the script writes the canonical directly.
#
# The converter is openapi-to-postmanv2 at CONVERTER_VERSION below,
# the single place that version is chosen. A pre-installed
# `openapi2postmanv2` on PATH (the verifier image carries one) is used
# only when its --version prints exactly that string; any other
# version, and any machine without one, goes through npx at the pin,
# so only Node is required. Pinned so the generated output stays
# reproducible — bumping means updating CONVERTER_VERSION, re-running
# this script and committing the diff; scripts/ci/check-verify-image-pins.sh
# holds docker/verify/Dockerfile's ARG default to the same value.

set -euo pipefail

CONVERTER_VERSION="6.0.1"

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$REPO_ROOT"

if ! command -v npx >/dev/null 2>&1; then
  echo "npx not found — install Node (https://nodejs.org/) to regenerate the Postman collection."
  echo "Source of truth: openapi/stellar-index.v1.yaml"
  exit 1
fi

mkdir -p examples/postman

# openapi-to-postmanv2 prints a sub-summary on success; pipe to
# /dev/null and rely on the exit code so this is silent on success.
#
# NODE_OPTIONS preloads postman-seed-random.js to make the converter's
# json-schema-faker deterministic (it otherwise Math.random()s its way
# through enum/format example values, so every run produced a different
# collection and the committed artifact silently drifted).
TMP=$(mktemp)

# A binary on PATH is taken only at the pinned version: any other
# converter release changes the generated collection, and PATH is not
# a version statement.
installed_version=""
if command -v openapi2postmanv2 >/dev/null 2>&1; then
  installed_version="$(openapi2postmanv2 --version 2>/dev/null || true)"
fi
if [[ "$installed_version" == "$CONVERTER_VERSION" ]]; then
  CONVERTER=(openapi2postmanv2)
else
  CONVERTER=(npx --yes "openapi-to-postmanv2@${CONVERTER_VERSION}")
fi

NODE_OPTIONS="--require ${REPO_ROOT}/scripts/dev/postman-seed-random.js" \
  "${CONVERTER[@]}" \
      -s openapi/stellar-index.v1.yaml \
      -o "$TMP" \
      -p \
      >/dev/null

# openapi-to-postmanv2 stamps a fresh UUIDv4 into every "id" field
# (and a "_postman_id" at the collection root, sourced from crypto so
# the Math.random seed above doesn't cover it) on every run. Postman
# doesn't need them — it regenerates IDs on import — and the noise
# makes the file unmergeable. Strip both so the diff is meaningful
# (only changes when the openapi spec itself changes).
if ! command -v jq >/dev/null 2>&1; then
  echo "jq not found — install jq (https://stedolan.github.io/jq/) for ID-stripping"
  exit 1
fi

CANONICAL="examples/postman/stellar-index.postman_collection.json"

# Post-process (single jq pass, all deterministic):
#   1. Strip the converter's random ids (see above).
#   2. Replace the converter's collection-level `noauth` with bearer
#      auth bound to a {{bearerToken}} collection variable. The
#      OpenAPI global security is `[{}, APIKeyAuth]` (anonymous
#      allowed), which the converter renders as noauth — shipping
#      the collection un-authable. Requests inherit collection auth
#      in Postman, so binding it here means "paste your key into the
#      bearerToken variable" is the only setup step; anonymous use
#      still works because the server accepts keyless requests on
#      public endpoints (empty token → empty header value).
jq 'walk(if type == "object" then (del(.id) | del(._postman_id)) else . end)
    | .auth = {
        type: "bearer",
        bearer: [{ key: "token", value: "{{bearerToken}}", type: "string" }]
      }
    | .variable += [{
        key: "bearerToken",
        value: "",
        type: "string",
        description: "Stellar Index API key (sip_…). Create one at https://stellarindex.io/dashboard/keys or via POST /v1/signup. Leave empty for anonymous access (lower rate limit; /v1/account/* requires a key)."
      }]' "$TMP" \
  > "$CANONICAL"
rm -f "$TMP"

echo "Generated $CANONICAL"
echo "  $(wc -c < "$CANONICAL" | tr -d ' ') bytes"
