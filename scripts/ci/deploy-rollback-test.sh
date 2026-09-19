#!/usr/bin/env bash
# deploy-rollback-test.sh — the deploy's rollback path must never remove a
# binary it cannot prove it can put back (K078, audit-2026-09-02).
#
# deploy-one-binary.yml swaps each binary in binaries_csv in turn. When a
# LATER binary fails its probe the rescue rolls the EARLIER ones back too
# (CID-14), so the host lands on one consistent version set. That rollback
# used to re-derive its restore state at rollback time: it `mv`'d every
# recorded binary aside to .rolledback-<version> and then `mv`'d a
# .prev-<tag> back with `ignore_errors: true`. A binary this run installed
# for the FIRST time has no .prev-<tag> — the restore silently failed, the
# install path was left EMPTY, and the play still reported that binary as
# "rolled back". A rollback runs when something is already wrong; deleting
# a healthy binary there turns a bad deploy into an outage (a new binary in
# binaries_csv, or any host rebuilt from bare metal, reaches it).
#
# Structural (python + yaml, no ansible — always runs):
#   a. the forward path records a per-binary RESTORE RECORD (live_path +
#      prev_path), not a bare tag: what to undo comes from what the deploy
#      observed, never from re-deriving it during the rescue;
#   b. no task in the rescue moves a recorded binary's live path aside
#      unconditionally — every destructive mv over the recorded set is
#      gated on a `when:`.
#
# Behavioural (a real `ansible-playbook` run of the task file against a
# throwaway container, so /var/lib/stellarindex/deployed-versions is the
# REAL production path and the userland is GNU like r1's):
#   c. partial deploy — a binary this run installed for the first time is
#      NOT removed by the whole-deploy rollback, and its sidecar keeps the
#      version actually on disk;
#   d. a binary with a prior install IS rolled back, byte-for-byte, and the
#      superseded build is parked at .rolledback-<version>;
#   e. an untracked prior install (binary present, no sidecar) is restored
#      and its sidecar goes back to ABSENT, not to the synthetic
#      "untracked-<timestamp>" tag;
#   f. the failing binary itself is restored and the bad build parked at
#      .failed-<version>;
#   g. a binary on the host that this deploy never names is untouched —
#      no move, no sidecar rewrite, no stray artefacts;
#   h. the play fails and its message NAMES what could not be rolled back
#      instead of claiming a clean rollback;
#   i. repeated rollback is convergent, not cumulative: a second identical
#      failing run loses no binary and changes no restored content;
#   k. a deploy that fails BEFORE the new build reaches the live path
#      leaves the build already installed there untouched — only what this
#      run actually installed is ever moved aside.
#
# The target is a container because the task file writes the real
# /var/lib/stellarindex/deployed-versions path and expects a root-ish,
# GNU userland; `python:3.12-slim` carries the python ansible needs.
# Every fixture binary is listed in cli_binaries so the systemd tasks
# short-circuit — this pins the filesystem half of the rescue, which is
# the destructive half.
#
# Overrides for the red-proof against a pre-fix copy: TASKFILE.
# Run: bash scripts/ci/deploy-rollback-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1

TASKFILE="${TASKFILE:-configs/ansible/tasks/deploy-one-binary.yml}"
IMAGE="${DEPLOY_ROLLBACK_IMAGE:-python:3.12-slim}"
VERSION="v9.9.9"
SIDECARS=/var/lib/stellarindex/deployed-versions

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

if [ ! -f "$TASKFILE" ]; then
  echo "deploy-rollback-test: FAIL — file not found: $TASKFILE" >&2
  exit 1
fi

# ── structural ─────────────────────────────────────────────────────────
echo "deploy-rollback-test: structural ($TASKFILE)"
SOUT="$(mktemp)"
# Not `out="$(python3 … <<'PY')"`: bash 3.2 (macOS) mis-parses quotes inside
# a heredoc nested in a command substitution.
python3 - "$TASKFILE" > "$SOUT" 2>&1 <<'PY'
import sys, yaml

path = sys.argv[1]
with open(path) as fh:
    tasks = yaml.safe_load(fh)

fails = []
rescue = []
forward = []
for t in tasks or []:
    if isinstance(t, dict) and "block" in t:
        forward = t.get("block") or []
        rescue = t.get("rescue") or []
if not rescue:
    fails.append("no rescue block found — the rollback path is what this test pins")

# a. the forward path records a restore RECORD, not a re-derivable tag.
#    Some task before the swap must build a mapping naming the binary's
#    live path and the backup it can be put back from, and THAT is what
#    the per-binary swap record must carry.
record_vars = []
for t in tasks or []:
    for holder in ([t] if not isinstance(t.get("block"), list) else [t] + (t.get("block") or [])):
        sf = holder.get("ansible.builtin.set_fact") or {}
        for var, val in sf.items():
            if isinstance(val, dict) and {"live_path", "prev_path"} <= set(val):
                record_vars.append(var)
if not record_vars:
    fails.append("no task builds a restore record (a mapping with live_path + prev_path) "
                 "before the swap — the rescue can only re-derive what to restore, which "
                 "is exactly the defect (K078)")

rec = [t for t in forward
       if "swapped_binaries" in yaml.safe_dump(t.get("ansible.builtin.set_fact", {}) or {})]
if len(rec) != 1:
    fails.append("expected exactly ONE task recording swapped_binaries, found %d" % len(rec))
elif record_vars:
    blob = yaml.safe_dump(rec[0])
    if not any(v in blob for v in record_vars):
        fails.append("the swapped_binaries record does not store the restore record (%s) — "
                     "the rescue would have to re-derive what to restore (K078): %s"
                     % ("/".join(record_vars), rec[0].get("name")))

# b. no destructive mv over the recorded set runs unconditionally.
for t in rescue:
    cmd = (t.get("ansible.builtin.command") or {})
    cmd = cmd.get("cmd", "") if isinstance(cmd, dict) else ""
    if not cmd.startswith("mv "):
        continue
    if "loop" not in t:
        continue
    # A task that moves the LIVE path of a looped recorded binary out of
    # the way must be conditional on that binary being restorable.
    if ".prev-" in cmd.split(" ")[1] or "prev_path" in cmd.split(" ")[1]:
        continue
    if "when" not in t:
        fails.append("rescue task %r runs `%s` for every recorded binary with no "
                     "`when:` — it can empty an install path it cannot refill (K078)"
                     % (t.get("name"), cmd))

print("\n".join(fails) if fails else
      "record carries live_path+prev_path; every looped destructive mv in the rescue is gated")
sys.exit(1 if fails else 0)
PY
src_rc=$?
if [ "$src_rc" -eq 0 ]; then ok "structural: $(cat "$SOUT")"; else bad "structural: $(cat "$SOUT")"; fi
rm -f "$SOUT"

# ── behavioural ────────────────────────────────────────────────────────
if ! command -v ansible-playbook >/dev/null; then
  echo "deploy-rollback-test: FAIL — ansible-playbook not on PATH (this test must not pass vacuously)" >&2
  exit 1
fi
if ! command -v docker >/dev/null; then
  echo "deploy-rollback-test: FAIL — docker not on PATH; the behavioural half runs the real task file against a throwaway container (it writes $SIDECARS)" >&2
  exit 1
fi
# Land the probe's output in a variable before slicing it: a `| grep -q`
# under pipefail kills the producer with SIGPIPE (lint-shell-sigpipe).
collections="$(ansible-galaxy collection list community.docker 2>&1)"
case "$collections" in
  *community.docker*) ;;
  *) echo "deploy-rollback-test: FAIL — the community.docker collection is not installed (needed for the container connection): $collections" >&2
     exit 1 ;;
esac

TASKFILE_ABS="$(cd "$(dirname "$TASKFILE")" && pwd)/$(basename "$TASKFILE")"
CT="$(mktemp -d)"                       # controller side: dist + fixture
CNAME="si-rollback-test-$$"
docker rm -f "$CNAME" >/dev/null 2>&1
if ! docker run -d --name "$CNAME" --entrypoint sleep "$IMAGE" infinity >/dev/null; then
  echo "deploy-rollback-test: FAIL — could not start the $IMAGE target container" >&2
  exit 1
fi
cleanup() { rm -rf "$CT"; docker rm -f "$CNAME" >/dev/null 2>&1; }
trap cleanup EXIT

t_exec() { docker exec "$CNAME" bash -c "$1"; }
BIN=/opt/si-bin                         # stand-in for /usr/local/bin

# Controller-side release artefacts. The CLI smoke test in the task file
# fails a binary under 1024 bytes — that is the real, in-file failure
# trigger for the binary whose probe must fail.
mkdir -p "$CT/dist"
mk_art() {  # mk_art <binary> <marker> <big|small>
  {
    printf '%s\n' "$2"
    [ "$3" = "big" ] && head -c 2048 /dev/zero | tr '\0' 'x'
  } > "$CT/dist/$1-linux-amd64"
}
mk_art si-alpha   alpha-new   big
mk_art si-bravo   bravo-new   big
mk_art si-charlie charlie-new big
mk_art si-delta   delta-new   small     # < 1024 bytes → CLI smoke fails

# Host pre-state:
#   si-alpha   — prior install, tracked by a sidecar        (restorable)
#   si-bravo   — NOTHING on the host: first ever install    (unrestorable)
#   si-charlie — prior install, NO sidecar (untracked)      (restorable)
#   si-delta   — prior install; this run's artefact is bad  (the failure)
#   si-echo    — on the host, never named by this deploy    (must not move)
t_exec "mkdir -p '$BIN' '$SIDECARS' \
  && printf 'alpha-old\n'       > '$BIN/si-alpha' \
  && printf 'charlie-old\n'     > '$BIN/si-charlie' \
  && printf 'delta-old\n'       > '$BIN/si-delta' \
  && printf 'echo-untouched\n'  > '$BIN/si-echo' \
  && chmod 0755 '$BIN'/si-* \
  && printf 'v0.0.1' > '$SIDECARS/si-alpha' \
  && printf 'v0.0.1' > '$SIDECARS/si-delta' \
  && printf 'v0.0.1' > '$SIDECARS/si-echo'" >/dev/null

cat > "$CT/fixture.yml" <<YML
- hosts: all
  gather_facts: false
  become: false
  vars:
    version: $VERSION
    local_dist_dir: $CT/dist
    install_dir: $BIN
    api_port: 3000
    api_health_path: /v1/healthz
    health_grace_seconds: 0
    # Every fixture binary is a "CLI" so the systemd tasks short-circuit;
    # what this test pins is the filesystem half of the rescue.
    cli_binaries: [si-alpha, si-bravo, si-charlie, si-delta, si-echo]
  tasks:
    - ansible.builtin.include_tasks: $TASKFILE_ABS
      loop: [si-alpha, si-bravo, si-charlie, si-delta]
      loop_control:
        loop_var: binary
YML

run_play() {  # sets $out, $rc
  out="$(cd "$CT" && ANSIBLE_LOCALHOST_WARNING=false ANSIBLE_INVENTORY_UNPARSED_WARNING=false \
         ansible-playbook -i "$CNAME," -c community.docker.docker fixture.yml 2>&1)"; rc=$?
}

# content <path> — file content, or the literal "<absent>"
content() { t_exec "if [ -e '$1' ]; then cat '$1'; else printf '<absent>'; fi" 2>/dev/null; }
head1()   { t_exec "if [ -e '$1' ]; then head -1 '$1'; else printf '<absent>'; fi" 2>/dev/null; }
listing() { t_exec "ls -1 '$BIN' | LC_ALL=C sort"; }

want_head() {  # want_head <label> <path> <expected first line>
  got="$(head1 "$2")"
  if [ "$got" = "$3" ]; then ok "$1"; else bad "$1 — $2 is '$got', want '$3'"; fi
}
want_exact() {  # want_exact <label> <path> <expected content>
  got="$(content "$2")"
  if [ "$got" = "$3" ]; then ok "$1"; else bad "$1 — $2 is '$got', want '$3'"; fi
}

echo "deploy-rollback-test: behavioural (target=$CNAME, $IMAGE)"
echo_before="$(content "$BIN/si-echo")$(content "$SIDECARS/si-echo")"

# ── run 1 — si-delta's probe fails, everything earlier rolls back ───────
run_play
if [ "$rc" -ne 0 ]; then ok "h. the play fails (rc=$rc)"
else bad "h. the play exited 0 — a failed deploy must be non-zero: $out"; fi

# c. first-ever install must survive the rollback it cannot undo
want_head "c. first-install binary si-bravo is NOT deleted by the rollback" "$BIN/si-bravo" "bravo-new"
want_exact "c. si-bravo's sidecar names the version actually on disk" "$SIDECARS/si-bravo" "$VERSION"

# d. a tracked prior install is restored byte-for-byte
want_head "d. si-alpha restored to its prior build" "$BIN/si-alpha" "alpha-old"
want_head "d. si-alpha's superseded build parked for forensics" "$BIN/si-alpha.rolledback-$VERSION" "alpha-new"
want_exact "d. si-alpha's sidecar restored" "$SIDECARS/si-alpha" "v0.0.1"

# e. untracked prior install: binary back, sidecar back to ABSENT
want_head "e. si-charlie (untracked prior install) restored" "$BIN/si-charlie" "charlie-old"
want_exact "e. si-charlie's sidecar returns to absent, not a synthetic tag" "$SIDECARS/si-charlie" "<absent>"

# f. the failing binary itself
want_head "f. si-delta restored to its prior build" "$BIN/si-delta" "delta-old"
want_head "f. si-delta's bad build parked at .failed-$VERSION" "$BIN/si-delta.failed-$VERSION" "delta-new"
want_exact "f. si-delta's sidecar restored" "$SIDECARS/si-delta" "v0.0.1"

# g. a binary this deploy never named
if [ "$(content "$BIN/si-echo")$(content "$SIDECARS/si-echo")" = "$echo_before" ] \
   && [ "$(t_exec "ls -1 '$BIN' | grep -c '^si-echo'")" = "1" ]; then
  ok "g. si-echo — a binary this deploy never named — is untouched"
else
  bad "g. si-echo changed: $(listing)"
fi

# h. the failure message must name what it could NOT put back
if grep -q "si-bravo" <<<"$out" && grep -qi "not rolled back" <<<"$out"; then
  ok "h. the failure names si-bravo as NOT rolled back"
else
  bad "h. the failure message does not name si-bravo as un-rolled-back (it claims a clean rollback)"
fi

# ── run 2 — the same failing deploy again: convergent, not cumulative ───
before2="$(t_exec "for b in si-alpha si-bravo si-charlie si-delta si-echo; do \
  printf '%s=' \"\$b\"; head -1 '$BIN'/\$b 2>/dev/null || printf '<absent>'; done")"
run_play
after2="$(t_exec "for b in si-alpha si-bravo si-charlie si-delta si-echo; do \
  printf '%s=' \"\$b\"; head -1 '$BIN'/\$b 2>/dev/null || printf '<absent>'; done")"
if [ "$rc" -ne 0 ] && [ "$after2" = "$before2" ] && ! grep -q '<absent>' <<<"$after2"; then
  ok "i. a repeated failing deploy is convergent — no binary lost, no content changed"
else
  bad "i. repeated rollback is cumulative: before='$before2' after='$after2' rc=$rc"
fi

# ── run 3 — the FIRST binary fails: nothing was recorded yet ───────────
# The whole-deploy half must render and act correctly over an empty
# record set, which is the shape every single-binary deploy takes.
cat > "$CT/fixture-solo.yml" <<YML
- hosts: all
  gather_facts: false
  become: false
  vars:
    version: $VERSION
    local_dist_dir: $CT/dist
    install_dir: $BIN
    api_port: 3000
    api_health_path: /v1/healthz
    health_grace_seconds: 0
    cli_binaries: [si-alpha, si-bravo, si-charlie, si-delta, si-echo]
  tasks:
    - ansible.builtin.include_tasks: $TASKFILE_ABS
      loop: [si-delta]
      loop_control:
        loop_var: binary
YML
before3="$(listing)"
out="$(cd "$CT" && ANSIBLE_LOCALHOST_WARNING=false ANSIBLE_INVENTORY_UNPARSED_WARNING=false \
       ansible-playbook -i "$CNAME," -c community.docker.docker fixture-solo.yml 2>&1)"; rc=$?
if [ "$rc" -ne 0 ] && [ "$(head1 "$BIN/si-delta")" = "delta-old" ] \
   && ! grep -q "Also rolled back" <<<"$out" && ! grep -q "NOT rolled back" <<<"$out"; then
  ok "j. a single-binary deploy fails and rolls back with an empty record set"
else
  bad "j. single-binary deploy: rc=$rc si-delta=$(head1 "$BIN/si-delta")"$'\n'"$out"
fi
if [ "$(listing)" = "$before3" ]; then ok "j. no other install path changed"
else bad "j. install dir changed:"$'\n'"$(listing)"$'\n'"was:"$'\n'"$before3"; fi

# ── run 4 — the deploy fails BEFORE the new build reaches the live path ─
# The backup `mv` is the first thing the guarded block does, and it errors
# on a sidecar that is not a single word (these are written by hand during
# a manual rollback — see docs/operations/rollback.md). The live path then
# still holds the working build this run never replaced, and the rescue
# must leave it alone rather than park it as "the bad binary".
t_exec "printf 'foxtrot-old\n' > '$BIN/si-foxtrot' && chmod 0755 '$BIN/si-foxtrot' \
  && printf 'v0.0.1 stray' > '$SIDECARS/si-foxtrot'" >/dev/null
mk_art si-foxtrot foxtrot-new big
cat > "$CT/fixture-early.yml" <<YML
- hosts: all
  gather_facts: false
  become: false
  vars:
    version: $VERSION
    local_dist_dir: $CT/dist
    install_dir: $BIN
    api_port: 3000
    api_health_path: /v1/healthz
    health_grace_seconds: 0
    cli_binaries: [si-alpha, si-bravo, si-charlie, si-delta, si-echo, si-foxtrot]
  tasks:
    - ansible.builtin.include_tasks: $TASKFILE_ABS
      loop: [si-foxtrot]
      loop_control:
        loop_var: binary
YML
out="$(cd "$CT" && ANSIBLE_LOCALHOST_WARNING=false ANSIBLE_INVENTORY_UNPARSED_WARNING=false \
       ansible-playbook -i "$CNAME," -c community.docker.docker fixture-early.yml 2>&1)"; rc=$?
if [ "$rc" -ne 0 ]; then ok "k. a pre-swap failure fails the play (rc=$rc)"
else bad "k. a pre-swap failure exited 0: $out"; fi
want_head "k. the build this run never replaced is still installed" "$BIN/si-foxtrot" "foxtrot-old"
want_exact "k. it was not parked as the bad binary" "$BIN/si-foxtrot.failed-$VERSION" "<absent>"

echo "deploy-rollback-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
