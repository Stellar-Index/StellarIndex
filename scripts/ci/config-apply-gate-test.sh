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
  OUT="$(cd "$TMP/repo" && GITHUB_STEP_SUMMARY="$TMP/summary" bash "$GATE" "$1" "${2:-false}" "${3:-}" "${4:-}" 2>&1)"
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

# --- 11. the [applied] argument: a self-applying surface is retired
# from the gate WITHOUT weakening it for anything else.
#
# configs/prometheus/rules.r1/ became self-applying (apply-rules.sh runs
# in the deploy job, and VERIFIES against /api/v1/rules rather than just
# copying). The risk of that exemption is that it is written too broadly
# and silently clears surfaces nobody applied — which would restore the
# exact 2026-09-01 failure the gate exists to catch. So the cases that
# matter are the negative ones: the exemption must not leak.
mkrepo
release configs/prometheus/rules.r1/storage.yml
runGate v0.2.0 false "" "configs/prometheus/rules.r1/"
expect "an auto-applied rules change needs no operator ack" 0 "applied and verified automatically"

# The same change WITHOUT the applied arg (e.g. a region whose deploy
# does not run the applier) must still gate. Passing here would mean the
# exemption is unconditional.
mkrepo
release configs/prometheus/rules.r1/storage.yml
runGate v0.2.0
expect "the same change still gates when nothing applied it" 1 "changed 1 config surface(s)"

# The exemption must not cover a DIFFERENT surface. rules.r1 being
# auto-applied says nothing about the multi-host rule tree, the ansible
# role, or alertmanager.
mkrepo
release deploy/monitoring/rules/storage.yml
runGate v0.2.0 false "" "configs/prometheus/rules.r1/"
expect "the exemption does not cover deploy/monitoring/rules/" 1 "changed 1 config surface(s)"

mkrepo
release configs/ansible/roles/archival-node/templates/stellarindex.toml.j2
runGate v0.2.0 false "" "configs/prometheus/rules.r1/"
expect "the exemption does not cover the ansible role" 1 "changed 1 config surface(s)"

# A release touching BOTH an applied and an unapplied surface must still
# fail — and report only the surface that is actually outstanding. This
# is the mixed case that a naive "any applied ⇒ pass" would get wrong.
mkrepo
releaseAs v0.2.0 configs/prometheus/rules.r1/storage.yml
releaseAs v0.3.0 configs/ansible/roles/archival-node/templates/stellarindex.toml.j2
runGate v0.3.0 false v0.1.0 "configs/prometheus/rules.r1/"
expect "a mixed release still gates on the surface nobody applied" 1 "changed 1 config surface(s)"
# The gate must ATTRIBUTE each surface correctly, not merely fail: the
# auto-applied one under the "applied and VERIFIED" banner, and the
# outstanding count must be 1 (the ansible template), not 2. A gate that
# failed with a count of 2 would be telling the operator to go apply a
# surface this deploy already applied and proved live.
if [[ "$OUT" != *"VERIFIED automatically"* ]]; then
  echo "FAIL: mixed release did not report the auto-applied surface as applied"
  echo "$OUT" | sed 's/^/    | /' | head -8
  fail=$((fail + 1))
elif [[ "$OUT" != *"changed 1 config surface(s)"* ]]; then
  echo "FAIL: mixed release miscounted the outstanding surfaces (want exactly 1)"
  echo "$OUT" | sed 's/^/    | /' | head -8
  fail=$((fail + 1))
elif [[ "$OUT" == *"VERIFIED automatically"*"stellarindex.toml.j2"* ]]; then
  echo "FAIL: the auto-applied banner claimed a surface this deploy never applied"
  echo "$OUT" | sed 's/^/    | /' | head -8
  fail=$((fail + 1))
else
  echo "ok: mixed release attributes each surface to the right column"
  pass=$((pass + 1))
fi

# The applied prefix is an anchored PATH PREFIX, not a substring. A
# caller passing a loose token must not exempt a sibling tree: "rules"
# must not clear deploy/monitoring/rules/. Without anchoring, grep -F
# matches mid-path and the exemption silently widens to a surface
# nothing applied.
mkrepo
release deploy/monitoring/rules/storage.yml
runGate v0.2.0 false "" "rules"
expect "a loose prefix does not exempt a sibling rule tree" 1 "changed 1 config surface(s)"

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
  # ORDERING, not mere presence. The check below used to be two
  # independent greps for `id: baseline` and `deployed-versions`, and its
  # message asserted the baseline is read BEFORE the playbook — which
  # nothing verified. Moving the baseline step below the deploy step
  # would have kept this green while making the gate permanently
  # vacuous: the "live" version would be the version just deployed, so
  # the diff range collapses to nothing and every config change passes.
  bl_line=$(grep -n 'id: baseline' "$WF" | head -1 | cut -d: -f1)
  pb_line=$(grep -nE 'name:.*(Run deploy playbook|deploy playbook)' "$WF" | head -1 | cut -d: -f1)
  if [[ -n "$bl_line" && -n "$pb_line" && "$bl_line" -lt "$pb_line" ]] \
     && grep -q 'deployed-versions' "$WF"; then
    check_caller "deploy.yml reads the host's live version BEFORE the playbook runs (baseline@L$bl_line < playbook@L$pb_line)" "ok"
  else
    check_caller "deploy.yml reads the host's live version before the playbook runs (baseline@L${bl_line:-none} playbook@L${pb_line:-none} — if the baseline is read AFTER the deploy it equals the version just deployed and the gate is vacuous)" "no"
  fi

  # The sidecars carry NO trailing newline (ansible copy `content:`), so
  # the read must supply one per file. `cat` mashes six versions into a
  # single token that a `^`-only anchor matches, which failed the gate
  # closed on every deploy and skipped the post-deploy smoke step.
  #
  # Asserted as a PROPERTY, not one spelling: the read must run `awk 1`
  # and must not `cat` the directory. #427 replaced the single-line
  # `awk 1 /var/lib/.../stellarindex-*` with a loop that skips the
  # migrate sidecar and calls `awk 1 "$f"` per file — same guarantee,
  # different text, and the old literal grep failed it.
  if grep -q 'deployed-versions' "$WF" \
     && grep -qE '(^|[^-[:alnum:]])awk 1( |")' "$WF" \
     && ! grep -qE '(^|[^[:alnum:]])cat [^|]*deployed-versions' "$WF"; then
    check_caller "deploy.yml reads the sidecars newline-safely (awk 1, not cat)" "ok"
  else
    check_caller "deploy.yml reads the sidecars newline-safely (found a bare 'cat' over deployed-versions/*, or no 'awk 1' at all — the files have no trailing newline, so N binaries concatenate into one garbage token)" "no"
  fi

  # And the version regex must be anchored at BOTH ends, so a mashed or
  # otherwise malformed sidecar is rejected rather than passed through
  # as if it were a version.
  if grep -qE "grep -E '\^v\[0-9\]\+\\\.\[0-9\]\+\\\.\[0-9\]\+\(-\[0-9A-Za-z\.\]\+\)\?\\\$'" "$WF"; then
    check_caller "deploy.yml's version filter is end-anchored" "ok"
  else
    check_caller "deploy.yml's version filter is end-anchored (a '^'-only anchor matches a concatenation like v0.1.0v0.2.0)" "no"
  fi
fi

echo
echo "config-apply-gate-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
