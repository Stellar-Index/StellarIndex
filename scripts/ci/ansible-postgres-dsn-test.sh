#!/usr/bin/env bash
# ansible-postgres-dsn-test.sh — the archival-node role's Postgres DSN must
# carry its credentials percent-encoded (CA2-A37-harden-9).
#
# The DSN is a URL. A raw '/', '#', '@', '?' or ':' in the password ends the
# userinfo early, so pgx / lib/pq fail with `invalid port ":ab" after host`
# and the migrate step and every service refuse to start — and the bootstrap
# runbook used to recommend `openssl rand -base64 32`, which emits '/' about
# half the time.
#
# What must hold:
#   1. no Ansible file composes `postgres://{{ user }}:{{ pass }}@` inline;
#      the one composition is postgres_dsn_stellarindex in defaults/main.yml
#      and every consumer (the env file, the ops env file, the migrate step)
#      renders that variable;
#   2. a real `ansible-playbook -c local` render of the shipped
#      stellarindex.env.j2 with a hostile user/password/db yields a DSN that
#      a URL parser splits back into exactly those values;
#   3. a hex password (what credential-rotation.md prescribes, and what a
#      working deploy already has) renders byte-identical to the raw form.
#
# Needs ansible-playbook and python3 (the ci.yml ansible-check job has both).
# Run: bash scripts/ci/ansible-postgres-dsn-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ANSIBLE_DIR="${ANSIBLE_DIR:-$PWD/configs/ansible}"   # override for the red-proof against a pre-fix copy
ROLE="$ANSIBLE_DIR/roles/archival-node"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

for tool in ansible-playbook python3; do
  if ! command -v "$tool" >/dev/null; then
    echo "ansible-postgres-dsn-test: FAIL — $tool not on PATH (this test must not pass vacuously)" >&2
    exit 1
  fi
done

# ── 1. one composition, every consumer renders it ─────────────────────────
inline=$(grep -rnE --include='*.yml' --include='*.yaml' --include='*.j2' \
  'postgres(ql)?://[^[:space:]]*\{\{[^}]*\}\}:\{\{' "$ANSIBLE_DIR" \
  | grep -v "^$ROLE/defaults/main.yml:" || true)
if [ -z "$inline" ]; then
  ok "no inline user:password DSN composition outside defaults/main.yml"
else
  bad "inline DSN composition(s) bypass postgres_dsn_stellarindex:"
  printf '%s\n' "$inline"
fi
for f in templates/stellarindex.env.j2 tasks/09-minio.yml tasks/14-stellarindex-services.yml; do
  if grep -qE 'STELLARINDEX_POSTGRES_DSN(=|: ")\{\{ postgres_dsn_stellarindex \}\}' "$ROLE/$f"; then
    ok "$f renders postgres_dsn_stellarindex"
  else
    bad "$f does not render STELLARINDEX_POSTGRES_DSN from postgres_dsn_stellarindex"
  fi
done

# ── 2/3. real render of the shipped env template ─────────────────────────
cat > "$TMP/play.yml" <<YML
- hosts: localhost
  gather_facts: false
  vars_files:
    - $ROLE/defaults/main.yml
    - $TMP/creds.json
  tasks:
    - ansible.builtin.template:
        src: $ROLE/templates/stellarindex.env.j2
        dest: $TMP/stellarindex.env
        mode: "0600"
YML

render() { # render <creds-json> -> prints the DSN line's value, or nothing
  printf '%s' "$1" > "$TMP/creds.json"
  rm -f "$TMP/stellarindex.env"
  if ! ANSIBLE_LOCALHOST_WARNING=false ANSIBLE_INVENTORY_UNPARSED_WARNING=false \
       ANSIBLE_PYTHON_INTERPRETER=auto_silent \
       ansible-playbook -c local -i localhost, "$TMP/play.yml" </dev/null >"$TMP/ansible.out" 2>&1; then
    tail -20 "$TMP/ansible.out"
    return
  fi
  sed -n 's/^STELLARINDEX_POSTGRES_DSN=//p' "$TMP/stellarindex.env"
}

COMMON='"region_deployment": "testnet", "vault_stellarindex_reader_secret_key": "x"'
HOSTILE_USER='si:ro@le/x'
HOSTILE_PASS='ab/cd#ef@gh?ij:kl%mn+op=qr/'
HOSTILE_DB='stellar/index'
dsn=$(render "{\"postgres_user_stellarindex\": \"$HOSTILE_USER\", \"postgres_pass_stellarindex\": \"$HOSTILE_PASS\", \"postgres_db_stellarindex\": \"$HOSTILE_DB\", $COMMON}")
verdict=$(python3 - "$dsn" "$HOSTILE_USER" "$HOSTILE_PASS" "$HOSTILE_DB" <<'PY'
import sys
from urllib.parse import urlsplit, unquote
dsn, user, pw, db = sys.argv[1:5]
try:
    u = urlsplit(dsn)
    got = (u.scheme, unquote(u.username or ""), unquote(u.password or ""),
           u.hostname, u.port, unquote(u.path), u.query, u.fragment)
except ValueError as e:
    print("unparseable: %s" % e); sys.exit(0)
want = ("postgres", user, pw, "127.0.0.1", 5432, "/" + db, "sslmode=disable", "")
print("round-trips" if got == want else "got %r want %r" % (got, want))
PY
)
if [ "$verdict" = "round-trips" ]; then
  ok "hostile user/password/db round-trip through the rendered DSN"
else
  bad "rendered DSN does not round-trip its credentials: $verdict"
fi

HEX_FIXTURE='0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0'
dsn=$(render "{\"postgres_pass_stellarindex\": \"$HEX_FIXTURE\", $COMMON}")
want="postgres://stellarindex:$HEX_FIXTURE@127.0.0.1:5432/stellarindex?sslmode=disable"
if [ "$dsn" = "$want" ]; then
  ok "hex password renders byte-identical to the raw form"
else
  bad "hex password DSN changed — got: $dsn"
fi

echo "ansible-postgres-dsn-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
