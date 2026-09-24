#!/usr/bin/env bash
# lint-deploy-credentials-test.sh — fixtures for lint-deploy-credentials.py.
#
# Pins that the lint catches each way a production credential can reach a
# job without passing the environment gate, and passes the gated shape.
#
# Run: bash scripts/ci/lint-deploy-credentials-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-deploy-credentials.py"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# run <workflow-yaml>: lint a one-workflow directory.
run() {
  rm -rf "$TMP/wf"
  mkdir -p "$TMP/wf"
  printf '%s\n' "$1" > "$TMP/wf/w.yml"
  OUT="$(python3 "$LINT" "$TMP/wf" 2>&1)"
  RC=$?
}

expect() {
  local name="$1" want_rc="$2" want_sub="${3:-}"
  if [ "$RC" -ne "$want_rc" ]; then
    echo "FAIL: $name — exit $RC, want $want_rc" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  if [ -n "$want_sub" ] && ! grep -qF -- "$want_sub" <<<"$OUT"; then
    echo "FAIL: $name — output missing '$want_sub'" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

GATE_STEP='      - run: bash scripts/ci/assert-deploy-gate.sh r1'
KEY_STEP="      - env:
          SSH_KEY: \${{ secrets.DEPLOY_SSH_PRIVATE_KEY }}
        run: echo x"

run "jobs:
  j:
    runs-on: ubuntu-latest
    environment: r1
    steps:
$GATE_STEP
$KEY_STEP"
expect 'environment + gate before the key → pass' 0 '1 credential-holding job(s) checked, 0 violation(s)'

# The ansible-drift.yml shape the gap was found in.
run "jobs:
  j:
    runs-on: ubuntu-latest
    steps:
$KEY_STEP"
expect 'no environment, no gate → fail' 1 "declares no \`environment:\`"
expect '…and names the missing gate' 1 'never runs scripts/ci/assert-deploy-gate.sh'

run "jobs:
  j:
    environment: r1
    steps:
$KEY_STEP
$GATE_STEP"
expect 'gate AFTER the credential step → fail' 1 'before the gate step 2'

run "jobs:
  j:
    environment: r1
    env:
      TOKEN: \${{ secrets.CLOUDFLARE_API_TOKEN }}
    steps:
$GATE_STEP"
expect 'credential in job-level env → fail' 1 'at job level'

run "jobs:
  j:
    environment: docs-production
    steps:
      - uses: cloudflare/wrangler-action@v4
        with:
          apiToken: \${{ secrets['CLOUDFLARE_API_TOKEN'] }}"
expect 'bracket-indexed credential, no gate → fail' 1 'never runs'

run "jobs:
  j:
    uses: ./.github/workflows/other.yml
    secrets: inherit"
expect 'secrets: inherit → fail' 1 'passes every secret on'

run "jobs:
  j:
    runs-on: ubuntu-latest
    steps:
      - env:
          TOKEN: \${{ secrets.GITHUB_TOKEN }}
        run: echo x"
expect 'non-production secret → not a credential job' 0 '0 credential-holding job(s)'

rm -rf "$TMP/wf"
mkdir -p "$TMP/wf"
OUT="$(python3 "$LINT" "$TMP/wf" 2>&1)"
RC=$?
expect 'empty workflows dir → fail, never vacuous' 1 'no workflows'

OUT="$(python3 "$LINT" 2>&1)"
RC=$?
expect 'the real .github/workflows passes' 0 '0 violation(s)'

echo
echo "lint-deploy-credentials-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
