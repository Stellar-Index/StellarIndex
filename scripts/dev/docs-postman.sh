#!/usr/bin/env bash
# Regenerate the Postman collection from openapi/stellar-index.v1.yaml.
#
# Writes examples/postman/stellar-index.postman_collection.json, the canonical
# user-facing path (referenced by README.md, the file users download).
#
# The converter is openapi-to-postmanv2 at CONVERTER_VERSION below, the single
# place that version is chosen. A pre-installed `openapi2postmanv2` on PATH
# (the verifier image carries one) is used only when its --version prints
# exactly that string; otherwise npx at the pin, so only Node is required.
# Pinned for reproducible output: bumping means updating CONVERTER_VERSION,
# re-running this script and committing the diff;
# scripts/ci/check-verify-image-pins.sh holds docker/verify/Dockerfile's ARG
# default to the same value.

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

# The converter prints a sub-summary on success; stdout goes to /dev/null and
# the exit code carries the result.
#
# NODE_OPTIONS preloads postman-seed-random.js so the converter's
# json-schema-faker is deterministic (it otherwise Math.random()s enum/format
# example values and the committed artifact drifts on every run).
TMP=$(mktemp)

# A PATH binary is taken only at the pinned version: any other converter
# release changes the output, and PATH is not a version statement.
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

# The converter stamps a fresh UUIDv4 into every "id" (and a "_postman_id" at
# the root, from crypto, so the Math.random seed above does not cover it) on
# every run. Postman regenerates IDs on import and the noise makes the file
# unmergeable; strip both so the diff changes only when the spec does.
if ! command -v jq >/dev/null 2>&1; then
  echo "jq not found — install jq (https://stedolan.github.io/jq/) for ID-stripping"
  exit 1
fi

CANONICAL="examples/postman/stellar-index.postman_collection.json"

# Post-process (single jq pass, deterministic):
#   1. Strip the random ids (see above).
#   2. Replace the collection-level `noauth` with bearer auth bound to a
#      {{bearerToken}} collection variable. The OpenAPI global security is
#      `[{}, APIKeyAuth]` (anonymous allowed), which the converter renders as
#      noauth, leaving the collection un-authable. Requests inherit collection
#      auth, so "paste your key into bearerToken" is the only setup step;
#      anonymous use still works (empty token → empty header value, accepted on
#      public endpoints).
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
