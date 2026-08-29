#!/usr/bin/env python3
"""Secret-rendering ansible template tasks must not print their diff.

audit-2026-08-28 backup-restore-7: the task that renders
pgbackrest.conf.j2 inlines repo1/repo2-cipher-pass and the repo2 S3 key
pair (the only key that decrypts the survival backup copy), yet carried
neither `diff: false` nor `no_log: true`. The documented review path is
`ansible-playbook --check --diff` (README, the operator register, and the
weekly ansible-drift workflow which `tee`s the output into a GitHub
Actions log) — and ansible's template diff prints the FULL before/after
content. Actions masks only registered secrets, not values decrypted from
the vault file, so the passphrase lands in scrollback and the CI log.

Rule: every `ansible.builtin.template` / `template` task under
configs/ansible/roles/*/tasks whose (statically resolvable) template
renders a secret-shaped variable must set `diff: false` or
`no_log: true`. `diff: false` is preferred — it keeps the changed/ok
verdict visible and suppresses only the hunk body.

Pre-existing violators are grandfathered below with a reason, in the
same spirit as configs/ansible/.ansible-lint's skip_list: burn the list
down, never grow it. A grandfathered entry that no longer violates is
reported so the list stays honest.
"""
import glob
import os
import re
import sys

try:
    import yaml
except ImportError:
    # Fail-closed like lint-rule-structure.py: a silent exit-0 here would be
    # exactly the vacuous pass this lint exists to prevent.
    print("lint-ansible-secret-diff: FAIL — PyYAML not available; cannot lint "
          "ansible task files (install pyyaml; refusing to pass vacuously)",
          file=sys.stderr)
    sys.exit(2)

ROLES_DIR = "configs/ansible/roles"

# Suffix-anchored on purpose: `stellar_passphrase` (the public network
# passphrase) and `ssh_permit_password_auth` (a bool) must NOT match, while
# `*_cipher_pass`, `*_s3_key`, `*_s3_key_secret`, `*_token`, `*_password`,
# `vault_*_api_key` must.
SECRET_VAR = re.compile(
    r"\b[A-Za-z0-9_]*(_password|_pass|_secret|_token|_api_key|_s3_key)\b"
)
JINJA_EXPR = re.compile(r"\{\{(.*?)\}\}", re.S)

# file::task-name → reason. Each is a secret-rendering template task that
# predates this lint. They are the same class as backup-restore-7 and are
# tracked for burn-down; new entries are not accepted.
GRANDFATHERED = {
    "archival-node/tasks/09-minio.yml::Template MinIO environment file":
        "minio_root_password — pre-lint; needs diff: false",
    "haproxy/tasks/04-keepalived-configure.yml::render /etc/keepalived/keepalived.conf":
        "keepalived_vrrp_password — pre-lint; needs diff: false",
    "patroni/tasks/03-etcd-configure.yml::render /etc/default/etcd":
        "etcd_cluster_token — pre-lint; needs diff: false",
    "patroni/tasks/06-patroni-configure.yml::render /etc/patroni/patroni.yml (first-run / forced)":
        "patroni_*_password — pre-lint; needs diff: false",
    "redis-sentinel/tasks/03-redis-configure.yml::render /etc/redis/redis.conf":
        "redis_password — pre-lint; needs diff: false",
    "redis-sentinel/tasks/03-redis-configure.yml::render /etc/redis/users.acl (lockdown)":
        "redis_password — pre-lint; needs diff: false",
    "redis-sentinel/tasks/04-sentinel-configure.yml::render /etc/redis/sentinel.conf":
        "redis_password — pre-lint; needs diff: false",
}

bad = 0
checked = 0
seen_grandfathered = set()


def err(msg):
    global bad
    bad += 1
    print(f"lint-ansible-secret-diff: FAIL — {msg}", file=sys.stderr)


def template_renders_secret(path):
    with open(path, encoding="utf-8") as fh:
        text = fh.read()
    return [m.group(0).strip() for m in JINJA_EXPR.finditer(text)
            if SECRET_VAR.search(m.group(1))]


def walk(tasks, task_file, role):
    global checked
    for task in tasks:
        if not isinstance(task, dict):
            continue
        for key in ("block", "rescue", "always"):
            if isinstance(task.get(key), list):
                walk(task[key], task_file, role)
        mod = task.get("ansible.builtin.template", task.get("template"))
        if mod is None:
            continue
        if isinstance(mod, dict):
            src = mod.get("src")
        else:  # k=v shorthand
            m = re.search(r"\bsrc=(\S+)", str(mod))
            src = m.group(1) if m else None
        if not src or "{{" in src:
            # loop-driven src (e.g. systemd/{{ item }}.j2) — not statically
            # resolvable; those templates render unit files, not secrets.
            continue
        tpl = os.path.join(ROLES_DIR, role, "templates", src)
        if not os.path.isfile(tpl):
            continue
        hits = template_renders_secret(tpl)
        if not hits:
            continue
        checked += 1
        key = f"{role}/{os.path.relpath(task_file, os.path.join(ROLES_DIR, role))}::{task.get('name')}"
        suppressed = task.get("diff") is False or task.get("no_log") is True
        if key in GRANDFATHERED:
            seen_grandfathered.add(key)
            if suppressed:
                err(f"{key} is grandfathered but now complies — remove it from "
                    f"GRANDFATHERED so the list stays honest")
            continue
        if not suppressed:
            err(f"{key} renders {tpl} which inlines secret-shaped vars "
                f"{hits} but sets neither `diff: false` nor `no_log: true` — "
                f"`--check --diff` would print the secrets into scrollback / "
                f"the CI log")


task_files = sorted(glob.glob(os.path.join(ROLES_DIR, "*", "tasks", "*.yml")))
for task_file in task_files:
    role = task_file.split(os.sep)[3]
    with open(task_file, encoding="utf-8") as fh:
        try:
            doc = yaml.safe_load(fh)
        except yaml.YAMLError as exc:
            err(f"{task_file}: YAML parse error: {exc}")
            continue
    if isinstance(doc, list):
        walk(doc, task_file, role)

for key in sorted(set(GRANDFATHERED) - seen_grandfathered):
    err(f"GRANDFATHERED entry {key!r} no longer matches any secret-rendering "
        f"template task — remove it")

if not task_files or checked == 0:
    err(f"scanned {len(task_files)} task files but found 0 secret-rendering "
        f"template tasks — the scan is broken, refusing to pass vacuously")

print(f"lint-ansible-secret-diff: {checked} secret-rendering template task(s) "
      f"checked across {len(task_files)} task file(s), "
      f"{len(seen_grandfathered)} grandfathered, {bad} violation(s)")
sys.exit(1 if bad else 0)
