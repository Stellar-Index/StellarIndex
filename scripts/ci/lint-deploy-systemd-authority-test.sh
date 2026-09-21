#!/usr/bin/env bash
# lint-deploy-systemd-authority-test.sh — fixture tests for the
# reference-vs-template content check (RLT-434).
#
# Being templated used to be the whole REFERENCE test: any .j2 sharing a
# unit's filename made the gate skip it, no matter what either file
# said. The load-bearing case below is a reference copy and its .j2
# twin that share one directive (RestartSec) with two different
# values — the exact shape RestartSec/User/VERIFY_ARCHIVE_MAX_RUNTIME
# drifted in for real (issue #818) — and asserts the gate now fails on
# it, undeclared, and passes once it's declared in DIVERGENCES.
#
# Run: bash scripts/ci/lint-deploy-systemd-authority-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
GATE="$PWD/scripts/ci/lint-deploy-systemd-authority.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

DEPLOY_DIR="$TMP/deploy_systemd"
ROLE_DIR="$TMP/role"
mkdir -p "$DEPLOY_DIR" "$ROLE_DIR/tasks" "$ROLE_DIR/templates/systemd"

pass=0
fail=0

# A task that installs one unrelated unit FROM deploy/systemd, so the
# authoritative set is non-empty and the gate's own "parser broke" guard
# doesn't fire — this fixture is about the REFERENCE path, not that one.
cat > "$ROLE_DIR/tasks/10-install.yml" <<'EOF'
- name: install the harness unit
  ansible.builtin.copy:
    src: "{{ playbook_dir }}/../../../deploy/systemd/{{ item }}"
    dest: "/etc/systemd/system/{{ item }}"
  loop:
    - harness.service
EOF
cat > "$DEPLOY_DIR/harness.service" <<'EOF'
[Service]
ExecStart=/usr/local/bin/harness
EOF

# run — invoke the gate against the fixture tree; sets RC + OUT.
run() {
  OUT="$(DEPLOY_SYSTEMD_DIR="$DEPLOY_DIR" DEPLOY_SYSTEMD_ROLE="$ROLE_DIR" bash "$GATE" 2>&1)"
  RC=$?
}

# expect <name> <want-rc> [<want-substring>]
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

# ── 1. The defect: same directive, two values, undeclared ────────────
cat > "$DEPLOY_DIR/example.service" <<'EOF'
[Service]
RestartSec=5s
ExecStart=/usr/local/bin/example
EOF
cat > "$ROLE_DIR/templates/systemd/example.service.j2" <<'EOF'
[Service]
RestartSec=10s
ExecStart=/usr/local/bin/example
EOF
rm -f "$DEPLOY_DIR/ORPHANS" "$DEPLOY_DIR/DIVERGENCES"

run
expect 'undeclared RestartSec divergence fails the gate' 1 'RestartSec'

# ── 2. Declaring it closes the gate, but keeps it visible ────────────
cat > "$DEPLOY_DIR/DIVERGENCES" <<'EOF'
example.service:RestartSec
EOF
run
expect 'declared divergence passes' 0 'known divergence'
expect 'declared divergence still names the key' 0 'RestartSec'

# ── 3. Undeclare one key, another undeclared key on the same unit ────
# still fails — declaring is per-directive, not per-unit.
cat > "$ROLE_DIR/templates/systemd/example.service.j2" <<'EOF'
[Service]
RestartSec=10s
ExecStart=/usr/local/sbin/wrapper example
EOF
run
expect 'a second undeclared key on an already-declared unit still fails' 1 'ExecStart'

# ── 4. No divergence at all: identical directives pass clean ─────────
cat > "$DEPLOY_DIR/DIVERGENCES" <<'EOF'
EOF
cat > "$ROLE_DIR/templates/systemd/example.service.j2" <<'EOF'
[Service]
RestartSec=5s
ExecStart=/usr/local/bin/example
EOF
run
expect 'identical reference and template pass with no divergence noise' 0 ''
if grep -q 'DIVERGED' <<<"$OUT"; then
  echo "FAIL: identical units must not report a divergence" >&2
  fail=$((fail + 1))
else
  echo "ok: identical units report no divergence"
  pass=$((pass + 1))
fi

# ── 5. A templated value (Jinja) is not a false-positive divergence ──
cat > "$ROLE_DIR/templates/systemd/example.service.j2" <<'EOF'
[Service]
RestartSec={{ example_restart_sec }}
ExecStart=/usr/local/bin/example
EOF
run
expect 'a Jinja-templated directive is not flagged as diverged' 0 ''
if grep -q 'DIVERGED' <<<"$OUT"; then
  echo "FAIL: a templated directive must not be compared literally" >&2
  fail=$((fail + 1))
else
  echo "ok: templated directive skipped, not flagged"
  pass=$((pass + 1))
fi

echo "lint-deploy-systemd-authority-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
