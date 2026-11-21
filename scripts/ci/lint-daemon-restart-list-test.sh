#!/usr/bin/env bash
# lint-daemon-restart-list-test.sh — fixture tests for lint-daemon-restart-list.sh.
# Run: bash scripts/ci/lint-daemon-restart-list-test.sh
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
LINT="$PWD/scripts/ci/lint-daemon-restart-list.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
pass=0
fail=0
S="$TMP/roles/r/templates/systemd"
mkdir -p "$S"

playbook() { # $1 = cli_daemon_units block body (may be empty)
  printf '%s\n' '- hosts: all' '  vars:' '    cli_binaries:' '      - stellarindex-ops' \
    '      - stellarindex-migrate  # no unit' >"$TMP/playbook.yml"
  [ -z "$1" ] || printf '%s\n' '    cli_daemon_units:' "$1" >>"$TMP/playbook.yml"
  printf '%s\n' '  tasks: []' >>"$TMP/playbook.yml"
}
unit() { # $1 = name, $2 = Type line (or empty), $3.. = ExecStart lines
  n="$1" t="$2"; shift 2
  { printf '[Service]\n'; [ -z "$t" ] || printf '%s\n' "$t"; printf '%s\n' "$@"; } >"$S/$n.service.j2"
}
run() { # $1 = case name, $2 = expected rc
  OUT="$(ROLES_DIR="$TMP/roles" PLAYBOOK="$TMP/playbook.yml" bash "$LINT" 2>&1)"
  rc=$?
  if [ "$rc" = "$2" ]; then
    echo "ok: $1"; pass=$((pass + 1))
  else
    echo "FAIL: $1 (rc=$rc, want $2)" >&2; printf '%s\n' "$OUT" >&2; fail=$((fail + 1))
  fi
}

playbook '      stellarindex-ops:
        - follower'
unit follower Type=simple 'ExecStart=/usr/local/bin/stellarindex-ops ch-x -follow'
unit rollup Type=oneshot 'ExecStart=/usr/local/bin/stellarindex-ops ch-rollup'
unit nightly Type=notify "ExecStart=/usr/local/sbin/run-heavy-job.sh v \\" '  /usr/local/bin/stellarindex-ops verify'
touch "$S/nightly.timer.j2"
unit stellarindex-api Type=simple 'ExecStart=/usr/local/bin/stellarindex-api -config x'
unit other Type=simple 'ExecStart=/usr/bin/stellar-core run'
run 'listed daemon, oneshot, timer-driven notify, own-unit daemon, non-release daemon → OK' 0

unit tailer '' "ExecStart=/usr/local/sbin/wrap.sh \\" '  /usr/local/bin/stellarindex-ops ch-y -follow'
run 'unlisted daemon (default Type, binary on a continuation line) → FAIL' 1
unit tailer Type=oneshot "ExecStart=/usr/local/sbin/wrap.sh \\" '  /usr/local/bin/stellarindex-ops ch-y'
run 'the same unit as a oneshot → OK' 0

unit api-shadow Type=simple 'ExecStart=/usr/local/bin/stellarindex-api -config y'
run 'non-CLI binary run by a differently named daemon → FAIL' 1
rm -f "$S/api-shadow.service.j2"

playbook '      stellarindex-ops:
        - follower
        - gone'
run 'listed unit with no long-running template → FAIL (stale entry)' 1

playbook '      stellarindex-ops:
        - follower
      stellarindex-migrate:
        - rollup'
run 'listed unit that is a oneshot → FAIL (stale entry)' 1

playbook ''
run 'playbook without cli_daemon_units → FAIL' 1

echo "lint-daemon-restart-list-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
