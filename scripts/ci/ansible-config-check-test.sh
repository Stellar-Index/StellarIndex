#!/usr/bin/env bash
# ansible-config-check-test.sh — the archival-node role must not replace /etc/stellarindex.toml with a file the
# installed binaries refuse to boot on (they reject unknown keys and crash-loop on the restart that follows).
#
# Runs the role's REAL tasks/stellarindex-toml.yml (ansible-playbook -c local) against stub binaries in a temp
# bin dir, with the live file in a temp dir:
#   1. no binary installed (first install): validation is skipped and the file is written;
#   2. a render the installed stellarindex-ops accepts replaces the live file;
#   3. a render it refuses fails the play and leaves the live file byte-identical;
#   4. a stellarindex-ops older than config-check fails closed, live file untouched;
#   5. a config-reading daemon at another version than stellarindex-ops fails closed, live file untouched;
#   6. --check on a good host passes and writes nothing.
# The copied tasks file drops its `owner: root`/`group: root` lines so it runs unprivileged; nothing else changes.
#
# Needs ansible-playbook (the ci.yml ansible-check job has it).
# Run: bash scripts/ci/ansible-config-check-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ANSIBLE_DIR="${ANSIBLE_DIR:-$PWD/configs/ansible}"   # override for the red-proof against a pre-fix copy
TASKS="$ANSIBLE_DIR/roles/archival-node/tasks/stellarindex-toml.yml"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

if ! command -v ansible-playbook >/dev/null; then
  echo "ansible-config-check-test: FAIL — ansible-playbook not on PATH (this test must not pass vacuously)" >&2
  exit 1
fi
if [ ! -f "$TASKS" ]; then
  echo "ansible-config-check-test: FAIL — $TASKS not found" >&2
  exit 1
fi

R="$TMP/roles/t"
mkdir -p "$R/tasks" "$R/templates" "$R/handlers" "$TMP/live"
grep -vE '^[[:space:]]+(owner|group):[[:space:]]+root[[:space:]]*$' "$TASKS" > "$R/tasks/stellarindex-toml.yml"
printf -- '---\n- ansible.builtin.import_tasks: stellarindex-toml.yml\n' > "$R/tasks/main.yml"
printf '{{ body }}\n' > "$R/templates/stellarindex.toml.j2"
cat > "$R/handlers/main.yml" <<'YML'
---
- {name: Restart stellarindex-indexer, ansible.builtin.debug: {msg: restart}}
- {name: Restart stellarindex-aggregator, ansible.builtin.debug: {msg: restart}}
- {name: Restart stellarindex-api, ansible.builtin.debug: {msg: restart}}
YML
cat > "$TMP/play.yml" <<'YML'
- hosts: localhost
  gather_facts: false
  roles: [t]
YML

LIVE="$TMP/live/stellarindex.toml"

# install <ops-version|none|old> <daemon-version|none>: fresh stub binaries. The ops stub refuses a file
# carrying `unknown_key`, as config.Load does.
install() {
  rm -rf "${TMP:?}/bin"; mkdir -p "$TMP/bin"
  if [ "$1" != none ]; then
    cat > "$TMP/bin/stellarindex-ops" <<SH
#!/bin/sh
case "\$1" in
  version) echo "$1 (2026-01-01, go1.0)" ;;
  config-check)
    if [ "$1" = old ]; then echo "stellarindex-ops: unknown subcommand \\"config-check\\"" >&2; exit 2; fi
    [ "\$2" = -h ] && exit 0
    if grep -q unknown_key "\$3"; then echo "config: unknown keys in \$3: api.unknown_key" >&2; exit 1; fi
    echo "config-check: ok \$3" ;;
  *) exit 2 ;;
esac
SH
    chmod +x "$TMP/bin/stellarindex-ops"
  fi
  if [ "$2" != none ]; then
    for b in stellarindex-api stellarindex-indexer stellarindex-aggregator; do
      printf '#!/bin/sh\necho "%s (2026-01-02, go1.1)"\n' "$2" > "$TMP/bin/$b"
      chmod +x "$TMP/bin/$b"
    done
  fi
}

# apply <body> [extra ansible-playbook args]: rc in $rc, output in $TMP/out.
apply() {
  local body=$1; shift
  ANSIBLE_ROLES_PATH="$TMP/roles" ANSIBLE_LOCALHOST_WARNING=false ANSIBLE_INVENTORY_UNPARSED_WARNING=false \
    ANSIBLE_PYTHON_INTERPRETER=auto_silent ANSIBLE_LOCAL_TEMP="$TMP/ansible-tmp" \
    ansible-playbook -c local -i localhost, "$TMP/play.yml" \
      -e "{\"body\": \"$body\", \"stellarindex_toml_dest\": \"$LIVE\", \"stellarindex_toml_bin_dir\": \"$TMP/bin\"}" \
      "$@" </dev/null >"$TMP/out" 2>&1
  rc=$?
}

live_is() { [ -f "$LIVE" ] && [ "$(cat "$LIVE")" = "$1" ]; }

# 1. first install
rm -f "$LIVE"; install none none; apply "region = 1"
if [ "$rc" -eq 0 ] && live_is "region = 1"; then ok "first install: no binary, validation skipped, file written"
else bad "first install (rc=$rc)"; tail -15 "$TMP/out"; fi

# 2. accepted render replaces the live file
printf 'old = 1\n' > "$LIVE"; install v1.0.0 v1.0.0; apply "region = 2"
if [ "$rc" -eq 0 ] && live_is "region = 2"; then ok "accepted render replaces the live file"
else bad "accepted render (rc=$rc)"; tail -15 "$TMP/out"; fi

# 3. refused render leaves the live file untouched
printf 'old = 1\n' > "$LIVE"; install v1.0.0 v1.0.0; apply "unknown_key = 1"
if [ "$rc" -ne 0 ] && live_is "old = 1" && grep -q "api.unknown_key" "$TMP/out"; then
  ok "refused render fails the play, names the key, live file untouched"
else bad "refused render (rc=$rc, live=$(cat "$LIVE"))"; tail -15 "$TMP/out"; fi

# 4. stellarindex-ops older than config-check
printf 'old = 1\n' > "$LIVE"; install old v1.0.0; apply "region = 4"
if [ "$rc" -ne 0 ] && live_is "old = 1" && grep -q "predates config-check: deploy the binary first" "$TMP/out"; then
  ok "old stellarindex-ops fails closed with the deploy-first message, live file untouched"
else bad "old stellarindex-ops (rc=$rc, live=$(cat "$LIVE"))"; tail -15 "$TMP/out"; fi

# 5. a daemon at another version than stellarindex-ops
printf 'old = 1\n' > "$LIVE"; install v1.1.0 v1.0.0; apply "region = 5"
if [ "$rc" -ne 0 ] && live_is "old = 1" && grep -q "deploy this release's binaries first" "$TMP/out"; then
  ok "version skew fails closed, live file untouched"
else bad "version skew (rc=$rc, live=$(cat "$LIVE"))"; tail -15 "$TMP/out"; fi

# 6. --check writes nothing
printf 'old = 1\n' > "$LIVE"; install v1.0.0 v1.0.0; apply "region = 6" --check
if [ "$rc" -eq 0 ] && live_is "old = 1"; then ok "--check passes and writes nothing"
else bad "--check (rc=$rc, live=$(cat "$LIVE"))"; tail -15 "$TMP/out"; fi

echo "ansible-config-check-test: $pass passed, $fail failed (6 cases)"
[ "$fail" -eq 0 ]
