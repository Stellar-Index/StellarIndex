#!/usr/bin/env bash
# lint-deploy-systemd-authority.sh — deploy/systemd/ is MIXED AUTHORITY,
# and this keeps that fact true and visible.
#
# Two kinds of file live in that directory and they behave oppositely:
#
#   AUTHORITATIVE — ansible installs the file straight from here
#     (`src: {{ playbook_dir }}/../../../deploy/systemd/{{ item }}`).
#     Editing it changes production on the next apply.
#
#   REFERENCE — the role ships its own .j2 for the same unit. The copy
#     here is documentation and is NOT what runs; the .j2 is. Editing it
#     changes nothing, which is how several of these drifted (see the
#     drift notes in stellarindex-api.service.j2).
#
# A third state is the finding this lint exists for:
#
#   ORPHAN — installed by nothing and templated nowhere. It looks like a
#     deployed unit, reads like one, and is not one. r1-deployment-state.md
#     documents an operator convention of scp-ing units straight out of
#     this directory, so an orphan is a live footgun: it will start, and
#     nothing will ever reconcile it (wave-D LID-7).
#
# Every orphan must be listed in deploy/systemd/ORPHANS with a reason, so
# adding one is a deliberate act with a written justification rather than
# an accident.
#
# Run: bash scripts/ci/lint-deploy-systemd-authority.sh
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

DIR=deploy/systemd
ROLE=configs/ansible/roles/archival-node
ORPHANS="$DIR/ORPHANS"
fail=0
checked=0

[ -d "$DIR" ] || { echo "lint-deploy-systemd-authority: $DIR missing"; exit 2; }

# Units genuinely installed FROM deploy/systemd, parsed per-task.
authoritative=$(python3 "$(dirname "$0")/deploy-systemd-authoritative.py" "$ROLE")
if [ -z "$authoritative" ]; then
  echo "lint-deploy-systemd-authority: the authoritative set came back EMPTY —" >&2
  echo "  the task shape changed and this lint would classify every unit as an" >&2
  echo "  orphan or a template. Fix the parser, do not delete the check." >&2
  exit 1
fi

declared=""
[ -f "$ORPHANS" ] && declared=$(grep -vE '^\s*#|^\s*$' "$ORPHANS" | sed -E 's/\s*#.*//; s/\s+$//')

for f in "$DIR"/*.service "$DIR"/*.timer; do
  [ -f "$f" ] || continue
  b=$(basename "$f")
  checked=$((checked + 1))

  # AUTHORITATIVE: named in the loop of a task that COPIES/TEMPLATES a
  # src under deploy/systemd — both facts about the SAME task.
  #
  # This used to be two INDEPENDENT greps: "does `- <unit>` appear as a
  # list item anywhere in tasks/" and "does the string
  # `deploy/systemd/{{ item }}` appear anywhere in tasks/". The second is
  # a repo-wide constant — one unrelated task in 15-log-discipline.yml
  # satisfies it for every unit — so classification collapsed to the
  # first, which matches ANY YAML list item. Adding a unit to an
  # `ansible.builtin.systemd` ENABLE loop (installing nothing) was enough
  # to pass as authoritative: precisely the "enable a unit you don't
  # install" footgun this lint exists to name, and which the header
  # itself describes.
  #
  # The set is computed by deploy-systemd-authoritative.py, which parses
  # the task YAML instead of grepping it.
  if printf '%s\n' "$authoritative" | grep -qxF "$b"; then
    continue
  fi
  # REFERENCE: the role templates the same unit itself.
  [ -f "$ROLE/templates/systemd/$b.j2" ] && continue

  # ORPHAN — must be declared.
  if printf '%s\n' "$declared" | grep -qxF "$b"; then
    continue
  fi
  echo "  ORPHAN: $DIR/$b is installed by nothing and has no .j2 in the role."
  echo "          It looks like a deployed unit and is not one. Either wire it"
  echo "          into the role, delete it, or declare it in $ORPHANS with the"
  echo "          reason it is kept."
  fail=$((fail + 1))
done

if [ "$checked" -eq 0 ]; then
  echo "lint-deploy-systemd-authority: FAIL — no unit files found; the scan is broken"
  exit 2
fi

if [ "$fail" -gt 0 ]; then
  echo "lint-deploy-systemd-authority: $fail undeclared orphan unit(s) of $checked checked"
  exit 1
fi
echo "lint-deploy-systemd-authority: OK — $checked unit file(s), every one installed, templated, or a declared orphan."
