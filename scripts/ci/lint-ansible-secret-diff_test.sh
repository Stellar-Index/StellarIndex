#!/usr/bin/env bash
# lint-ansible-secret-diff_test.sh — a scan glob of only
# ROLES_DIR/*/tasks/*.yml would miss a secret-rendering `ansible.builtin.template`
# task placed in a role's handlers/ (or a playbook, or the shared
# configs/ansible/tasks/). This plants one in a
# fake role's handlers/ file and asserts the lint catches it.
#
# Run: bash scripts/ci/lint-ansible-secret-diff_test.sh
set -euo pipefail
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
LINT="$REPO_ROOT/scripts/ci/lint-ansible-secret-diff.py"

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

mkdir -p "$TMPDIR/configs/ansible/roles/fakerole/handlers"
mkdir -p "$TMPDIR/configs/ansible/roles/fakerole/templates"

cat > "$TMPDIR/configs/ansible/roles/fakerole/handlers/main.yml" <<'EOF'
---
- name: Render leaked secret config
  ansible.builtin.template:
    src: leak.conf.j2
    dest: /etc/leak.conf
EOF

cat > "$TMPDIR/configs/ansible/roles/fakerole/templates/leak.conf.j2" <<'EOF'
password = {{ fake_service_password }}
EOF

set +e
OUTPUT="$(cd "$TMPDIR" && python3 "$LINT" 2>&1)"
STATUS=$?
set -e

echo "$OUTPUT"

if [[ $STATUS -eq 0 ]]; then
  echo "FAIL: lint passed (exit 0) on a secret-rendering template task hidden in handlers/main.yml — the handlers/ scan gap (T492) reproduced" >&2
  exit 1
fi

if [[ "$OUTPUT" != *"fakerole/handlers/main.yml::Render leaked secret config"* ]]; then
  echo "FAIL: lint failed for the wrong reason — did not name the handlers/main.yml violation" >&2
  exit 1
fi

echo "PASS: lint-ansible-secret-diff catches a secret-rendering template task in a role's handlers/ file"

# A playbook's top-level items are plays; their template tasks sit under
# pre_tasks / tasks / post_tasks / handlers.
mkdir -p "$TMPDIR/configs/ansible/playbooks/templates"
cat > "$TMPDIR/configs/ansible/playbooks/templates/pb.conf.j2" <<'EOF'
password = {{ fake_play_password }}
EOF
cat > "$TMPDIR/configs/ansible/playbooks/site.yml" <<'EOF'
---
- hosts: all
  pre_tasks:
    - name: Render pre secret
      ansible.builtin.template:
        src: pb.conf.j2
        dest: /etc/pre.conf
  tasks:
    - name: Render play secret
      ansible.builtin.template:
        src: pb.conf.j2
        dest: /etc/play.conf
  handlers:
    - name: Render handler secret
      ansible.builtin.template:
        src: pb.conf.j2
        dest: /etc/handler.conf
EOF

set +e
OUTPUT="$(cd "$TMPDIR" && python3 "$LINT" 2>&1)"
set -e

for name in "Render pre secret" "Render play secret" "Render handler secret"; do
  if [[ "$OUTPUT" != *"playbooks/site.yml::$name"* ]]; then
    echo "FAIL: lint did not flag '$name' in a playbook play" >&2
    exit 1
  fi
done

echo "PASS: lint-ansible-secret-diff catches secret-rendering template tasks inside a playbook's plays"

# A secret name that continues past its keyword (`_secret_key`, `_seal_key`)
# is not covered by a suffix-only match on `_secret`.
cat > "$TMPDIR/configs/ansible/roles/fakerole/templates/leak.conf.j2" <<'EOF'
key = {{ fake_service_secret_key }}
EOF
set +e
OUTPUT="$(cd "$TMPDIR" && python3 "$LINT" 2>&1)"
STATUS=$?
set -e
if [[ $STATUS -eq 0 || "$OUTPUT" != *"fake_service_secret_key"* ]]; then
  echo "FAIL: lint did not flag a template inlining *_secret_key" >&2
  echo "$OUTPUT" >&2
  exit 1
fi

echo "PASS: lint-ansible-secret-diff catches a template inlining *_secret_key"

# Pager keys and webhook or healthcheck URLs are credentials too.
for var in fake_pagerduty_key fake_discord_webhook_url_pages fake_healthchecks_alert_delivery_url; do
  printf 'x = {{ %s }}\n' "$var" > "$TMPDIR/configs/ansible/roles/fakerole/templates/leak.conf.j2"
  set +e
  OUTPUT="$(cd "$TMPDIR" && python3 "$LINT" 2>&1)"
  STATUS=$?
  set -e
  if [[ $STATUS -eq 0 || "$OUTPUT" != *"$var"* ]]; then
    echo "FAIL: lint did not flag a template inlining $var" >&2
    echo "$OUTPUT" >&2
    exit 1
  fi
done

echo "PASS: lint-ansible-secret-diff catches pager keys and webhook or healthcheck URLs"

# A keyword followed by a qualifier is still the secret; the env-var NAME is not.
for var in fake_pass_core fake_secret_access_key vault_fake_previous_key; do
  printf 'x = {{ %s }}\n' "$var" > "$TMPDIR/configs/ansible/roles/fakerole/templates/leak.conf.j2"
  set +e
  OUTPUT="$(cd "$TMPDIR" && python3 "$LINT" 2>&1)"
  STATUS=$?
  set -e
  if [[ $STATUS -eq 0 || "$OUTPUT" != *"$var"* ]]; then
    echo "FAIL: lint did not flag a template inlining $var" >&2
    echo "$OUTPUT" >&2
    exit 1
  fi
done
printf 'x = {{ fake_secret_key_env }}\n' > "$TMPDIR/configs/ansible/roles/fakerole/templates/leak.conf.j2"
set +e
OUTPUT="$(cd "$TMPDIR" && python3 "$LINT" 2>&1)"
set -e
if [[ "$OUTPUT" == *"fake_secret_key_env"* ]]; then
  echo "FAIL: lint flagged the env-var name fake_secret_key_env as a secret" >&2
  exit 1
fi
echo "PASS: lint-ansible-secret-diff catches qualified secret names, not env-var names"

# copy/lineinfile/blockinfile text is templated too; a {# #} comment is not.
mkdir -p "$TMPDIR/configs/ansible/roles/fakerole/tasks"
cat > "$TMPDIR/configs/ansible/roles/fakerole/tasks/main.yml" <<'EOF'
---
- name: Inline unit with a secret
  copy:
    dest: /etc/x.service
    content: |
      # Environment=PW={{ fake_redis_password }}
- name: Inline line with a secret
  ansible.builtin.lineinfile:
    path: /etc/x
    line: "pw={{ fake_line_password }}"
- name: Inline comment only
  copy:
    dest: /etc/y
    content: "{# {{ fake_hidden_password }} #}"
EOF
set +e
OUTPUT="$(cd "$TMPDIR" && python3 "$LINT" 2>&1)"
STATUS=$?
set -e
for name in "Inline unit with a secret" "Inline line with a secret"; do
  if [[ $STATUS -eq 0 || "$OUTPUT" != *"fakerole/tasks/main.yml::$name"* ]]; then
    echo "FAIL: lint did not flag inline-text task '$name'" >&2
    echo "$OUTPUT" >&2
    exit 1
  fi
done
if [[ "$OUTPUT" == *"Inline comment only"* ]]; then
  echo "FAIL: lint flagged a secret inside a Jinja {# #} comment" >&2
  exit 1
fi
echo "PASS: lint-ansible-secret-diff checks copy/lineinfile inline text"
