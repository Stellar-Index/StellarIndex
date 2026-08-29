#!/usr/bin/env bash
# lint-deploy-playbook.sh — the deploy path must work over the test-net
# ProxyJump and on hosts without pgBackRest (deploy-ansible-deploy-2).
#
# deploy.yml reaches the NAT-only testnet/futurenet VMs ONLY via
# `--ssh-common-args "-o ProxyJump=…"`. Modules whose transport runs on
# the CONTROLLER (ansible.posix.synchronize → rsync dials the host itself)
# never see that argument, so a deploy-binary.yml that used synchronize
# for the migrations dir timed out before it reached the schema — and the
# backup-freshness gate, which fails closed on a missing pgbackrest, could
# only ever exit 1 on the VMs (18-pgbackrest-backup.yml installs nothing
# there). The sole escape was migrations_skip=true: the workflow could
# never apply schema off r1. Nothing in CI syntax-checks deploy-binary.yml
# and `gh run list` showed zero test-net deploys, so it stayed latent.
#
# Deterministic, no network, no ansible:
#   1. every module in deploy-binary.yml + the task files it includes is
#      connection-agnostic (no synchronize / rsync). The transfer must ALSO
#      be one archive, not a directory-src copy (O(files): > 16 min for
#      291 files on r1, run 33244745680) — deploy-sync-test.sh pins that;
#   2. the pgBackRest gate is conditioned on `pgbackrest_backup_enabled`
#      (default true — fail closed) and deploy.yml passes that var as -e,
#      set false for the test-net regions and left true for r1.
#
# Run: bash scripts/ci/lint-deploy-playbook.sh
set -euo pipefail

cd "$(dirname "$0")/../.."

PLAYBOOK="${PLAYBOOK:-configs/ansible/playbooks/deploy-binary.yml}"
WORKFLOW="${WORKFLOW:-.github/workflows/deploy.yml}"

for f in "$PLAYBOOK" "$WORKFLOW"; do
  if [ ! -f "$f" ]; then
    echo "lint-deploy-playbook: FAIL — file not found: $f" >&2
    exit 1
  fi
done

python3 - "$PLAYBOOK" "$WORKFLOW" <<'PY'
import os, re, sys, yaml

playbook, workflow = sys.argv[1], sys.argv[2]
fails = []

# ─── 1. controller-side transport modules ─────────────────────────────
# rsync-backed modules run on the controller and cannot inherit the
# ProxyJump that deploy.yml passes through --ssh-common-args.
JUMP_UNSAFE = {"ansible.posix.synchronize", "synchronize"}
TASK_KEYS = ("pre_tasks", "tasks", "post_tasks", "handlers")
INCLUDE_KEYS = ("include_tasks", "ansible.builtin.include_tasks",
                "import_tasks", "ansible.builtin.import_tasks")

def walk(tasks, path, seen, modules):
    for t in tasks or []:
        if not isinstance(t, dict):
            continue
        if "block" in t:
            for k in ("block", "rescue", "always"):
                walk(t.get(k), path, seen, modules)
            continue
        for k, v in t.items():
            if k in INCLUDE_KEYS:
                inc = v["file"] if isinstance(v, dict) else v
                inc = os.path.normpath(os.path.join(os.path.dirname(path), str(inc)))
                if inc not in seen and os.path.isfile(inc):
                    seen.add(inc)
                    with open(inc) as fh:
                        walk(yaml.safe_load(fh), inc, seen, modules)
            if "." in k or k in JUMP_UNSAFE:
                modules.append((path, t.get("name", "<unnamed>"), k))

with open(playbook) as fh:
    plays = yaml.safe_load(fh)
seen = {playbook}
modules = []
for play in plays:
    for key in TASK_KEYS:
        walk(play.get(key), playbook, seen, modules)

for path, name, mod in modules:
    if mod in JUMP_UNSAFE:
        fails.append("%s: task %r uses %s — rsync runs on the controller and "
                     "cannot follow deploy.yml's --ssh-common-args ProxyJump "
                     "(test-net VMs unreachable); ship ONE archive with "
                     "ansible.builtin.unarchive (a directory-src copy is "
                     "O(files) — deploy-sync-test.sh)"
                     % (path, name, mod))

# ─── 2. pgBackRest gate is host-shape aware, and the workflow drives it ──
def find_task(plays, needle):
    for play in plays:
        for key in TASK_KEYS:
            for t in play.get(key) or []:
                if isinstance(t, dict) and needle in str(t.get("name", "")):
                    return t
    return None

gate = find_task(plays, "pgBackRest backup exists before migrating")
if gate is None:
    fails.append("%s: backup-freshness gate task not found" % playbook)
else:
    when = gate.get("when")
    when = when if isinstance(when, list) else [when]
    if not any("pgbackrest_backup_enabled" in str(w) for w in when):
        fails.append("%s: the backup-freshness gate is not conditioned on "
                     "pgbackrest_backup_enabled — on the test-net VMs (no "
                     "pgbackrest) it can only fail, so the workflow can never "
                     "apply schema there" % playbook)

with open(workflow) as fh:
    wf = fh.read()
if not re.search(r'-e "pgbackrest_backup_enabled=\$\{', wf):
    fails.append("%s: the deploy step does not pass "
                 "-e \"pgbackrest_backup_enabled=…\" — the ad-hoc `-i host,` "
                 "inventory loads no inventory vars, so the playbook cannot "
                 "learn the host shape any other way" % workflow)
for region in ("testnet", "futurenet"):
    m = re.search(r'\n\s*%s\)\n(.*?)\n\s*;;' % region, wf, re.S)
    if not m or 'backup_gate="false"' not in m.group(1):
        fails.append("%s: region %s does not set backup_gate=\"false\" — "
                     "its VM has no pgbackrest stanza" % (workflow, region))
m = re.search(r'\n\s*r1\)\n(.*?)\n\s*;;', wf, re.S)
if m and 'backup_gate="false"' in m.group(1):
    fails.append("%s: region r1 sets backup_gate=\"false\" — r1 HAS a stanza; "
                 "never weaken the gate there" % workflow)

n_files = len(seen)
print("lint-deploy-playbook: checked %d modules across %d task files + %s"
      % (len(modules), n_files, workflow))
if fails:
    for f in fails:
        print("lint-deploy-playbook: FAIL — " + f)
    sys.exit(1)
print("lint-deploy-playbook: OK")
PY
