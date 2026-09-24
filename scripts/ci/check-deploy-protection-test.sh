#!/usr/bin/env bash
# check-deploy-protection-test.sh — fixture tests for the deploy
# approval gate assertion (deploy-approval-gate, audit-2026-07-23) and
# for assert-deploy-gate.sh, the wrapper every credential-holding job
# calls.
#
# Pins the fail-CLOSED contract: every shape that is not "a
# required_reviewers rule with at least one reviewer AND a deployment
# branch policy admitting only main" must exit non-zero. A gate that
# mistakes `"protection_rules": []` for a pass is exactly the hole this
# check was written to close; one that ignores a null branch policy lets
# a dispatch from any branch run that branch's own playbook against prod.
#
# Run: bash scripts/ci/check-deploy-protection-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
CHECK="$PWD/scripts/ci/check-deploy-protection.sh"
GATE="$PWD/scripts/ci/assert-deploy-gate.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

MAIN_ONLY='{"total_count":1,"branch_policies":[{"id":1,"name":"main","type":"branch"}]}'
CUSTOM='"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true}'
REVIEWED='"protection_rules":[{"id":1,"type":"required_reviewers","reviewers":[{"type":"User","reviewer":{"login":"op"}}],"prevent_self_review":false}]'

# run [--observe] <json-body> <env-name> [<branch-policies-body>]
run() {
  local flag=() pol=()
  if [ "$1" = "--observe" ]; then
    flag=(--observe)
    shift
  fi
  printf '%s' "$1" > "$TMP/env.json"
  if [ "$#" -ge 3 ]; then
    printf '%s' "$3" > "$TMP/policies.json"
    pol=("$TMP/policies.json")
  fi
  OUT="$(bash "$CHECK" ${flag[@]+"${flag[@]}"} "$TMP/env.json" "$2" ${pol[@]+"${pol[@]}"} 2>&1)"
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
  if [ -n "$want_sub" ] && ! grep -q -- "$want_sub" <<<"$OUT"; then
    echo "FAIL: $name — output missing '$want_sub'" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

# The shape the r1 environment actually had when the audit ran
# (gh api repos/Stellar-Index/StellarIndex/environments, 2026-07-24).
run '{"name":"r1","protection_rules":[]}' r1
expect 'no protection rules at all → fail closed' 1 'gate MISSING'

# A wait_timer is not an approval — nobody has to look at it.
run '{"name":"r1","protection_rules":[{"id":1,"type":"wait_timer","wait_timer":30}]}' r1
expect 'wait_timer alone → fail closed' 1 'gate MISSING'

# The rule exists but its reviewer list is empty: GitHub treats this as
# no gate, and so must we.
run '{"name":"r1","protection_rules":[{"id":1,"type":"required_reviewers","reviewers":[]}]}' r1
expect 'required_reviewers with zero reviewers → fail closed' 1 'gate MISSING'

run '{"name":"r1",'"$REVIEWED"','"$CUSTOM"'}' r1 "$MAIN_ONLY"
expect 'one reviewer + main-only branch policy → pass' 0 'approval gate present'

run '{"name":"r1",'"$REVIEWED"','"$CUSTOM"'}' r1 "$MAIN_ONLY"
expect 'self-review allowed is reported, not fatal' 0 'allows self-review'

run '{"name":"r1","protection_rules":[{"id":1,"type":"required_reviewers","reviewers":[{"type":"Team","reviewer":{"slug":"ops"}}],"prevent_self_review":true}],'"$CUSTOM"'}' r1 "$MAIN_ONLY"
expect 'team reviewer + prevent_self_review → pass' 0 'prevent_self_review=true'

# --- deployment branch policy -------------------------------------------
# The live shape of every environment when the gap was found: even with
# reviewers armed, a null deployment_branch_policy lets a dispatch from
# ANY branch run that branch's own workflow and playbook against prod.
run '{"name":"r1",'"$REVIEWED"',"deployment_branch_policy":null}' r1
expect 'reviewers but null branch policy → fail closed' 1 'branch policy MISSING'

run '{"name":"r1",'"$REVIEWED"'}' r1
expect 'reviewers but no branch policy key → fail closed' 1 'branch policy MISSING'

# "Protected branches" admits every protected branch, and which branches
# are protected is not visible from here: unverifiable, so refused.
run '{"name":"r1",'"$REVIEWED"',"deployment_branch_policy":{"protected_branches":true,"custom_branch_policies":false}}' r1
expect 'protected-branches policy → fail closed' 1 'branch policy NOT main-only'

run '{"name":"r1",'"$REVIEWED"','"$CUSTOM"'}' r1
expect 'custom policy but patterns not supplied → fail closed' 1 'branch policy NOT VERIFIED'

run '{"name":"r1",'"$REVIEWED"','"$CUSTOM"'}' r1 '{"total_count":2,"branch_policies":[{"id":1,"name":"main","type":"branch"},{"id":2,"name":"release/*","type":"branch"}]}'
expect 'custom policy admitting a second pattern → fail closed, names it' 1 'release/\*'

run '{"name":"r1",'"$REVIEWED"','"$CUSTOM"'}' r1 '{"total_count":1,"branch_policies":[{"id":1,"name":"main","type":"tag"}]}'
expect 'a TAG named main is not the main branch → fail closed' 1 'branch policy NOT main-only'

run '{"name":"r1",'"$REVIEWED"','"$CUSTOM"'}' r1 '{"total_count":0,"branch_policies":[]}'
expect 'custom policy with zero patterns → fail closed' 1 'branch policy NOT main-only'

run '{"name":"r1",'"$REVIEWED"','"$CUSTOM"'}' r1 '{"total_count":31,"branch_policies":[{"id":1,"name":"main","type":"branch"}]}'
expect 'truncated policy page → fail closed' 1 'branch policy NOT VERIFIED'

run '{"name":"r1",'"$REVIEWED"','"$CUSTOM"'}' r1 'not json'
expect 'garbage policy body → fail closed' 1 'branch policy NOT VERIFIED'

# --observe: scheduled read-only jobs cannot wait on a reviewer, but the
# credentials they hold are still main-only.
run --observe '{"name":"r1-observe","protection_rules":[],'"$CUSTOM"'}' r1-observe "$MAIN_ONLY"
expect '--observe: no reviewers + main-only → pass' 0 'main-only'

run --observe '{"name":"r1-observe","protection_rules":[],"deployment_branch_policy":null}' r1-observe
expect '--observe: null branch policy → fail closed' 1 'branch policy MISSING'

# Wrong environment in the response = the caller asked for the wrong
# thing; passing here would verify a gate on some OTHER environment.
run '{"name":"staging","protection_rules":[{"id":1,"type":"required_reviewers","reviewers":[{"type":"User","reviewer":{"login":"op"}}]}]}' r1
expect 'environment name mismatch → fail closed' 1 'expected'

run '{"message":"Not Found","status":"404"}' r1
expect '404 body → fail closed' 1 'NOT VERIFIED'

run 'not json at all' r1
expect 'garbage body → fail closed' 1 'not valid JSON'

run '' r1
expect 'empty body → fail closed' 1 'missing or empty'

# --- assert-deploy-gate.sh (fake gh on PATH; no network) ---------------
mkdir -p "$TMP/bin" "$TMP/gh"
cat > "$TMP/bin/gh" <<'SH'
#!/usr/bin/env bash
# Fake `gh api <path>`: serves $FAKE_GH/env.json and $FAKE_GH/policies.json.
printf '%s\n' "$*" >> "$FAKE_GH/calls"
case "$2" in
  */deployment-branch-policies*) f="$FAKE_GH/policies.json" ;;
  *) f="$FAKE_GH/env.json" ;;
esac
[ -f "$f" ] || exit 1
cat "$f"
SH
chmod +x "$TMP/bin/gh"

# gate <relaxed> <until> <env-json|-> <policies-json|-> <gate args...>
gate() {
  local relaxed="$1" until="$2" env="$3" policies="$4"
  shift 4
  rm -f "$TMP/gh/env.json" "$TMP/gh/policies.json" "$TMP/gh/calls"
  [ "$env" = "-" ] || printf '%s' "$env" > "$TMP/gh/env.json"
  [ "$policies" = "-" ] || printf '%s' "$policies" > "$TMP/gh/policies.json"
  OUT="$(PATH="$TMP/bin:$PATH" FAKE_GH="$TMP/gh" REPO=o/r \
    APPROVAL_RELAXED="$relaxed" APPROVAL_RELAXED_UNTIL="$until" \
    bash "$GATE" "$@" 2>&1)"
  RC=$?
}

gate '' '' '{"name":"r1",'"$REVIEWED"','"$CUSTOM"'}' "$MAIN_ONLY" r1
expect 'gate: reviewers + main-only → pass' 0 'approval gate present'
if grep -q 'environments/r1/deployment-branch-policies' "$TMP/gh/calls" 2>/dev/null; then
  echo "ok: gate fetched the custom branch policies"
  pass=$((pass + 1))
else
  echo "FAIL: gate did not fetch the custom branch policies" >&2
  fail=$((fail + 1))
fi

gate '' '' '{"name":"r1",'"$REVIEWED"',"deployment_branch_policy":null}' - r1
expect 'gate: null branch policy → fail closed' 1 'branch policy MISSING'

# The reviewer relaxation is exactly that: it never relaxes WHICH branch
# may reach production.
gate true 2999-01-01 '{"name":"r1","protection_rules":[],"deployment_branch_policy":null}' - r1
expect 'gate: relaxed still enforces the branch policy' 1 'branch policy MISSING'

gate true 2999-01-01 '{"name":"r1","protection_rules":[],'"$CUSTOM"'}' "$MAIN_ONLY" r1
expect 'gate: relaxed skips only the reviewer check' 0 'main-only'

gate true '' '{"name":"r1",'"$REVIEWED"','"$CUSTOM"'}' "$MAIN_ONLY" r1
expect 'gate: open-ended relaxation → fail closed' 1 'no DEPLOY_APPROVAL_RELAXED_UNTIL'

gate '' '' '{"name":"r1","protection_rules":[],'"$CUSTOM"'}' "$MAIN_ONLY" r1
expect 'gate: unrelaxed, no reviewers → fail closed' 1 'gate MISSING'

gate '' '' - - r1
expect 'gate: environments API unreadable → fail closed' 1 'Could not read'

gate '' '' '{"name":"r1",'"$REVIEWED"','"$CUSTOM"'}' - r1
expect 'gate: branch-policy API unreadable → fail closed' 1 'Could not read'

# --observe ignores the relaxation variables entirely: it never checks
# reviewers, so there is nothing for them to relax.
gate true '' '{"name":"r1-observe","protection_rules":[],'"$CUSTOM"'}' "$MAIN_ONLY" --observe r1-observe
expect 'gate --observe: main-only, no reviewers → pass' 0 'main-only'

echo
echo "check-deploy-protection-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
