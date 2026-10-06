#!/usr/bin/env bash
# smoke-tmp-cleanup-test.sh — smoke.sh must leave no api_smoke.prom.tmp.*
# file behind when the atomic write fails (here: mv forced to fail).
#
# Run: bash configs/healthchecks/smoke-tmp-cleanup-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

mkdir -p "$TMP/bin" "$TMP/tf"
printf '#!/usr/bin/env bash\nexit 1\n' > "$TMP/bin/mv"
chmod +x "$TMP/bin/mv"
printf '#!/usr/bin/env bash\nexit 0\n' > "$TMP/smoke-stub.sh"
chmod +x "$TMP/smoke-stub.sh"

PATH="$TMP/bin:$PATH" TEXTFILE_DIR="$TMP/tf" SMOKE_SCRIPT="$TMP/smoke-stub.sh" \
  bash configs/healthchecks/smoke.sh >/dev/null 2>&1

left="$(find "$TMP/tf" -name 'api_smoke.prom*' | wc -l | tr -d ' ')"
if [ "$left" = "0" ]; then
  echo "  ok   no tmp file survives a failed rename"
else
  echo "  FAIL $left file(s) left behind:"
  ls "$TMP/tf"
  exit 1
fi
