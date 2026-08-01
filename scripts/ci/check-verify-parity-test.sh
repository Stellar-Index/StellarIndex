#!/usr/bin/env bash
# check-verify-parity-test.sh — fixture tests for the verify.sh ↔ CI
# import-checks parity gate (scripts/ci/check-verify-parity.sh, W5-ci-6).
#
# The gate's whole value is being non-vacuous: it must FAIL when CI runs a
# scripts/ci gate that verify.sh doesn't mirror, and PASS when they agree —
# in BOTH drift directions (CI gains a gate; verify loses one). It must also
# not miscount the CID-03 restore step's `for f in scripts/ci/…` references
# (git-show arguments, not invocations) as invoked gates, and must refuse to
# pass vacuously if it can't find the import-checks job at all. Fixtures are
# synthetic ci.yml / verify.sh pairs — no network, no gh, no repo state.
#
# Run: bash scripts/ci/check-verify-parity-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
CHECK="$PWD/scripts/ci/check-verify-parity.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# A minimal but structurally-faithful import-checks job block. The trailing
# `actions-pinning:` job key (2-space indent) is what bounds the block, and
# the `for f in scripts/ci/lint-restored.sh …` line reproduces the CID-03
# restore step whose references must NOT be counted as invocations.
write_ci() {
  # $1..$N = gate scripts to invoke as real steps inside import-checks.
  local out="$1"; shift
  {
    echo 'jobs:'
    echo '  import-checks:'
    echo '    name: import-boundary lint'
    echo '    runs-on: ubuntu-latest'
    echo '    steps:'
    echo '      - name: restore (CID-03) — references, not invocations'
    echo '        run: |'
    echo '          for f in scripts/ci/lint-restored.sh scripts/ci/lint-alsorestored.sh; do'
    echo '            git show base-ref-copy > restored-file'
    echo '          done'
    local s
    for s in "$@"; do
      echo "      - name: ${s##*/}"
      echo "        run: ./$s"
    done
    echo '  actions-pinning:'
    echo '    name: another job'
    echo '    runs-on: ubuntu-latest'
    echo '    steps:'
    echo '      - name: not-in-import-checks'
    echo '        run: ./scripts/ci/lint-otherjob.sh'
  } > "$out"
}

write_verify() {
  # $1 = out; $2..$N = gate scripts to invoke.
  local out="$1"; shift
  {
    echo '#!/usr/bin/env bash'
    echo 'set -euo pipefail'
    local s
    for s in "$@"; do
      echo "./$s"
    done
  } > "$out"
}

run() {  # run <ci.yml> <verify.sh>
  OUT="$(CI_YML="$1" VERIFY_SH="$2" bash "$CHECK" 2>&1)"
  RC=$?
}

expect() {
  local name="$1" want_rc="$2" want_sub="${3:-}"
  if [ "$RC" -ne "$want_rc" ]; then
    echo "FAIL: $name — exit $RC, want $want_rc" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1)); return
  fi
  if [ -n "$want_sub" ] && ! printf '%s' "$OUT" | grep -q -- "$want_sub"; then
    echo "FAIL: $name — output missing '$want_sub'" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1)); return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

# ── Parity holds → OK ───────────────────────────────────────────────
write_ci "$TMP/ci-ok.yml" scripts/ci/lint-a.sh scripts/ci/lint-b.sh
write_verify "$TMP/verify-ok.sh" scripts/ci/lint-a.sh scripts/ci/lint-b.sh
run "$TMP/ci-ok.yml" "$TMP/verify-ok.sh"
expect 'CI and verify agree → OK' 0 'OK — all'

# ── verify.sh MISSING a CI gate → FAIL naming it (removal drift) ─────
write_verify "$TMP/verify-missing.sh" scripts/ci/lint-a.sh
run "$TMP/ci-ok.yml" "$TMP/verify-missing.sh"
expect 'verify.sh drops a CI gate → FAIL' 1 'scripts/ci/lint-b.sh'

# ── CI GAINS a gate verify lacks → FAIL naming it (the finding scenario)
write_ci "$TMP/ci-grew.yml" scripts/ci/lint-a.sh scripts/ci/lint-b.sh scripts/ci/lint-newthing.sh
write_verify "$TMP/verify-two.sh" scripts/ci/lint-a.sh scripts/ci/lint-b.sh
run "$TMP/ci-grew.yml" "$TMP/verify-two.sh"
expect 'CI adds a gate verify.sh lacks → FAIL' 1 'scripts/ci/lint-newthing.sh'

# ── CID-03 restore-step references must NOT count as invocations ─────
# The for-loop lists lint-restored.sh / lint-alsorestored.sh with no ./ or
# bash prefix; verify.sh does not run them and parity must still be OK.
write_verify "$TMP/verify-ok2.sh" scripts/ci/lint-a.sh scripts/ci/lint-b.sh
run "$TMP/ci-ok.yml" "$TMP/verify-ok2.sh"
expect 'for-loop git-show references are not treated as invoked gates' 0 'OK — all'

# ── A gate in ANOTHER job (actions-pinning) is out of scope → OK ─────
# lint-otherjob.sh lives in the actions-pinning job, not import-checks;
# verify.sh need not mirror it.
run "$TMP/ci-ok.yml" "$TMP/verify-ok.sh"
expect 'gate in a different job is out of import-checks scope' 0 'OK — all'

# ── verify.sh may run EXTRA gates not in CI → still OK ───────────────
write_verify "$TMP/verify-extra.sh" scripts/ci/lint-a.sh scripts/ci/lint-b.sh scripts/ci/lint-docs.sh
run "$TMP/ci-ok.yml" "$TMP/verify-extra.sh"
expect 'verify.sh running extra gates is allowed' 0 'OK — all'

# ── Non-vacuous guard: an import-checks job with zero gate invocations
#    must FAIL, not silently pass. ────────────────────────────────────
write_ci "$TMP/ci-empty.yml"   # no gate scripts
run "$TMP/ci-empty.yml" "$TMP/verify-ok.sh"
expect 'no gates found in import-checks → FAIL (not vacuous)' 1 'no scripts/ci'

# ── Missing files → FAIL (defensive) ────────────────────────────────
run "$TMP/does-not-exist.yml" "$TMP/verify-ok.sh"
expect 'missing ci.yml → FAIL' 1 'file not found'

echo
echo "check-verify-parity-test: ${pass} passed, ${fail} failed"
[ "$fail" -eq 0 ] || exit 1
