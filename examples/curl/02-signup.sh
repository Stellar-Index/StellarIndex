#!/usr/bin/env bash
# POST /v1/signup — self-service first-key issuance.
#
# Returns a freshly minted Starter-tier API key. The plaintext is
# returned exactly once — store it. Idempotent on email: a second
# signup with the same address replays the original key_id but
# does NOT re-mint the plaintext (clients that lost the original
# response need a fresh email or operator help).
set -euo pipefail
BASE="${API_BASE_URL:-https://api.ratesengine.net}"
EMAIL="${1:-demo+$(date +%s)@example.com}"
LABEL="${2:-curl-example}"

curl -sS --fail -X POST "$BASE/v1/signup" \
  -H 'Content-Type: application/json' \
  -d "{\"email\": \"$EMAIL\", \"label\": \"$LABEL\"}"
echo
