#!/usr/bin/env bash
# ansible-api-env-test.sh — the public API's own env file
# (stellarindex-api.env.j2) must give the API what the shared env file
# (stellarindex.env.j2) gives it, and the ClickHouse serving pair it renders
# must be one the client accepts.
#
# The API loads ONLY /etc/default/stellarindex-api and cannot see the shared
# file, so a guard fixed in one template and not the other reaches the
# daemons but not the API. Rendered for real (ansible-playbook -c local) over
# a matrix of the serving flip and the live-daemon identity:
#   1. every name the shared template renders is rendered by the API template
#      or listed in NOT_NEEDED below with the reason the API never reads it;
#   2. every name both render has the same value in both;
#   3. a serving password in the API env implies a non-empty
#      storage.clickhouse_serving_user in the rendered stellarindex.toml
#      (internal/storage/clickhouse/ops_auth.go refuses a password with no
#      user, and every lake-backed route then 503s behind a 200 readyz), and
#      with the flip on both halves reach the API.
# And, statically: docs/operations/self-hosting.md creates the stellarindex-api
# user and its /etc/default/stellarindex-api heredoc names every variable the
# role renders for the API.
#
# Needs ansible-playbook (the ci.yml ansible-check job has it).
# Run: bash scripts/ci/ansible-api-env-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ANSIBLE_DIR="${ANSIBLE_DIR:-$PWD/configs/ansible}"   # override for the red-proof against a pre-fix copy
ROLE="$ANSIBLE_DIR/roles/archival-node"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# name:reason — shared-env names the API binary never reads.
NOT_NEEDED="
AWS_ACCESS_KEY_ID:lake S3 credentials; the API reads ClickHouse, never MinIO
AWS_SECRET_ACCESS_KEY:lake S3 credentials; the API reads ClickHouse, never MinIO
AWS_ENDPOINT_URL:lake S3 endpoint; the API reads ClickHouse, never MinIO
AWS_REGION:lake S3 region; the API reads ClickHouse, never MinIO
STELLARINDEX_S3_ACCESS_KEY:lake S3 credentials for the indexer
STELLARINDEX_S3_SECRET_KEY:lake S3 credentials for the indexer
COINGECKO_API_KEY:price connector key; only the aggregator polls CoinGecko
COINGECKO_DEMO_API_KEY:price connector key; only the aggregator polls CoinGecko
TIINGO_API_KEY:price connector key; only the aggregator polls Tiingo
EXCHANGERATESAPI_KEY:FX connector key; only the aggregator polls exchangeratesapi
"

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

if ! command -v ansible-playbook >/dev/null; then
  echo "ansible-api-env-test: FAIL — ansible-playbook not on PATH (this test must not pass vacuously)" >&2
  exit 1
fi

names_of() { sed -nE 's/^([A-Za-z_][A-Za-z0-9_]*)=.*/\1/p' "$1" | sort -u; }

# ── 1. every shared name is rendered for the API or declared not needed ──
shared_names=$(names_of "$ROLE/templates/stellarindex.env.j2")
api_names=" $(names_of "$ROLE/templates/stellarindex-api.env.j2" | tr '\n' ' ') "
if [ -z "$shared_names" ] || [ "$api_names" = "  " ]; then
  bad "no variable found in the env templates; the scan is broken"
fi
for n in $shared_names; do
  listed=$(printf '%s\n' "$NOT_NEEDED" | grep -c "^$n:")
  case "$api_names" in
    *" $n "*)
      [ "$listed" -eq 0 ] || bad "$n is rendered for the API but listed NOT_NEEDED in $0" ;;
    *)
      if [ "$listed" -eq 0 ]; then
        bad "$n is rendered for the daemons but not for the API — add it to stellarindex-api.env.j2 or list it in NOT_NEEDED with a reason"
      fi ;;
  esac
done
shared_set=" $(printf '%s' "$shared_names" | tr '\n' ' ') "
for entry in $(printf '%s\n' "$NOT_NEEDED" | sed -n 's/:.*//p'); do
  case "$shared_set" in *" $entry "*) ;; *)
    bad "NOT_NEEDED lists $entry, which stellarindex.env.j2 no longer renders" ;;
  esac
done
[ "$fail" -eq 0 ] && ok "every name in stellarindex.env.j2 is rendered for the API or declared not needed"

# ── 1b. the self-host guide writes the same API env file the role does ───
GUIDE="${GUIDE:-docs/operations/self-hosting.md}"
guide_env=$(sed -n '/tee \/etc\/default\/stellarindex-api /,/^EOT$/p' "$GUIDE" 2>/dev/null)
guide_names=" $(printf '%s\n' "$guide_env" | sed -nE 's/^([A-Za-z_][A-Za-z0-9_]*)=.*/\1/p' | tr '\n' ' ') "
before=$fail
if [ -z "$guide_env" ]; then
  bad "$GUIDE has no heredoc writing /etc/default/stellarindex-api; the scan is broken"
else
  for n in $api_names; do
    case "$guide_names" in *" $n "*) ;; *) bad "$GUIDE's /etc/default/stellarindex-api omits $n, which the role renders for the API" ;; esac
  done
fi
# The command may continue onto a second line; the user name ends it.
if ! grep -qE '^(sudo )?useradd .* stellarindex-api$|^[[:space:]]+--shell [^ ]+ stellarindex-api$' "$GUIDE"; then
  bad "$GUIDE never creates the stellarindex-api user the API unit runs as"
fi
[ "$fail" -eq "$before" ] && ok "$GUIDE creates the API user and writes every name the role renders for the API"

# ── 2/3. real render over the matrix ──────────────────────────────────────
cat > "$TMP/play.yml" <<YML
- hosts: localhost
  gather_facts: false
  vars_files:
    - $ROLE/defaults/main.yml
    - $TMP/vars.json
  tasks:
    - ansible.builtin.template: {src: $ROLE/templates/stellarindex.env.j2, dest: $TMP/shared.env, mode: "0600"}
    - ansible.builtin.template: {src: $ROLE/templates/stellarindex-api.env.j2, dest: $TMP/api.env, mode: "0600"}
    - ansible.builtin.template: {src: $ROLE/templates/stellarindex.toml.j2, dest: $TMP/stellarindex.toml, mode: "0600"}
YML

# Fixture values only; none is a real credential.
COMMON='"region_deployment": "testnet", "postgres_pass_stellarindex": "fixture-pg", "redis_password": "fixture-redis", "vault_stellarindex_reader_secret_key": "x", "vault_dashboard_code_secret": "x", "vault_webhook_seal_key": "x", "vault_clickhouse_serving_password": "fixture-serving"' # gitleaks:allow

value_of() { sed -n "s/^$1=//p" "$2"; }

check_case() { # check_case <label> <serving_enabled> <live_enabled>
  local label=$1 vars
  vars="{$COMMON, \"stellarindex_clickhouse_serving_enabled\": $2, \"clickhouse_live_daemon_profile_enabled\": $3, \"clickhouse_live_daemon_password\": \"fixture-live\"}" # gitleaks:allow
  printf '%s' "$vars" > "$TMP/vars.json"
  rm -f "$TMP/shared.env" "$TMP/api.env" "$TMP/stellarindex.toml"
  if ! ANSIBLE_LOCALHOST_WARNING=false ANSIBLE_INVENTORY_UNPARSED_WARNING=false \
       ANSIBLE_PYTHON_INTERPRETER=auto_silent \
       ansible-playbook -c local -i localhost, "$TMP/play.yml" </dev/null >"$TMP/ansible.out" 2>&1; then
    bad "$label: render failed"
    tail -20 "$TMP/ansible.out"
    return
  fi
  local n drift=0 sv su
  for n in $(names_of "$TMP/api.env"); do
    grep -q "^$n=" "$TMP/shared.env" || continue
    if [ "$(value_of "$n" "$TMP/api.env")" != "$(value_of "$n" "$TMP/shared.env")" ]; then
      bad "$label: $n renders differently for the API than in stellarindex.env.j2"
      drift=1
    fi
  done
  [ "$drift" -eq 0 ] && ok "$label: every shared name renders the same value for the API"

  sv=$(value_of STELLARINDEX_CLICKHOUSE_SERVING_PASSWORD "$TMP/api.env")
  su=$(sed -nE 's/^clickhouse_serving_user[[:space:]]*=[[:space:]]*"(.*)"$/\1/p' "$TMP/stellarindex.toml")
  if [ -n "$sv" ] && [ -z "$su" ]; then
    bad "$label: the API env carries the serving password but stellarindex.toml's clickhouse_serving_user is empty"
  elif [ "$2" = true ] && { [ -z "$sv" ] || [ -z "$su" ]; }; then
    bad "$label: serving flip on but the API gets user='$su' and a password of ${#sv} chars"
  else
    ok "$label: the API's serving pair is set together or not at all (user='$su')"
  fi

  # With the flip on the API reads as api_serving; the write-capable
  # live_daemon / ops_batch pairs must not sit in its environment.
  if [ "$2" = true ] && grep -qE '^STELLARINDEX_CLICKHOUSE_(LIVE|OPS)_' "$TMP/api.env"; then
    bad "$label: serving flip on but the API env carries a write-capable ClickHouse identity: $(grep -oE '^STELLARINDEX_CLICKHOUSE_(LIVE|OPS)_[A-Z]+' "$TMP/api.env" | tr '\n' ' ')"
  elif [ "$2" = true ]; then
    ok "$label: no live_daemon or ops_batch ClickHouse identity in the API env"
  fi
}

check_case "serving off, live identity off" false false
check_case "serving off, live identity on" false true
check_case "serving on, live identity on" true true

echo "ansible-api-env-test: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
