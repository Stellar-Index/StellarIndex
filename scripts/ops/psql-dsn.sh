#!/usr/bin/env bash
# psql-dsn: run psql with its connection read from the PGDSN variable, never from an argument.
#
#   PGDSN="$STELLARINDEX_POSTGRES_DSN" psql-dsn -tAc "SELECT 1"
#   PSQL_BIN=/usr/lib/postgresql/15/bin/psql names a psql other than the one on PATH.
#
# A DSN on psql's command line is readable by every local user in /proc/<pid>/cmdline for as long as psql
# runs, password included. A process's environment is readable only by its own uid and root, so the DSN is
# split into libpq's PG* variables here and psql is started with no connection argument.
#
# Accepts a postgres:// or postgresql:// URI, or a keyword=value string. A parameter with no PG* variable is
# refused by name (exit 2): dropping it silently would connect differently from what was configured.
set -euo pipefail

if [[ -z "${PGDSN:-}" ]]; then
  echo "psql-dsn: PGDSN is empty; pass the connection string in the environment, not as an argument" >&2
  exit 2
fi
command -v python3 >/dev/null 2>&1 || { echo "psql-dsn: python3 not found (needed to split the DSN)" >&2; exit 2; }

# The DSN reaches python in its environment; only the program text is on python's command line.
read -r -d '' PARSER <<'PY' || true
import os, shlex, sys
from urllib.parse import unquote

ENV = {
    "host": "PGHOST", "hostaddr": "PGHOSTADDR", "port": "PGPORT", "user": "PGUSER", "password": "PGPASSWORD",
    "dbname": "PGDATABASE", "sslmode": "PGSSLMODE", "sslrootcert": "PGSSLROOTCERT", "sslcert": "PGSSLCERT",
    "sslkey": "PGSSLKEY", "connect_timeout": "PGCONNECT_TIMEOUT", "application_name": "PGAPPNAME",
    "options": "PGOPTIONS", "target_session_attrs": "PGTARGETSESSIONATTRS", "channel_binding": "PGCHANNELBINDING",
    "passfile": "PGPASSFILE", "service": "PGSERVICE", "gssencmode": "PGGSSENCMODE",
    "client_encoding": "PGCLIENTENCODING", "sslcrl": "PGSSLCRL", "sslcrldir": "PGSSLCRLDIR", "sslsni": "PGSSLSNI",
    "requirepeer": "PGREQUIREPEER", "require_auth": "PGREQUIREAUTH", "load_balance_hosts": "PGLOADBALANCEHOSTS",
    "ssl_min_protocol_version": "PGSSLMINPROTOCOLVERSION", "ssl_max_protocol_version": "PGSSLMAXPROTOCOLVERSION",
    "krbsrvname": "PGKRBSRVNAME", "gsslib": "PGGSSLIB",
}


def die(msg):
    sys.stderr.write("psql-dsn: " + msg + "\n")
    sys.exit(2)


def from_uri(rest):
    out = {}
    rest, _, query = rest.partition("?")
    netloc, _, path = rest.partition("/")
    userinfo, at, hostspec = netloc.rpartition("@")
    if at:
        user, colon, password = userinfo.partition(":")
        if user:
            out["user"] = unquote(user)
        if colon:
            out["password"] = unquote(password)
    hosts, ports = [], []
    for part in hostspec.split(",") if hostspec else []:
        if part.startswith("["):
            host, _, port = part[1:].partition("]")
            port = port.lstrip(":")
        else:
            host, _, port = part.partition(":")
        hosts.append(unquote(host))
        ports.append(port)
    if any(hosts):
        out["host"] = ",".join(hosts)
    if any(ports):
        out["port"] = ",".join(ports)
    if path:
        out["dbname"] = unquote(path)
    # Not parse_qsl: it reads "+" as a space, and libpq does not.
    for pair in query.split("&") if query else []:
        key, _, value = pair.partition("=")
        out[unquote(key)] = unquote(value)
    return out


def from_keywords(dsn):
    out = {}
    try:
        words = shlex.split(dsn)
    except ValueError:
        die("the keyword=value DSN has an unbalanced quote")
    for word in words:
        key, eq, value = word.partition("=")
        if not eq:
            die("the DSN is neither a postgres:// URI nor keyword=value pairs")
        out[key.strip()] = value
    return out


dsn = os.environ.get("PGDSN", "")
for scheme in ("postgresql://", "postgres://"):
    if dsn.startswith(scheme):
        params = from_uri(dsn[len(scheme):])
        break
else:
    params = from_keywords(dsn)

for key, value in params.items():
    if key not in ENV:
        die("DSN parameter '%s' has no PG* environment variable; refusing to connect without it" % key)
    if "\0" in value or "\n" in value:
        die("DSN parameter '%s' holds a newline or NUL" % key)
    sys.stdout.write(ENV[key] + "=" + value + "\n")
sys.stdout.write("#ok\n")
PY

# Straight from the parser into this shell's environment: the password never touches a file. "#ok" is the parser's
# last line, so a parser that died part-way is not mistaken for a short DSN.
parsed=0
while IFS= read -r line; do
  if [[ "$line" == "#ok" ]]; then parsed=1; else export "${line?}"; fi
done < <(python3 -I -c "$PARSER")
[[ "$parsed" -eq 1 ]] || exit 2
unset PGDSN PARSER
exec "${PSQL_BIN:-psql}" "$@"
