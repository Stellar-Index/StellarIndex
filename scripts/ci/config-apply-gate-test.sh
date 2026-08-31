#!/usr/bin/env bash
# config-apply-gate-test.sh — fixture tests for the deploy-time
# config-apply gate.
#
# config-apply-gate.sh is what stops a binary-only deploy from silently
# shipping a feature whose config half never landed (2026-08-25
# declared-peg + rules.d). Its SURFACES list IS the gate: a config path
# it does not name is a path it will never flag. That list is pinned
# here rather than assumed, because on 2026-08-28 a node_exporter probe
# + its systemd units shipped as inline `content:` blocks in
# tasks/10-observability.yml — under tasks/, which the list did not
# cover — and only a hand check noticed the gate would have passed it.
#
#   - a release that changed no surface passes;
#   - a diff under roles/archival-node/templates/ fails un-acknowledged
#     and passes with config_acknowledged=true;
#   - a diff under roles/archival-node/tasks/ (the 2026-08-28 hole)
#     fails un-acknowledged;
#   - a diff under roles/archival-node/files/ fails un-acknowledged;
#   - (audit deploy-ansible-gate-4, 2026-08-28) a diff under
#     roles/archival-node/defaults/, handlers/, inventory/, a sibling
#     role, configs/healthchecks/, or one of the repo scripts the role
#     copies onto the host fails un-acknowledged — each of these renders
#     or lands on the host and none is applied by deploy-binary.yml;
#   - a diff that only touches Go source, or only the deploy playbook
#     the deploy itself runs, is NOT a config surface;
#   - a skip-ahead deploy diffs against the host-reported baseline (3rd
#     arg) — ancestry alone misses config changed in the skipped tags;
#   - an unresolvable version or baseline, i.e. a diff the gate cannot
#     see, FAILS rather than passing as "no changes" (fail-closed);
#   - a first release (no prior tag) skips rather than failing.
#
# Run: bash scripts/ci/config-apply-gate-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
GATE="$PWD/scripts/ci/config-apply-gate.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

# mkrepo — a throwaway repo with one tagged release (v0.1.0) carrying a
# representative file on every surface, so a later edit reads as a
# change to that surface rather than a brand-new path.
mkrepo() {
  rm -rf "$TMP/repo"
  mkdir -p "$TMP/repo"
  (
    cd "$TMP/repo" || exit 1
    git init -q .
    git config user.email t@t.invalid
    git config user.name t
    git config commit.gpgsign false
    git config tag.gpgsign false
    mkdir -p configs/ansible/roles/archival-node/{templates,tasks,files,defaults,handlers} \
      configs/ansible/roles/prometheus/templates configs/ansible/inventory \
      configs/ansible/playbooks configs/healthchecks scripts/ops scripts/dev \
      configs/prometheus/rules.r1 deploy/monitoring/rules deploy/systemd \
      deploy/clickhouse internal/x
    printf 'v1\n' > configs/ansible/roles/archival-node/templates/stellarindex.toml.j2
    printf 'v1\n' > configs/ansible/roles/archival-node/tasks/10-observability.yml
    printf 'v1\n' > configs/ansible/roles/archival-node/files/node-healthcheck.sh
    printf 'v1\n' > configs/ansible/roles/archival-node/defaults/main.yml
    printf 'v1\n' > configs/ansible/roles/archival-node/handlers/main.yml
    printf 'v1\n' > configs/ansible/roles/prometheus/templates/prometheus.yml.j2
    printf 'v1\n' > configs/ansible/inventory/r1.yml
    printf 'v1\n' > configs/ansible/playbooks/deploy-binary.yml
    printf 'v1\n' > configs/healthchecks/smoke.sh
    printf 'v1\n' > scripts/ops/config-assertions.sh
    printf 'v1\n' > scripts/dev/r1-smoke.sh
    printf 'v1\n' > deploy/systemd/stellarindex-api.service
    printf 'package x\n' > internal/x/x.go
    git add -A
    git commit -qm "base"
    git tag v0.1.0
  )
}

# releaseAs <tag> <path> — edit <path>, commit, tag <tag>.
releaseAs() {
  (
    cd "$TMP/repo" || exit 1
    printf '%s\n' "$1" > "$2"
    git add -A
    git commit -qm "change $2"
    git tag "$1"
  )
}

# release <path> — edit <path>, commit, tag v0.2.0 (the deploying version).
release() {
  releaseAs v0.2.0 "$1"
}

# runGate <version> [ack] → sets RC + OUT. GITHUB_STEP_SUMMARY is pointed
# at a scratch file so the summary block does not pollute OUT.
runGate() {
  OUT="$(cd "$TMP/repo" && GITHUB_STEP_SUMMARY="$TMP/summary" bash "$GATE" "$1" "${2:-false}" "${3:-}" 2>&1)"
  RC=$?
}

# expect <name> <want-rc> [want-substring]
expect() {
  local name="$1" want_rc="$2" want_sub="${3:-}"
  if [[ "$RC" -ne "$want_rc" ]]; then
    echo "FAIL: $name — rc=$RC want=$want_rc"
    echo "$OUT" | sed 's/^/    | /' | head -12
    fail=$((fail + 1))
    return
  fi
  if [[ -n "$want_sub" && "$OUT" != *"$want_sub"* ]]; then
    echo "FAIL: $name — output missing: $want_sub"
    echo "$OUT" | sed 's/^/    | /' | head -12
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

# --- 1. no surface changed passes -----------------------------------
mkrepo
release internal/x/x.go
runGate v0.2.0
expect "Go-only release is not a config surface" 0 "no config-surface changes"

# --- 2. templates/ change fails un-acked, passes acked --------------
mkrepo
release configs/ansible/roles/archival-node/templates/stellarindex.toml.j2
runGate v0.2.0
expect "templates/ change fails un-acknowledged" 1 "changed 1 config surface(s)"
runGate v0.2.0 true
expect "templates/ change passes with config_acknowledged=true" 0 "config-apply acknowledged"

# --- 3. THE 2026-08-28 HOLE: tasks/ carries inline config ----------
mkrepo
release configs/ansible/roles/archival-node/tasks/10-observability.yml
runGate v0.2.0
expect "tasks/ change fails un-acknowledged" 1 "changed 1 config surface(s)"

# --- 4. files/ carries the role's scripts ---------------------------
mkrepo
release configs/ansible/roles/archival-node/files/node-healthcheck.sh
runGate v0.2.0
expect "files/ change fails un-acknowledged" 1 "changed 1 config surface(s)"

# --- 5. a non-ansible surface (systemd) is still covered -----------
mkrepo
release deploy/systemd/stellarindex-api.service
runGate v0.2.0
expect "deploy/systemd/ change fails un-acknowledged" 1 "changed 1 config surface(s)"

# --- 6. first release (no prior tag) skips ---------------------------
mkrepo
runGate v0.1.0
expect "no previous tag skips" 0 "skipping config-drift gate"

# --- 7. deploy-ansible-gate-4: surfaces the role renders/copies that the
#        list did not name. One fixture per added entry; a defaults-only
#        release (e.g. a galexie_ledgers_per_file bump that re-renders
#        galexie.toml) used to pass as "binary deploy is complete".
for p in \
  configs/ansible/roles/archival-node/defaults/main.yml \
  configs/ansible/roles/archival-node/handlers/main.yml \
  configs/ansible/roles/prometheus/templates/prometheus.yml.j2 \
  configs/ansible/inventory/r1.yml \
  configs/healthchecks/smoke.sh \
  scripts/ops/config-assertions.sh \
  scripts/dev/r1-smoke.sh; do
  mkrepo
  release "$p"
  runGate v0.2.0
  expect "$p change fails un-acknowledged" 1 "changed 1 config surface(s)"
done

# --- 8. the deploy playbook is what the deploy RUNS — applied, not dead;
#        listing it would make every deploy-mechanics change a false ack.
mkrepo
release configs/ansible/playbooks/deploy-binary.yml
runGate v0.2.0
expect "playbooks/deploy-binary.yml change is not a config surface" 0 "no config-surface changes"

# --- 9. skip-ahead: host is on v0.1.0, v0.2.0 changed defaults/, we deploy
#        v0.3.0 (Go-only). Ancestry diffs v0.2.0..v0.3.0 and sees nothing;
#        the host-reported baseline sees the v0.2.0 config change.
mkrepo
releaseAs v0.2.0 configs/ansible/roles/archival-node/defaults/main.yml
releaseAs v0.3.0 internal/x/x.go
runGate v0.3.0 false v0.1.0
expect "skip-ahead deploy fails against host baseline" 1 "changed 1 config surface(s)"
expect "skip-ahead names the host baseline" 1 "baseline: v0.1.0 (host-reported live version)"
runGate v0.3.0 true v0.1.0
expect "skip-ahead passes with config_acknowledged=true" 0 "config-apply acknowledged"
runGate v0.3.0
expect "ancestry baseline is printed when no host version is given" 0 "baseline: v0.2.0 (previous release tag by ancestry"

# --- 10. fail-closed: a diff the gate cannot see is an error, not a green.
mkrepo
release internal/x/x.go
runGate vNOPE
expect "unresolvable version fails closed" 1 "does not resolve to a commit"
runGate v0.2.0 false vNOPE
expect "unresolvable host baseline fails closed" 1 "does not resolve to a commit"

# ─── The CALLER must pass the baseline ──────────────────────────────
#
# Every case above exercises the SCRIPT, which has always handled a 3rd
# argument correctly. The defect was that .github/workflows/deploy.yml
# called it with TWO — so the gate diffed against "the previous release
# tag by ancestry" instead of the host's live version. That is only the
# same thing when the fleet is exactly one release behind, and
# deployed-versions.md states plainly that a tag cut does not imply the
# fleet moved to it.
#
# Measured on real tags: the 2-arg form reports "no config-surface
# changes between v0.47.1 and v0.47.2 — the binary deploy is complete"
# and exits 0, while the 3-arg form against a real host baseline of
# v0.45.0 finds THIRTEEN changed config surfaces and exits 1 (wave-D
# LID-5).
#
# A script-level test cannot catch a caller-level omission, so check the
# caller.
check_caller() {
  local desc="$1" cond="$2"
  if [[ "$cond" == "ok" ]]; then
    printf 'ok: %s\n' "$desc"; pass=$((pass + 1))
  else
    printf 'FAIL: %s\n' "$desc"; fail=$((fail + 1))
  fi
}

# The cases above run inside a throwaway fixture repo, so resolve the
# workflow from THIS script's own location, not the working directory.
WF="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/.github/workflows/deploy.yml"
if [[ ! -f "$WF" ]]; then
  check_caller "deploy workflow present" "no"
else
  call=$(grep -hE 'config-apply-gate\.sh' "$WF" | grep -vE '^\s*#' | head -1)
  argc=$(printf '%s' "$call" | sed -E 's|.*config-apply-gate\.sh||' | grep -o '"\$[A-Z_]*"' | wc -l | tr -d ' ')
  if [[ -n "$call" && "$argc" -ge 3 ]]; then
    check_caller "deploy.yml passes the host baseline as the 3rd argument" "ok"
  else
    check_caller "deploy.yml passes the host baseline as the 3rd argument (got $argc args — without it a catch-up or skip-ahead deploy gets a false 'the binary deploy is complete')" "no"
  fi
  if grep -q 'id: baseline' "$WF" && grep -q 'deployed-versions' "$WF"; then
    check_caller "deploy.yml reads the host's live version before the playbook runs" "ok"
  else
    check_caller "deploy.yml reads the host's live version before the playbook runs (else the 3rd argument is always empty)" "no"
  fi
fi

echo
echo "config-apply-gate-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
