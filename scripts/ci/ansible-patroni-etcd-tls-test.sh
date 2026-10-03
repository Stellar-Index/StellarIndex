#!/usr/bin/env bash
# ansible-patroni-etcd-tls-test.sh — the patroni role must never render or
# configure a plaintext etcd by default.
#
# etcd holds the Patroni leader lock; a plaintext, unauthenticated client or
# peer port lets any host on the private range rewrite cluster state.
#
# What must hold (defaults, 3-host postgres_cluster):
#   1. etcd.conf.j2 has no http:// URL; every *_URLS / INITIAL_CLUSTER entry
#      is https; mutual cert auth is on for both the client and peer port, with
#      CERT/KEY/TRUSTED_CA set for each;
#   2. patroni.yml.j2's etcd3 block is protocol https with cacert, cert, key;
#   3. the TLS-material assert tasks in 03-etcd-configure.yml and
#      06-patroni-configure.yml fail without the PEM vars (and pass with them);
#   4. only etcd_tls_enabled=false renders http.
#
# Needs ansible-playbook and python3 with PyYAML (the ansible job has both).
# Run: bash scripts/ci/ansible-patroni-etcd-tls-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ANSIBLE_DIR="${ANSIBLE_DIR:-$PWD/configs/ansible}"   # override for the red-proof against a pre-fix copy
ROLE="$ANSIBLE_DIR/roles/patroni"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

for tool in ansible-playbook python3; do
  if ! command -v "$tool" >/dev/null; then
    echo "ansible-patroni-etcd-tls-test: FAIL — $tool not on PATH (this test must not pass vacuously)" >&2
    exit 1
  fi
done

cat > "$TMP/inventory.yml" <<'YML'
all:
  children:
    postgres_cluster:
      hosts:
        pg1: { ansible_host: 10.0.0.1, ansible_connection: local }
        pg2: { ansible_host: 10.0.0.2, ansible_connection: local }
        pg3: { ansible_host: 10.0.0.3, ansible_connection: local }
YML

cat > "$TMP/render.yml" <<YML
- hosts: pg1
  gather_facts: false
  vars_files:
    - $ROLE/defaults/main.yml
  vars:
    patroni_rest_basic_auth_user: u
    patroni_rest_basic_auth_password: p
    patroni_postgres_password: p
    patroni_replicator_password: p
    patroni_rewind_password: p
  tasks:
    - ansible.builtin.template: { src: $ROLE/templates/etcd.conf.j2, dest: "{{ out_dir }}/etcd.conf" }
    - ansible.builtin.template: { src: $ROLE/templates/patroni.yml.j2, dest: "{{ out_dir }}/patroni.yml" }
YML

run_ansible() { # run_ansible <playbook> [extra ansible-playbook args...]
  local pb="$1"; shift
  ANSIBLE_LOCALHOST_WARNING=false ANSIBLE_INVENTORY_UNPARSED_WARNING=false \
    ANSIBLE_PYTHON_INTERPRETER=auto_silent \
    ansible-playbook -i "$TMP/inventory.yml" "$pb" "$@" </dev/null >"$TMP/ansible.out" 2>&1
}

render() { # render <dir> [extra-vars-json]
  mkdir -p "$1"
  run_ansible "$TMP/render.yml" -e "out_dir=$1" -e "${2:-{\}}" || { tail -20 "$TMP/ansible.out"; return 1; }
}

# ── 1/2. default render ──────────────────────────────────────────────────
if ! render "$TMP/tls"; then
  bad "default render failed"
else
  etcd="$TMP/tls/etcd.conf"; pat="$TMP/tls/patroni.yml"
  if grep -q 'http://' "$etcd"; then bad "etcd.conf renders an http:// URL by default"; else ok "etcd.conf has no http:// URL"; fi

  urls=$(grep -E '^ETCD_(LISTEN_PEER_URLS|LISTEN_CLIENT_URLS|INITIAL_ADVERTISE_PEER_URLS|ADVERTISE_CLIENT_URLS|INITIAL_CLUSTER)=' "$etcd" \
    | sed -E 's/^[^=]*="//; s/"$//' | tr ',' '\n' | sed -E 's/^[^=]*=//')
  n=$(printf '%s\n' "$urls" | grep -c .)
  nonhttps=$(printf '%s\n' "$urls" | grep -cv '^https://')
  if [ "$n" -eq 8 ] && [ "$nonhttps" -eq 0 ]; then
    ok "all $n etcd URL entries are https"
  else
    bad "etcd URL entries not all https ($n entries)"; printf '%s\n' "$urls"
  fi

  for want in \
    'ETCD_CLIENT_CERT_AUTH="true"' 'ETCD_PEER_CLIENT_CERT_AUTH="true"' \
    'ETCD_CERT_FILE="/etc/etcd/server.pem"' 'ETCD_KEY_FILE="/etc/etcd/server-key.pem"' \
    'ETCD_TRUSTED_CA_FILE="/etc/etcd/ca.pem"' \
    'ETCD_PEER_CERT_FILE="/etc/etcd/server.pem"' 'ETCD_PEER_KEY_FILE="/etc/etcd/server-key.pem"' \
    'ETCD_PEER_TRUSTED_CA_FILE="/etc/etcd/ca.pem"'; do
    if grep -qxF "$want" "$etcd"; then ok "etcd.conf has $want"; else bad "etcd.conf missing $want"; fi
  done

  verdict=$(python3 - "$pat" <<'PY'
import sys, yaml
e = yaml.safe_load(open(sys.argv[1]))["etcd3"]
want = {"protocol": "https", "cacert": "/etc/patroni/etcd-tls/ca.pem",
        "cert": "/etc/patroni/etcd-tls/client.pem", "key": "/etc/patroni/etcd-tls/client-key.pem"}
bad = {k: e.get(k) for k, v in want.items() if e.get(k) != v}
print("ok" if not bad else "got %r" % bad)
PY
)
  if [ "$verdict" = "ok" ]; then ok "patroni.yml etcd3 is https with cacert/cert/key"; else bad "patroni.yml etcd3 TLS block: $verdict"; fi
fi

# ── 4. the plaintext render is opt-in only ───────────────────────────────
if render "$TMP/plain" '{"etcd_tls_enabled": false}'; then
  if grep -q 'http://' "$TMP/plain/etcd.conf" && ! grep -q 'https://' "$TMP/plain/etcd.conf" \
     && ! grep -q 'protocol: https' "$TMP/plain/patroni.yml"; then
    ok "etcd_tls_enabled=false is the only path to http"
  else
    bad "etcd_tls_enabled=false render is not plaintext"
  fi
else
  bad "etcd_tls_enabled=false render failed"
fi

# ── 3. the assert tasks refuse missing PEM material ──────────────────────
# Lift the assert task out of each task file and run it alone.
assert_play() { # assert_play <task-file> <name-substring> <vars-json> -> play path
  python3 - "$1" "$2" "$3" "$TMP/assert.yml" "$ROLE/defaults/main.yml" <<'PY'
import sys, json, yaml
tasks = yaml.safe_load(open(sys.argv[1]))
t = [x for x in tasks if "assert" in x and sys.argv[2] in x["name"]]
if len(t) != 1:
    sys.exit("no unique assert task matching %r" % sys.argv[2])
play = [{"hosts": "pg1", "gather_facts": False, "vars_files": [sys.argv[5]], "vars": json.loads(sys.argv[3]), "tasks": t}]
yaml.safe_dump(play, open(sys.argv[4], "w"))
PY
}

check_assert() { # check_assert <task-file> <name-substring> <label> <full-vars-json>
  if ! assert_play "$1" "$2" '{}'; then bad "$3: assert task not found"; return; fi
  if run_ansible "$TMP/assert.yml"; then
    bad "$3: assert passes without PEM material"
  else
    ok "$3: assert fails without PEM material"
  fi
  assert_play "$1" "$2" "$4"
  if run_ansible "$TMP/assert.yml"; then
    ok "$3: assert passes with PEM material"
  else
    bad "$3: assert fails even with PEM material"; tail -10 "$TMP/ansible.out"
  fi
}

check_assert "$ROLE/tasks/03-etcd-configure.yml" "etcd TLS material" "03-etcd-configure" \
  '{"etcd_tls_ca_pem": "x", "etcd_tls_cert_pem": "x", "etcd_tls_key_pem": "x"}'
check_assert "$ROLE/tasks/06-patroni-configure.yml" "etcd client TLS material" "06-patroni-configure" \
  '{"etcd_tls_ca_pem": "x", "patroni_etcd_client_cert_pem": "x", "patroni_etcd_client_key_pem": "x"}'

echo "ansible-patroni-etcd-tls-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
