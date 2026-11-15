#!/usr/bin/env bash
# psql-dsn-test: the wrapper hands psql its connection in PG* variables and never on the command line.
# Runs against a fake psql that prints what it was given; no Postgres is contacted.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/psql-dsn.sh"
pass=0; fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }

work="$(mktemp -d "${TMPDIR:-/tmp}/psql-dsn-test.XXXXXX")"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin"
cat > "$work/bin/psql" <<'SH'
#!/usr/bin/env bash
printf 'ARGV:%s\n' "$*"
env | grep -E '^PG[A-Z_]+=' | sort
SH
chmod +x "$work/bin/psql"

# run <dsn> [args…]: output in $OUT, status in $RC. A clean PG* environment, so only the wrapper's exports show.
run() {
  local dsn="$1"; shift
  OUT="$(env -i PATH="$work/bin:$PATH" PGDSN="$dsn" bash "$SCRIPT" "$@" 2>&1)"; RC=$?
}
has() { grep -qxF -- "$1" <<<"$OUT"; }

secret='p@ss:w/rd?&x'
run 'postgres://owner:p%40ss%3Aw%2Frd%3F%26x@127.0.0.1:5432/stellarindex?sslmode=disable&connect_timeout=5' -tAc 'SELECT 1'
if [[ "$RC" -eq 0 ]] && has "PGHOST=127.0.0.1" && has "PGPORT=5432" && has "PGUSER=owner" && has "PGDATABASE=stellarindex" \
     && has "PGPASSWORD=$secret" && has "PGSSLMODE=disable" && has "PGCONNECT_TIMEOUT=5"; then
  ok "a URI is split into PG* variables, percent-escapes decoded"
else
  bad "URI split: rc=$RC
$(printf '%s\n' "$OUT" | sed 's/^PGPASSWORD=.*/PGPASSWORD=<set>/')"
fi
if has "ARGV:-tAc SELECT 1" && ! grep -q 'postgres://' <<<"$OUT" && ! grep -q '^PGDSN=' <<<"$OUT"; then
  ok "psql gets the caller's arguments only: no DSN on its command line or in PGDSN"
else
  bad "the DSN reached psql's command line or environment"
fi

run "host=/var/run/postgresql dbname=stellarindex user=ops password='two words'" -c 'SELECT 2'
if [[ "$RC" -eq 0 ]] && has "PGHOST=/var/run/postgresql" && has "PGUSER=ops" && has "PGPASSWORD=two words" && has "PGDATABASE=stellarindex"; then
  ok "a keyword=value DSN is split the same way"
else
  bad "keyword DSN: rc=$RC"
fi

run 'postgresql://u@[::1]:6543,db2:6544/x' -c 'SELECT 3'
if [[ "$RC" -eq 0 ]] && has "PGHOST=::1,db2" && has "PGPORT=6543,6544" && ! grep -q '^PGPASSWORD=' <<<"$OUT"; then
  ok "a bracketed IPv6 host and a host list keep their ports; no password means no PGPASSWORD"
else
  bad "host list: rc=$RC
$OUT"
fi

run 'postgres://owner@db/stellarindex?application_name=a+b&client_encoding=UTF8' -c 'SELECT 5'
if [[ "$RC" -eq 0 ]] && has "PGAPPNAME=a+b" && has "PGCLIENTENCODING=UTF8"; then
  ok "a plus sign in a URI parameter stays a plus, as in libpq"
else
  bad "plus in a parameter: rc=$RC
$OUT"
fi

run 'postgres://owner:hunter2@db/stellarindex?pool_max_conns=4' -c 'SELECT 4'
if [[ "$RC" -eq 2 ]] && grep -q "parameter 'pool_max_conns' has no PG\* environment variable" <<<"$OUT" \
     && ! grep -q 'ARGV:' <<<"$OUT" && ! grep -q 'hunter2' <<<"$OUT"; then
  ok "a parameter with no PG* variable is refused by name, psql is not run, and the password is not echoed"
else
  bad "unknown parameter: rc=$RC
${OUT//hunter2/<password>}"
fi

run '' -c 'SELECT 5'
if [[ "$RC" -eq 2 ]] && ! grep -q 'ARGV:' <<<"$OUT"; then
  ok "an empty PGDSN is a refusal, not a connection to libpq's defaults"
else
  bad "empty PGDSN: rc=$RC"
fi

run 'just-a-database-name' -c 'SELECT 6'
if [[ "$RC" -eq 2 ]] && ! grep -q 'ARGV:' <<<"$OUT"; then
  ok "a string that is neither form is refused"
else
  bad "malformed DSN: rc=$RC"
fi

echo "psql-dsn-test: $pass passed, $fail failed"
[[ "$fail" -eq 0 ]]
