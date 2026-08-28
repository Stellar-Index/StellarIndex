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
#   - a diff that only touches Go source is NOT a config surface;
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
    mkdir -p configs/ansible/roles/archival-node/{templates,tasks,files} \
      configs/prometheus/rules.r1 deploy/monitoring/rules deploy/systemd \
      deploy/clickhouse internal/x
    printf 'v1\n' > configs/ansible/roles/archival-node/templates/stellarindex.toml.j2
    printf 'v1\n' > configs/ansible/roles/archival-node/tasks/10-observability.yml
    printf 'v1\n' > configs/ansible/roles/archival-node/files/node-healthcheck.sh
    printf 'v1\n' > deploy/systemd/stellarindex-api.service
    printf 'package x\n' > internal/x/x.go
    git add -A
    git commit -qm "base"
    git tag v0.1.0
  )
}

# release <path> — edit <path>, commit, tag v0.2.0 (the deploying version).
release() {
  (
    cd "$TMP/repo" || exit 1
    printf 'v2\n' > "$1"
    git add -A
    git commit -qm "change $1"
    git tag v0.2.0
  )
}

# runGate <version> [ack] → sets RC + OUT. GITHUB_STEP_SUMMARY is pointed
# at a scratch file so the summary block does not pollute OUT.
runGate() {
  OUT="$(cd "$TMP/repo" && GITHUB_STEP_SUMMARY="$TMP/summary" bash "$GATE" "$1" "${2:-false}" 2>&1)"
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

echo
echo "config-apply-gate-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
