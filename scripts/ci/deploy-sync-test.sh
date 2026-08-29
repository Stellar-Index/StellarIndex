#!/usr/bin/env bash
# deploy-sync-test.sh — the migrations sync in deploy-binary.yml must be
# ONE transfer with synchronize's delete semantics (2026-08-29 incident:
# r1 deploy of v0.49.0, run 33244745680).
#
# PR #268 replaced `ansible.posix.synchronize` (one rsync, `delete: true`)
# with `ansible.builtin.copy` pointed at a DIRECTORY src. copy is
# connection-agnostic (the ProxyJump goal was right) but O(files): one
# SFTP round-trip + remote checksum per file, no ControlPersist across
# module invocations on the GH runner. 291 already-identical files took
# > 16 minutes on r1 where the whole deploy used to take ~7 — and stale
# files on the host were silently kept. The fix (tasks/sync-migrations.yml)
# builds one deterministic tar.gz on the controller, ships it with
# `unarchive`, and prunes extras from a controller-computed manifest.
#
# Structural (python + yaml, no ansible — always runs):
#   a. exactly ONE transfer-class task touches the migrations dir, it is
#      `unarchive` of a single archive (no directory-src copy, no
#      synchronize, no per-file loop);
#   a'. the sync dest and the `stellarindex-migrate -migrations` path in
#      the Apply step are the same directory.
#
# Behavioural (a real `ansible-playbook` run of the task file against
# temp dirs — controller = this machine, target = localhost with
# `-c local` by default):
#   b. an extra file in dest (and one nested in a stale subdir) is removed;
#   c. an identical re-run reports changed=0;
#   d. files OUTSIDE dest (sibling file, sibling dir) are never touched;
#   e. a removed/changed source file is removed/updated on the next run;
#   f. an EMPTY source dir fails the play and leaves dest intact (fail
#      closed — the prune would otherwise wipe the dir).
#
# unarchive needs GNU tar on the TARGET (`tar --diff`), which macOS lacks.
# On such a machine point the target at a container that has it:
#   docker run -d --name si-sync-test --entrypoint sleep jrei/systemd-ubuntu:24.04 infinity
#   DEPLOY_SYNC_CONNECTION=community.docker.docker DEPLOY_SYNC_HOST=si-sync-test \
#     bash scripts/ci/deploy-sync-test.sh
# CI (ubuntu, ansible-check job) runs it with the local default.
#
# Overrides for the red-proof against a pre-fix copy: PLAYBOOK, TASKFILE.
# Run: bash scripts/ci/deploy-sync-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1

PLAYBOOK="${PLAYBOOK:-configs/ansible/playbooks/deploy-binary.yml}"
TASKFILE="${TASKFILE:-configs/ansible/tasks/sync-migrations.yml}"
CONN="${DEPLOY_SYNC_CONNECTION:-local}"
HOST="${DEPLOY_SYNC_HOST:-localhost}"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

for f in "$PLAYBOOK" "$TASKFILE"; do
  if [ ! -f "$f" ]; then
    echo "deploy-sync-test: FAIL — file not found: $f" >&2
    exit 1
  fi
done

# ── structural ─────────────────────────────────────────────────────────
echo "deploy-sync-test: structural ($PLAYBOOK)"
SOUT="$(mktemp)"
# Not `out="$(python3 … <<'PY')"`: bash 3.2 (macOS) mis-parses quotes inside
# a heredoc nested in a command substitution.
python3 - "$PLAYBOOK" > "$SOUT" 2>&1 <<'PY'
import os, sys, yaml

playbook = sys.argv[1]
DEST = "/usr/local/share/stellarindex/migrations"
TRANSFER = {"copy", "synchronize", "unarchive", "template", "script", "get_url"}
INCLUDE = ("include_tasks", "ansible.builtin.include_tasks",
           "import_tasks", "ansible.builtin.import_tasks")
LOOPS = ("loop", "with_items", "with_fileglob", "with_filetree", "with_list")
fails = []

def walk(tasks, path, seen, found):
    for t in tasks or []:
        if not isinstance(t, dict):
            continue
        if "block" in t:
            for k in ("block", "rescue", "always"):
                walk(t.get(k), path, seen, found)
            continue
        for k, v in t.items():
            if k in INCLUDE:
                inc = v["file"] if isinstance(v, dict) else v
                inc = os.path.normpath(os.path.join(os.path.dirname(path), str(inc)))
                if inc not in seen and os.path.isfile(inc):
                    seen.add(inc)
                    with open(inc) as fh:
                        walk(yaml.safe_load(fh), inc, seen, found)
                continue
            short = k.split(".")[-1]
            if short in TRANSFER and isinstance(v, dict):
                blob = yaml.safe_dump(v)
                if DEST in blob or "migrations_dest" in blob or "/migrations" in blob:
                    found.append((path, t.get("name", "<unnamed>"), short, v,
                                  any(l in t for l in LOOPS)))

with open(playbook) as fh:
    plays = yaml.safe_load(fh)
seen, found = {playbook}, []
for play in plays:
    for key in ("pre_tasks", "tasks", "post_tasks", "handlers"):
        walk(play.get(key), playbook, seen, found)

if len(found) != 1:
    fails.append("expected exactly ONE transfer task touching the migrations dir, found %d: %s"
                 % (len(found), [(p, n, m) for p, n, m, _, _ in found]))
for path, name, mod, args, looped in found:
    if looped:
        fails.append("%s: task %r loops a transfer module — O(files)" % (path, name))
    src = str(args.get("src", ""))
    if mod in ("copy", "synchronize", "template", "script") or src.endswith("/") or src.endswith("/migrations"):
        fails.append("%s: task %r uses %s with src=%r — a directory transfer is one "
                     "round-trip PER FILE (291 files, > 16 min on r1); ship ONE archive "
                     "via unarchive" % (path, name, mod, src))
    if mod == "unarchive":
        if not src.endswith((".tar.gz", ".tgz", ".tar")):
            fails.append("%s: task %r unarchive src %r is not a single tar archive" % (path, name, src))
        if args.get("remote_src") not in (None, False, "false", "no"):
            fails.append("%s: task %r unarchive must ship from the controller (remote_src: false)" % (path, name))

# a'. sync dest ↔ migrate path parity
migrate_ok = False
for play in plays:
    for t in play.get("pre_tasks") or []:
        if isinstance(t, dict) and "Apply outstanding migrations" in str(t.get("name", "")):
            sh = t.get("ansible.builtin.shell") or t.get("shell") or {}
            cmd = sh.get("cmd", "") if isinstance(sh, dict) else str(sh)
            migrate_ok = ("-migrations %s up" % DEST) in cmd
if not migrate_ok:
    fails.append("Apply step does not run stellarindex-migrate against %s" % DEST)
dest_default = False
for path in seen:
    with open(path) as fh:
        if "migrations_dest | default('%s')" % DEST in fh.read():
            dest_default = True
if found and not dest_default:
    fails.append("sync dest default is not %s (migrate would read a different dir)" % DEST)

print("checked %d task file(s); %d transfer task(s) touch the migrations dir" % (len(seen), len(found)))
for f in fails:
    print("FAIL — " + f)
sys.exit(1 if fails else 0)
PY
src_rc=$?
if [ "$src_rc" -eq 0 ]; then ok "structural: $(cat "$SOUT")"; else bad "structural: $(cat "$SOUT")"; fi
rm -f "$SOUT"

# ── behavioural ────────────────────────────────────────────────────────
if ! command -v ansible-playbook >/dev/null; then
  echo "deploy-sync-test: FAIL — ansible-playbook not on PATH (this test must not pass vacuously)" >&2
  exit 1
fi

# t_exec <cmd> — run a bash command on the TARGET (local or the container).
t_exec() {
  if [ "$CONN" = "local" ]; then
    bash -c "$1"
  else
    docker exec "$HOST" bash -c "$1"
  fi
}
if ! t_exec 'tar --version' 2>/dev/null | grep -q 'GNU tar'; then
  echo "deploy-sync-test: FAIL — target ($CONN/$HOST) has no GNU tar; unarchive needs it. See the header for the container recipe." >&2
  exit 1
fi

TASKFILE_ABS="$(cd "$(dirname "$TASKFILE")" && pwd)/$(basename "$TASKFILE")"
CT="$(mktemp -d)"                       # controller side: dist + fixture
TD="$(t_exec 'mktemp -d')"              # target side: dest + outside
T_OWNER="$(t_exec 'id -un')"; T_GROUP="$(t_exec 'id -gn')"
cleanup() { rm -rf "$CT"; t_exec "rm -rf '$TD'"; }
trap cleanup EXIT

DEST="$TD/migrations"
mkdir -p "$CT/dist/migrations"
printf 'create table a;\n' > "$CT/dist/migrations/001_a.up.sql"
printf 'drop table a;\n'   > "$CT/dist/migrations/001_a.down.sql"
printf 'create table b;\n' > "$CT/dist/migrations/002_b.up.sql"
# target: one identical file, one stale file, one stale nested file, and
# two things OUTSIDE dest that must survive untouched.
t_exec "mkdir -p '$DEST/old' '$TD/outside' \
  && printf 'create table a;\n' > '$DEST/001_a.up.sql' \
  && printf 'stale\n' > '$DEST/000_stale.up.sql' \
  && printf 'stale\n' > '$DEST/old/999_nested.up.sql' \
  && printf 'keep\n' > '$TD/outside/keep.sql' \
  && printf 'keep\n' > '$TD/migrations.sibling.sql'"

cat > "$CT/fixture.yml" <<YML
- hosts: all
  gather_facts: false
  become: false
  vars:
    local_dist_dir: $CT/dist
    migrations_dest: $DEST
    migrations_owner: $T_OWNER
    migrations_group: $T_GROUP
  tasks:
    - ansible.builtin.import_tasks: $TASKFILE_ABS
YML

run_play() {  # sets $out, $rc, $changed
  out="$(cd "$CT" && ANSIBLE_LOCALHOST_WARNING=false ANSIBLE_INVENTORY_UNPARSED_WARNING=false \
         ansible-playbook -i "$HOST," -c "$CONN" fixture.yml 2>&1)"; rc=$?
  changed="$(grep -oE 'changed=[0-9]+' <<<"$out" | tail -1 | cut -d= -f2)"
  changed="${changed:-?}"
}
dest_files() { t_exec "cd '$DEST' && find . -mindepth 1 -print | sed 's#^\./##' | LC_ALL=C sort"; }
outside_state() { t_exec "cat '$TD/outside/keep.sql' '$TD/migrations.sibling.sql'; ls '$TD/outside'"; }
before_outside="$(outside_state)"

# run 1 — converge onto a dest with extras
run_play
if [ "$rc" -ne 0 ]; then bad "run 1: play failed (rc=$rc): $out"; else ok "run 1: play ok (changed=$changed)"; fi
want=$'001_a.down.sql\n001_a.up.sql\n002_b.up.sql'
got="$(dest_files)"
if [ "$got" = "$want" ]; then ok "b. extras removed (stale file + stale nested subdir), release set present"
else bad "b. dest after run 1 is:"$'\n'"$got"$'\n'"want:"$'\n'"$want"; fi
if [ "$(t_exec "cat '$DEST/002_b.up.sql'")" = "create table b;" ]; then ok "run 1: new file content correct"
else bad "run 1: 002_b.up.sql content wrong"; fi
if [ "$(outside_state)" = "$before_outside" ]; then ok "d. files outside dest untouched after run 1"
else bad "d. something outside dest changed after run 1"; fi

# run 2 — identical → idempotent
run_play
if [ "$rc" -eq 0 ] && [ "$changed" = "0" ]; then ok "c. identical re-run is idempotent (changed=0)"
else bad "c. identical re-run rc=$rc changed=$changed: $out"; fi

# run 3 — source shrinks + one file changes content
rm -f "$CT/dist/migrations/002_b.up.sql"
printf 'create table a2;\n' > "$CT/dist/migrations/001_a.up.sql"
run_play
want=$'001_a.down.sql\n001_a.up.sql'
got="$(dest_files)"
if [ "$rc" -eq 0 ] && [ "$got" = "$want" ] && [ "$(t_exec "cat '$DEST/001_a.up.sql'")" = "create table a2;" ] && [ "$changed" != "0" ]; then
  ok "e. removed source file pruned + changed content updated (changed=$changed)"
else bad "e. rc=$rc changed=$changed dest:"$'\n'"$got"; fi
if [ "$(outside_state)" = "$before_outside" ]; then ok "d. files outside dest untouched after run 3"
else bad "d. something outside dest changed after run 3"; fi

# run 4 — empty source must fail closed, dest intact
rm -f "$CT/dist/migrations/"*
run_play
got="$(dest_files)"
if [ "$rc" -ne 0 ] && [ "$got" = "$want" ]; then ok "f. empty source dir fails the play and leaves dest intact"
else bad "f. empty source: rc=$rc dest:"$'\n'"$got"; fi

echo "deploy-sync-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
