#!/usr/bin/env bash
# lint-ansible-tasks-test.sh — fixtures for lint-ansible-tasks.sh.
#
# Load-bearing cases (2026-08-28 audit, deploy-ansible-shell-1 +
# deploy-ansible-secrets-9):
#   - a shell task whose body sets pipefail without `executable: /bin/bash`
#     is flagged (04-users.yml aborted every full role apply under dash);
#     the same task WITH the executable, and a `copy: content:` script that
#     carries its own shebang, are not;
#   - a vault secret in `argv:` / a folded `command: >-` body is flagged;
#     the same secret under `stdin:` / `environment:` is not (09-minio.yml
#     claimed env-based auth while passing the root password on argv);
#   - a shipped .sh giving `mc alias set` the secret positionally (across a
#     backslash continuation) is flagged; the stdin-piped form is not;
#   - baseline: a listed (rule, path) is reported but not counted; a listed
#     pair with no live violation is STALE and fails (shrink-only);
#   - the gate refuses to pass vacuously on an empty tree.
#
# The fixtures build a throwaway repo_root and run the REAL script against
# it (it derives repo_root from its own path).
#
# Run: bash scripts/ci/lint-ansible-tasks-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SRC="$PWD/scripts/ci/lint-ansible-tasks.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()   { pass=$((pass + 1)); echo "  ok   — $1"; }
bad()  { fail=$((fail + 1)); echo "  FAIL — $1"; }
# expect_hit <pattern> <desc>   — $out must contain the pattern
# expect_miss <pattern> <desc>  — $out must NOT contain the pattern
# expect_rc <n> <desc>          — $rc must equal n
expect_hit()  { if grep -q -- "$1" <<<"$out"; then ok "$2"; else bad "$2 (missing: $1)"; fi; }
expect_miss() { if grep -q -- "$1" <<<"$out"; then bad "$2 (unexpected: $1)"; else ok "$2"; fi; }
expect_rc()   { if [ "$rc" -eq "$1" ]; then ok "$2 (rc=$rc)"; else bad "$2 (expected rc $1, got $rc): $out"; fi; }

ROOT="$TMP/repo"
T="$ROOT/configs/ansible/roles/fx/tasks"
F="$ROOT/configs/ansible/roles/fx/files"
mkdir -p "$ROOT/scripts/ci" "$T" "$F"
cp "$SRC" "$ROOT/scripts/ci/lint-ansible-tasks.sh"

cat > "$T/pipefail.yml" <<'YML'
---
- name: bad — pipefail under /bin/sh
  ansible.builtin.shell:
    cmd: |
      set -euo pipefail
      find /x -type d | grep -q .
  changed_when: false

- name: good — pipefail under bash (cmd form)
  ansible.builtin.shell:
    cmd: |
      set -euo pipefail
      find /x -type d | grep -q .
    executable: /bin/bash
  changed_when: false

- name: good — pipefail under bash (args form)
  ansible.builtin.shell: |
    set -o pipefail
    curl -fsSL https://example | gpg --dearmor
  args:
    creates: /x
    executable: /bin/bash

- name: not a shell task — shipped script with its own shebang
  ansible.builtin.copy:
    dest: /usr/local/bin/x
    mode: "0755"
    content: |
      #!/bin/bash
      set -euo pipefail
      echo hi
YML

cat > "$T/secrets.yml" <<'YML'
---
- name: bad — secret on argv
  ansible.builtin.command:
    argv:
      - /usr/local/bin/mc
      - alias
      - set
      - local
      - "{{ minio_root_user }}"
      - "{{ minio_root_password }}"
  no_log: true

- name: good — secret on stdin, user still on argv
  ansible.builtin.command:
    argv:
      - /usr/local/bin/mc
      - admin
      - user
      - add
      - local
      - "{{ galexie_s3_access_key }}"
    stdin: "{{ galexie_s3_secret_key }}"
  no_log: true

- name: good — secret in environment
  ansible.builtin.command:
    argv:
      - /usr/local/bin/stellarindex-migrate
      - up
  environment:
    STELLARINDEX_POSTGRES_DSN: "postgres://u:{{ postgres_pass_stellarindex }}@127.0.0.1/db"
  no_log: true

- name: bad — secret in folded command body (short module name)
  command: >-
    redis-cli -h {{ ansible_host }}
    -a "{{ redis_password }}" --no-auth-warning PING
  no_log: true
YML

cat > "$F/bad.sh" <<'SH'
#!/bin/bash
# mc alias set live "$URL" "$KEY" "$AWS_SECRET_ACCESS_KEY"   <- comment, ignored
if /usr/local/bin/mc alias set live "$AWS_ENDPOINT_URL" \
     "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" >/dev/null 2>&1; then
  echo ok
fi
SH
cat > "$F/good.sh" <<'SH'
#!/bin/bash
if printf '%s\n%s\n' "$AWS_ACCESS_KEY_ID" "$AWS_SECRET_ACCESS_KEY" \
     | /usr/local/bin/mc alias set live "$AWS_ENDPOINT_URL" >/dev/null 2>&1; then
  echo ok
fi
SH

run() { (cd "$ROOT" && BASELINE="${1:-/nonexistent}" bash scripts/ci/lint-ansible-tasks.sh 2>&1); }

echo "case: no baseline"
out="$(run)"; rc=$?
expect_rc 4 "exit code = 4 findings"
expect_hit 'pipefail.yml:2: pipefail-needs-bash' "bad pipefail task flagged"
expect_miss 'pipefail.yml:9:' "cmd-form bash task not flagged"
expect_miss 'pipefail.yml:17:' "args-form bash task not flagged"
expect_miss 'pipefail.yml:25:' "copy:content script not flagged"
expect_hit 'secrets.yml:10: secret-on-argv' "secret on argv flagged"
expect_miss 'secrets.yml:21:' "stdin secret not flagged"
expect_miss 'secrets.yml:31:' "environment secret not flagged"
expect_hit 'secrets.yml:37: secret-on-argv' "folded command body secret flagged"
expect_hit 'bad.sh:3: secret-on-argv' "mc positional secret (continuation) flagged"
expect_miss 'bad.sh:2:' "commented mc line not flagged"
expect_miss 'good.sh' "stdin-piped mc not flagged"
expect_hit 'scanned 2 task files + 2 shell scripts' "self-accounting line"

echo "case: baseline suppresses + stale entry fails"
printf 'secret-on-argv\tconfigs/ansible/roles/fx/tasks/secrets.yml\npipefail-needs-bash\tconfigs/ansible/roles/fx/tasks/nolonger.yml\n' > "$TMP/baseline"
out="$(run "$TMP/baseline")"; rc=$?
# 4 live findings − 2 baselined (both secrets.yml) + 1 stale = 3
expect_rc 3 "exit code = 3"
expect_hit 'baselined (shrink-only' "baselined finding reported"
expect_hit 'STALE baseline entry.*nolonger.yml' "stale entry flagged"

echo "case: vacuity"
mkdir -p "$TMP/empty/scripts/ci" "$TMP/empty/configs/ansible"
cp "$SRC" "$TMP/empty/scripts/ci/"
out="$( (cd "$TMP/empty" && bash scripts/ci/lint-ansible-tasks.sh 2>&1) )"; rc=$?
expect_rc 1 "empty tree fails"
expect_hit 'must not pass vacuously' "empty tree fails loudly"

echo "lint-ansible-tasks-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
