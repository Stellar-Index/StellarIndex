#!/usr/bin/env bash
# ansible-postgres-archive-test.sh — the archival-node role must render the
# WAL archiving settings the reference host runs, so a rebuilt host keeps
# pgBackRest's point-in-time recovery, and must leave archiving off where
# pgBackRest is not provisioned (a failing archive_command retains WAL until
# pg_wal fills). Renders the role's real postgresql.conf.j2 in both shapes
# and cross-checks the stanza against the pgBackRest templates.
set -euo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROLE="configs/ansible/roles/archival-node"
TEMPLATE="$ROLE/templates/postgresql.conf.j2"

for f in "$TEMPLATE" "$ROLE/templates/pgbackrest.conf.j2" "$ROLE/templates/pgbackrest-backup.sh.j2"; do
  [ -r "$f" ] || { echo "ansible-postgres-archive-test: missing $f" >&2; exit 2; }
done

# Same interpreter discovery as prometheus-firewall-ipv6-test.sh: a plain
# python3 rarely has jinja2, but ansible-playbook ships its own.
PY=""
for cand in python3 /opt/homebrew/bin/python3; do
  if command -v "$cand" >/dev/null 2>&1 &&
    "$cand" -c 'import jinja2' >/dev/null 2>&1; then
    PY="$cand"
    break
  fi
done
if [ -z "$PY" ] && command -v ansible-playbook >/dev/null 2>&1; then
  cand=$(head -1 "$(command -v ansible-playbook)" | sed 's|^#!||' | awk '{print $1}')
  if [ -x "$cand" ] && "$cand" -c 'import jinja2' >/dev/null 2>&1; then PY="$cand"; fi
fi
if [ -z "$PY" ]; then
  if [ "${CI:-}" = "true" ]; then
    echo "ansible-postgres-archive-test: FAIL — no python3 with jinja2 (required in CI)" >&2
    exit 1
  fi
  echo "ansible-postgres-archive-test: SKIP (no python3 with jinja2 locally; CI enforces)"
  exit 0
fi

"$PY" - "$ROLE" <<'PYEOF'
import re
import sys

import jinja2

role = sys.argv[1]


def ansible_bool(v):
    # Mirrors ansible's `bool` filter: role defaults render "True"/"False" strings.
    if isinstance(v, bool):
        return v
    return str(v).strip().lower() in ("yes", "on", "1", "true", "y")


env = jinja2.Environment(undefined=jinja2.StrictUndefined, keep_trailing_newline=True)
env.filters["bool"] = ansible_bool
template = env.from_string(open(f"{role}/templates/postgresql.conf.j2").read())

base = dict(
    postgres_version="15",
    postgres_listen_addresses="127.0.0.1",
    postgres_port=5432,
    postgres_max_connections=200,
    postgres_shared_buffers="1GB",
    postgres_effective_cache_size="2GB",
    postgres_work_mem="32MB",
    postgres_maintenance_work_mem="1GB",
    postgres_wal_compression="zstd",
    postgres_max_wal_size="16GB",
)


def settings(**extra):
    out = {}
    for line in template.render(**base, **extra).splitlines():
        m = re.match(r"^([a-z_.]+)\s*=\s*('[^']*'|\S+)", line)
        if m:
            out[m.group(1)] = m.group(2)  # last wins, as in Postgres
    return out


failed = False


def check(cond, msg):
    global failed
    print(("  ok   " if cond else "  FAIL ") + msg)
    failed = failed or not cond


stanza = re.search(r"^STANZA=(\S+)$", open(f"{role}/templates/pgbackrest-backup.sh.j2").read(), re.M)
check(stanza is not None, "pgbackrest-backup.sh.j2 declares STANZA=")
stanza = stanza.group(1) if stanza else "<none>"
check(f"[{stanza}]" in open(f"{role}/templates/pgbackrest.conf.j2").read(),
      f"pgbackrest.conf.j2 carries the [{stanza}] stanza section")

want = {
    "wal_level": "replica",
    "archive_mode": "on",
    "archive_command": f"'pgbackrest --stanza={stanza} archive-push %p'",
    "archive_timeout": "0",
    "max_wal_size": "16GB",
}
for label, extra in (("backups enabled (bool)", dict(pgbackrest_backup_enabled=True)),
                     ("backups enabled (defaults string)", dict(pgbackrest_backup_enabled="True")),
                     ("flag unset (defaults to enabled)", {})):
    got = settings(**extra)
    for k, v in want.items():
        check(got.get(k) == v, f"{label}: {k} = {v} (got {got.get(k)})")

for label, flag in (("backups disabled (bool)", False), ("backups disabled (defaults string)", "False")):
    got = settings(pgbackrest_backup_enabled=flag)
    check(got.get("archive_mode", "off") == "off", f"{label}: archive_mode stays off (got {got.get('archive_mode')})")
    check("archive_command" not in got, f"{label}: no archive_command rendered")
    check(got.get("wal_level") == "replica", f"{label}: wal_level = replica")

if failed:
    print("ansible-postgres-archive-test: FAIL", file=sys.stderr)
    sys.exit(1)
print("ansible-postgres-archive-test: PASS")
PYEOF
