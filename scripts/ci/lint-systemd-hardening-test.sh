#!/usr/bin/env bash
# lint-systemd-hardening-test.sh — fixture tests for lint-systemd-hardening.sh.
#
# The load-bearing cases are the two unit shapes found in the
# real role: pgbackrest-backup (a User= and no sandbox at all) and
# node-healthcheck (a sandbox and no User=, so root by omission), plus the
# pgbackrest timer's zone-less 02:00. Each must fail undeclared.
#
# Run: bash scripts/ci/lint-systemd-hardening-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
GATE="$PWD/scripts/ci/lint-systemd-hardening.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

ROLE_DIR="$TMP/role"
UNITS="$ROLE_DIR/templates/systemd"
DEPLOY="$TMP/deploy_systemd"
mkdir -p "$UNITS"

pass=0
fail=0

run() {
  OUT="$(SYSTEMD_HARDENING_ROLE="$ROLE_DIR" SYSTEMD_HARDENING_DEPLOY_DIR="$DEPLOY" bash "$GATE" 2>&1)"
  RC=$?
}

# expect <name> <want-rc> [<want-substring>]
expect() {
  local name="$1" want_rc="$2" want_sub="${3:-}"
  if [ "$RC" -ne "$want_rc" ]; then
    echo "FAIL: $name — exit $RC, want $want_rc" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  if [ -n "$want_sub" ] && ! grep -qF -- "$want_sub" <<<"$OUT"; then
    echo "FAIL: $name — output missing '$want_sub'" >&2
    printf '%s\n' "$OUT" | sed 's/^/    /' >&2
    fail=$((fail + 1))
    return
  fi
  echo "ok: $name"
  pass=$((pass + 1))
}

hardened() {
  cat <<'EOF'
[Service]
Type=oneshot
User=stellarindex
ExecStart=/usr/local/bin/example
NoNewPrivileges=true
PrivateTmp=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
EOF
}

# A clean baseline: one hardened unit and one pinned timer.
hardened > "$UNITS/example.service.j2"
printf '[Timer]\nOnCalendar=*-*-* 03:00:00 UTC\n' > "$UNITS/example.timer.j2"
run
expect 'a fully hardened unit and a UTC timer pass' 0 'OK'

# ── 1. pgbackrest-backup's old shape: User=, no sandbox ─────────────────────
cat > "$UNITS/backup.service.j2" <<'EOF'
[Service]
Type=oneshot
User=postgres
Group=postgres
ExecStart=/usr/local/bin/pgbackrest-backup.sh
Nice=10
EOF
run
expect 'a unit with no sandbox directives fails' 1 'backup.service.j2 does not set NoNewPrivileges'
expect 'every missing baseline directive is named' 1 'does not set ProtectControlGroups'
rm "$UNITS/backup.service.j2"

# ── 2. node-healthcheck's old shape: sandbox, no User= ───────────────────────
hardened | grep -v '^User=' > "$UNITS/probe.service.j2"
run
expect 'a unit that runs as root by omission fails' 1 'probe.service.j2 does not set User'

# ── 3. DynamicUser=yes names the account and implies two directives ──
hardened | grep -vE '^(User|NoNewPrivileges|PrivateTmp)=' > "$UNITS/probe.service.j2"
echo 'DynamicUser=yes' >> "$UNITS/probe.service.j2"
run
expect 'DynamicUser=yes satisfies User, NoNewPrivileges and PrivateTmp' 0 'OK'

# ── 4. A directive set to false is not set ──────────────────────────
hardened | sed 's/^NoNewPrivileges=true/NoNewPrivileges=false/' > "$UNITS/probe.service.j2"
run
expect 'NoNewPrivileges=false counts as missing' 1 'probe.service.j2 does not set NoNewPrivileges'

# ── 5. Declaring the gap passes; a declaration outliving it fails ────
echo 'probe.service:NoNewPrivileges  # drops privilege with sudo' > "$UNITS/HARDENING-EXCEPTIONS"
run
expect 'a declared exception passes' 0 'OK'
hardened > "$UNITS/probe.service.j2"
run
expect 'a declared exception the unit no longer needs fails as stale' 1 'STALE'
rm "$UNITS/probe.service.j2" "$UNITS/HARDENING-EXCEPTIONS"

# ── 6. Timers: a fixed hour needs a zone ────────────────────────────
printf '[Timer]\nOnCalendar=*-*-* 02:00:00\n' > "$UNITS/backup.timer.j2"
run
expect 'a fixed-hour OnCalendar without UTC fails' 1 'backup.timer.j2 OnCalendar=*-*-* 02:00:00'
printf '[Timer]\nOnCalendar=*:0/15\nOnCalendar=hourly\n' > "$UNITS/backup.timer.j2"
run
expect 'minute-only and shorthand calendars need no zone' 0 'OK'
printf '[Timer]\nOnCalendar={{ backup_on_calendar }}\n' > "$UNITS/backup.timer.j2"
run
expect 'a templated calendar is left to its default' 0 'OK'

# ── 7. A timer-driven unit must not start a datastore ───────────────
{ printf '[Unit]\nAfter=clickhouse-server.service\nWants=clickhouse-server.service\n'; hardened; } > "$UNITS/backup.service.j2"
printf '[Timer]\nOnCalendar=hourly\n' > "$UNITS/backup.timer.j2"
run
expect 'a timer-driven unit that Wants= a datastore fails' 1 "backup.service.j2 is timer-driven and has 'Wants=clickhouse-server.service'"
{ printf '[Unit]\nAfter=postgresql.service\nRequires=postgresql@15-main.service\n'; hardened; } > "$UNITS/backup.service.j2"
run
expect 'Requires= on a datastore instance fails too' 1 'STARTS-DATASTORE'
{ printf '[Unit]\nAfter=clickhouse-server.service minio.service\nWants=network-online.target\n'; hardened; } > "$UNITS/backup.service.j2"
run
expect 'After= on a datastore and Wants= on the network pass' 0 'OK'
rm "$UNITS/backup.timer.j2"
{ printf '[Unit]\nRequires=postgresql.service\n'; hardened; } > "$UNITS/backup.service.j2"
run
expect 'a long-running unit with no timer is not judged' 0 'OK'
rm "$UNITS/backup.service.j2"

# ── 9. A unit installed verbatim from deploy/systemd is judged too ──
mkdir -p "$ROLE_DIR/tasks" "$DEPLOY"
cat > "$ROLE_DIR/tasks/10-install.yml" <<'EOF'
- name: install a unit straight from deploy/systemd
  ansible.builtin.copy:
    src: "{{ playbook_dir }}/../../../deploy/systemd/{{ item }}"
    dest: "/etc/systemd/system/{{ item }}"
  loop:
    - verbatim.service
EOF
hardened | grep -v '^NoNewPrivileges=' > "$DEPLOY/verbatim.service"
run
expect 'a verbatim-installed unit without NoNewPrivileges fails' 1 'verbatim.service does not set NoNewPrivileges'
hardened > "$DEPLOY/verbatim.service"
run
expect 'a hardened verbatim-installed unit passes' 0 'OK'
rm -rf "$ROLE_DIR/tasks" "$DEPLOY"

# ── 10. The API unit runs as its own uid with only its own env file ──
mkdir -p "$ROLE_DIR/defaults"
printf 'stellarindex_api_os_user: "stellarindex-api"\n' > "$ROLE_DIR/defaults/main.yml"
hardened > "$UNITS/stellarindex-indexer.service.j2"
api_unit() {
  hardened | sed 's/^User=stellarindex$/User={{ stellarindex_api_os_user }}/'
  cat <<'EOF'
EnvironmentFile=/etc/default/stellarindex-api
ProtectProc=invisible
InaccessiblePaths=-/run/lock -/etc/default/stellarindex -/etc/default/stellarindex-api -/etc/default/galexie -/etc/default/galexie-backfill -/etc/default/stellarindex-ops -/etc/clickhouse-client
SystemCallFilter=@system-service
TasksMax=4096
MemoryMax=8G
EOF
}
printf 'STELLARINDEX_POSTGRES_DSN={{ x }}\n{%% if y %%}\nMASSIVE_API_KEY={{ z }}\n{%% endif %%}\n' > "$UNITS/../stellarindex-api.env.j2"
api_unit > "$UNITS/stellarindex-api.service.j2"
run
expect 'an API on its own uid with an allow-listed env file passes' 0 'OK'
api_unit | sed 's/^User=.*/User=stellarindex/' > "$UNITS/stellarindex-api.service.j2"
run
expect "an API on the indexer's uid fails" 1 "API-SHARED-UID: $UNITS/stellarindex-api.service.j2 runs as 'stellarindex'"
{ api_unit; echo 'SupplementaryGroups=galexie'; } > "$UNITS/stellarindex-api.service.j2"
run
expect 'an API with supplementary groups fails' 1 'sets SupplementaryGroups='
{ api_unit; echo 'EnvironmentFile=-/etc/default/stellarindex'; } > "$UNITS/stellarindex-api.service.j2"
run
expect 'an API reading the shared env file fails' 1 'API-SHARED-ENV'
api_unit | grep -v '^ProtectProc=' > "$UNITS/stellarindex-api.service.j2"
run
expect 'an API without ProtectProc=invisible fails' 1 'API-PROC'
api_unit | sed 's# -/etc/default/stellarindex # #' > "$UNITS/stellarindex-api.service.j2"
run
expect 'an API that can see the shared env file fails' 1 'does not list /etc/default/stellarindex in InaccessiblePaths='
api_unit | sed 's# -/etc/default/stellarindex-api # #' > "$UNITS/stellarindex-api.service.j2"
run
expect 'an API whose children can read its own env file fails' 1 'does not list /etc/default/stellarindex-api in InaccessiblePaths='
api_unit > "$UNITS/stellarindex-api.service.j2"
printf 'STELLARINDEX_POSTGRES_DSN={{ x }}\nCOINGECKO_API_KEY={{ y }}\n' > "$UNITS/../stellarindex-api.env.j2"
run
expect 'an ingest-only credential in the API env file fails' 1 'API-ENV-FORBIDDEN'
printf 'STELLARINDEX_POSTGRES_DSN={{ x }}\nSOME_NEW_SECRET={{ y }}\n' > "$UNITS/../stellarindex-api.env.j2"
run
expect 'a name outside the allow-list fails' 1 'API-ENV-UNLISTED'
rm "$UNITS/../stellarindex-api.env.j2"
run
expect 'a role with the API unit and no API env file fails' 1 'API-ENV:'
rm -rf "$ROLE_DIR/defaults" "$UNITS/stellarindex-api.service.j2" "$UNITS/stellarindex-indexer.service.j2"

# ── 8. An empty scan is a broken scan, not a pass ───────────────────
rm -f "$UNITS"/*.j2
run
expect 'no templates found fails loudly' 2 'the scan is broken'

echo "lint-systemd-hardening-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
