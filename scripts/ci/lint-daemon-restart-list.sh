#!/usr/bin/env bash
# lint-daemon-restart-list.sh — every long-running role unit that execs a
# release binary must be restarted by the deploy that replaces that binary.
#
# deploy-one-binary.yml restarts `<binary>.service` for a daemon binary and,
# for a CLI binary (cli_binaries), try-restarts the units in
# cli_daemon_units[<binary>] (both in deploy-binary.yml). A unit outside that
# keeps running the replaced build after every release: cap67-movements ran a
# 37-day-old stellarindex-ops on r1 that way.
#
# A role unit is long-running when its Type is not oneshot (systemd's default
# is simple) and no sibling .timer starts it; a timer-started run execs the new
# build on its next start. Its ExecStart (with continuation lines) runs a
# release binary when it names /usr/local/bin/stellarindex-<name>. Such a unit
# is covered when it IS `<binary>.service` of a non-CLI binary, or is listed in
# cli_daemon_units[<binary>]. Fails on an uncovered unit and on a list entry
# that names no long-running unit execing that binary (a stale entry).
#
# Env overrides (used by the self-test): ROLES_DIR, PLAYBOOK.
# Run: bash scripts/ci/lint-daemon-restart-list.sh
set -euo pipefail
cd "$(dirname "$0")/../.."
ROLES_DIR="${ROLES_DIR:-configs/ansible/roles}"
PLAYBOOK="${PLAYBOOK:-configs/ansible/playbooks/deploy-binary.yml}"
me=lint-daemon-restart-list
[ -f "$PLAYBOOK" ] || { echo "$me: FAIL — playbook not found: $PLAYBOOK" >&2; exit 1; }
[ -d "$ROLES_DIR" ] || { echo "$me: FAIL — roles dir not found: $ROLES_DIR" >&2; exit 1; }

# Items of a top-level-in-vars YAML list or map-of-lists, as "<key> <item>"
# lines (key is "-" for a plain list). Stops at the first line indented no
# deeper than the block's own key.
yaml_block() { # $1 = block key
  awk -v k="$1" '
    { sub(/[[:space:]]+#.*$/, ""); if ($0 ~ /^[[:space:]]*(#|$)/) next
      match($0, /^ */); ind = RLENGTH }
    !grab && $0 ~ "^ *" k ":[[:space:]]*$" { grab = 1; base = ind; key = "-"; next }
    grab && ind <= base { exit }
    grab && /^ *- / { v = $0; sub(/^ *- */, "", v); print key, v; next }
    grab && /^ *[A-Za-z0-9_.-]+:[[:space:]]*$/ { key = $0; gsub(/[ :]/, "", key) }
  ' "$PLAYBOOK"
}
cli="$(yaml_block cli_binaries | awk '{print $2}')"
listed="$(yaml_block cli_daemon_units)"
if [ -z "$cli" ]; then
  echo "$me: FAIL — no cli_binaries list in $PLAYBOOK; this check must not pass vacuously." >&2
  exit 1
fi
if ! grep -qE '^ *cli_daemon_units:' "$PLAYBOOK"; then
  echo "$me: FAIL — no cli_daemon_units in $PLAYBOOK; the deploy has no restart list to check against." >&2
  exit 1
fi
NL='
'
has_line() { case "$NL$1$NL" in *"$NL$2$NL"*) return 0 ;; esac; return 1; } # $1 = lines, $2 = exact line
is_cli() { has_line "$cli" "$1"; }

fail=0
units=0
daemons="" # "<binary> <unit>" for every long-running unit execing a release binary
for f in "$ROLES_DIR"/*/templates/systemd/*.service.j2; do
  [ -f "$f" ] || continue
  units=$((units + 1))
  unit="$(basename "$f" .service.j2)"
  type="$(awk -F= '/^Type=/ { print $2; exit }' "$f")"
  [ "${type:-simple}" = oneshot ] && continue
  [ -f "$(dirname "$f")/$unit.timer.j2" ] && continue
  bins="$(awk '
    /^ExecStart=/ { on = 1 }
    on { print; if ($0 !~ /\\[[:space:]]*$/) on = 0 }
  ' "$f" | grep -oE '/usr/local/bin/stellarindex-[A-Za-z0-9_-]+' | sed 's#.*/##' | sort -u || true)"
  for b in $bins; do
    daemons="$daemons$b $unit
"
    if is_cli "$b"; then
      has_line "$listed" "$b $unit" && continue
      echo "$me: FAIL — $f: Type=${type:-simple} unit $unit runs $b and is not timer-driven, but deploy restarts nothing for it. Add it under cli_daemon_units: $b: in $PLAYBOOK." >&2
      fail=1
    elif [ "$unit" != "$b" ]; then
      echo "$me: FAIL — $f: Type=${type:-simple} unit $unit runs $b, but deploy only restarts $b.service. Run it as $b.service, or make $b a CLI binary with $unit in cli_daemon_units." >&2
      fail=1
    fi
  done
done
if [ "$units" -eq 0 ]; then
  echo "$me: FAIL — no *.service.j2 under $ROLES_DIR/*/templates/systemd; this check must not pass vacuously." >&2
  exit 1
fi
while read -r b u; do
  [ -n "$b" ] || continue
  if ! has_line "$daemons" "$b $u"; then
    echo "$me: FAIL — cli_daemon_units lists $u under $b, but no long-running role unit $u runs $b. Drop the stale entry." >&2
    fail=1
  fi
done <<EOF
$listed
EOF
[ "$fail" -eq 0 ] || exit 1
n="$(printf '%s' "$daemons" | grep -c . || true)"
echo "$me: OK — $units unit template(s), $n long-running release-binary unit(s), all restarted by deploy"
