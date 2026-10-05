#!/usr/bin/env bash
# lint-ch-client-auth-test.sh — self-test for lint-ch-client-auth.py.
#
# Plants a fake role that installs scripts into /usr/local/{bin,sbin} (by
# role file, by {{ playbook_dir }} path into scripts/, and inline content)
# and asserts the lint reports exactly the calls that carry no credential
# file, and nothing for the ones that do. The real tree must scan at least
# one installed script, so a checker that stopped matching fails here.
#
# Run: bash scripts/ci/lint-ch-client-auth-test.sh
set -euo pipefail
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
LINT="$REPO_ROOT/scripts/ci/lint-ch-client-auth.py"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
fail=0
check() { # name, command...
  local name="$1"; shift
  if "$@"; then echo "ok   $name"; else echo "FAIL $name"; fail=1; fi
}
# shellcheck disable=SC2329  # invoked indirectly via check
has() { grep -q -- "$1" <<<"$out"; }
# shellcheck disable=SC2329  # invoked indirectly via check
lacks() { ! has "$1"; }
# shellcheck disable=SC2329  # invoked indirectly via check
real_scanned() { [ "$rc" -eq 0 ] && grep -qE ' in [1-9][0-9]* installed' <<<"$real"; }

mkdir -p "$TMP/configs/ansible/roles/fake/tasks" "$TMP/configs/ansible/roles/fake/files" \
  "$TMP/configs/ansible/playbooks" "$TMP/scripts/ops"

cat > "$TMP/configs/ansible/roles/fake/tasks/main.yml" <<'EOF'
---
- name: Install bad role script
  ansible.builtin.copy:
    src: bad.sh
    dest: /usr/local/sbin/bad.sh
    mode: "0755"
- name: Install good repo script
  ansible.builtin.copy:
    src: "{{ playbook_dir }}/../../../scripts/ops/good.sh"
    dest: /usr/local/bin/good.sh
    mode: "0755"
- name: Grouped
  block:
    - name: Install inline script
      ansible.builtin.copy:
        dest: /usr/local/bin/inline.sh
        mode: "0755"
        content: |
          #!/bin/sh
          clickhouse-client -q "SELECT 1"
- name: Not installed into a bin dir
  ansible.builtin.copy:
    src: bad.sh
    dest: /opt/elsewhere/bad.sh
    mode: "0755"
EOF

cat > "$TMP/configs/ansible/roles/fake/files/bad.sh" <<'EOF'
#!/usr/bin/env bash
CH_HTTP="${CH_HTTP:-http://127.0.0.1:8123/}"
# clickhouse-client in a comment is not a call
echo "clickhouse-client failed" >&2
command -v clickhouse-client >/dev/null || exit 0
CH() { clickhouse-client --port 9300 "$@"; }
ch() { curl -sSf "$CH_HTTP" --data-binary "$1"; }
x=$(curl -sS --max-time 5 \
  http://localhost:8123/ --data-binary 'SELECT 1')
curl -C - -sS http://localhost:8123/ --data-binary 'SELECT 2'
curl -sS https://hc-ping.example/abc
EOF

cat > "$TMP/scripts/ops/good.sh" <<'EOF'
#!/usr/bin/env bash
F=/etc/clickhouse-client/ops-batch.xml
CH_AUTH=(); [ -r "$F" ] && CH_AUTH=(-C "$F")
NETRC=(--netrc-file /etc/clickhouse-client/ops-monitor.netrc)
CH_HTTP="${CH_HTTP:-http://127.0.0.1:8123/}"
CH() { clickhouse-client ${CH_AUTH[@]+"${CH_AUTH[@]}"} "$@"; }
clickhouse-client -C /etc/clickhouse-client/ops-monitor.xml -q "SELECT 1"
clickhouse-client --config-file=/etc/clickhouse-client/ops-monitor.xml -q "SELECT 1"
ch() { curl -sSf "${NETRC[@]}" "$CH_HTTP" --data-binary "$1"; }
curl -sS --netrc-file /etc/clickhouse-client/ops-monitor.netrc \
  http://127.0.0.1:8123/ --data-binary 'SELECT 1'
EOF

set +e
out="$(cd "$TMP" && python3 "$LINT")"
rc=$?
set -e
check "report-only exit 0 with findings" [ "$rc" -eq 0 ]
check "bare clickhouse-client reported" has 'bad.sh:6: native:'
check "curl via an 8123 variable reported" has 'bad.sh:7: http:'
check "continued curl line reported" has 'bad.sh:8: http:'
check "curl -C (continue-at) is not a credential" has 'bad.sh:10: http:'
check "inline content scanned" has 'inline.sh\|/usr/local/bin/inline.sh'
n="$(printf '%s\n' "$out" | grep -c ': native:\|: http:' || true)"
check "exactly 5 findings (got $n): comments, messages, command -v, non-CH curl, credentialed calls and non-bin dests are clean" [ "$n" -eq 5 ]
check "credential-file calls not reported" lacks 'good.sh'
check "self-accounting line counts 3 scripts" has 'in 3 installed script(s)'

mkdir -p "$TMP/empty/configs/ansible"
set +e
(cd "$TMP/empty" && python3 "$LINT" >/dev/null 2>&1)
rc=$?
set -e
check "scanning nothing fails closed (rc=$rc)" [ "$rc" -eq 2 ]

set +e
real="$(cd "$REPO_ROOT" && python3 "$LINT")"
rc=$?
set -e
check "real tree scans installed scripts" real_scanned

exit "$fail"
