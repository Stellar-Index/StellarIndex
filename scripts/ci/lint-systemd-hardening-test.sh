#!/usr/bin/env bash
# lint-systemd-hardening-test.sh — fixture tests for lint-systemd-hardening.sh.
#
# The load-bearing cases are the two unit shapes issue #617 found in the
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
mkdir -p "$UNITS"

pass=0
fail=0

run() {
  OUT="$(SYSTEMD_HARDENING_ROLE="$ROLE_DIR" bash "$GATE" 2>&1)"
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

# ── 1. pgbackrest-backup's pre-#617 shape: User=, no sandbox ─────────
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

# ── 2. node-healthcheck's pre-#617 shape: sandbox, no User= ──────────
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

# ── 7. An empty scan is a broken scan, not a pass ───────────────────
rm -f "$UNITS"/*.j2
run
expect 'no templates found fails loudly' 2 'the scan is broken'

echo "lint-systemd-hardening-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
