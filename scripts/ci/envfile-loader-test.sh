#!/usr/bin/env bash
# envfile-loader-test.sh — systemd EnvironmentFiles are read VERBATIM by
# every shell consumer, never `.`/source'd (deploy-ansible-secrets-5).
#
# /etc/default/stellarindex, /etc/default/stellarindex-ops and
# /etc/default/galexie are rendered UNQUOTED (systemd EnvironmentFile
# syntax: the line is taken literally). The deploy migrate step, a root
# cron and a dozen host scripts used to `set -a; . <file>` — which is the
# SHELL parser: `$k9` expands to empty, `;abc` becomes a command run as
# root, quotes vanish. The services (systemd) keep working while every
# sourcing path silently gets a different secret. The repo already knew
# (run-heavy-job.sh reads its pair verbatim "because the password may
# contain shell metacharacters"); this pins that to ALL consumers.
#
# What must hold:
#   1. no consumer sources /etc/default/{stellarindex,stellarindex-ops,
#      galexie} (comment lines excluded);
#   2. every bash consumer carries the ONE canonical load_env_file
#      (lockstep — a drifted copy is a fresh bug);
#   3. the canonical loader, extracted from a shipped script, round-trips
#      a fixture whose values carry `$`, `;`, quotes, spaces, `#`, `=`
#      and `\` byte-for-byte, in both export and non-export mode, and
#      skips comments/blank lines;
#   4. the POSIX one-liners (deploy-binary.yml migrate task under /bin/sh,
#      the tier-D cron) do the same under `sh`.
#
# Run: bash scripts/ci/envfile-loader-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
ok()  { printf '  ok   %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL %s\n' "$1"; fail=$((fail + 1)); }
expect() {  # expect GOT WANT LABEL
  if [[ "$1" == "$2" ]]; then ok "$3"; else bad "$3 — got: $1"; fi
}

FILES_DIR="${FILES_DIR:-configs/ansible/roles/archival-node/files}"
OPS_DIR="${OPS_DIR:-scripts/ops}"
TASKS="${TASKS:-configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml}"
PLAYBOOK="${PLAYBOOK:-configs/ansible/playbooks/deploy-binary.yml}"

BASH_CONSUMERS=(
  "$FILES_DIR/compute-archive-to.sh"
  "$FILES_DIR/compute-trim-cutoff.sh"
  "$FILES_DIR/data-freshness.sh"
  "$FILES_DIR/run-ch-supply.sh"
  "$FILES_DIR/run-compute-completeness.sh"
  "$OPS_DIR/ch-live-catchup.sh"
  "$OPS_DIR/ch-rebuild-projected.sh"
  "$OPS_DIR/ch-supply-flows-seed.sh"
  "$OPS_DIR/completeness-incremental.sh"
  "$OPS_DIR/phaseD-backfill.sh"
  "$OPS_DIR/phaseD-range.sh"
)
ALL_CONSUMERS=("${BASH_CONSUMERS[@]}" "$OPS_DIR/config-assertions.sh" "$TASKS" "$PLAYBOOK")

# ─── 1. nobody sources an EnvironmentFile ─────────────────────────────
for f in "${ALL_CONSUMERS[@]}"; do
  if [[ ! -r "$f" ]]; then bad "missing consumer $f"; continue; fi
  if grep -vE '^\s*#' "$f" | grep -qE '(^|[;[:space:]])(\.|source)[[:space:]]+/etc/default/(stellarindex|stellarindex-ops|galexie)([[:space:]]|;|$)'; then
    bad "$f sources a systemd EnvironmentFile with the shell parser (\`.\`/source) — a secret with \$ ; quotes or spaces is mangled or executed; read it verbatim"
  else
    ok "$f does not source /etc/default/{stellarindex,-ops,galexie}"
  fi
done

# ─── 2. one canonical loader, in lockstep ────────────────────────────
extract_loader() {  # prints the load_env_file() {...} block of $1
  awk '/^load_env_file\(\) \{/{p=1} p{print} p&&/^\}/{exit}' "$1"
}
canon="$(extract_loader "${BASH_CONSUMERS[0]}")"
if [[ -z "$canon" ]]; then
  bad "${BASH_CONSUMERS[0]} has no load_env_file() — nothing to pin"
else
  for f in "${BASH_CONSUMERS[@]}"; do
    this="$(extract_loader "$f")"
    if [[ -z "$this" ]]; then
      bad "$f has no load_env_file()"
    elif [[ "$this" != "$canon" ]]; then
      bad "$f load_env_file() drifted from ${BASH_CONSUMERS[0]}"
    else
      ok "$f carries the canonical load_env_file()"
    fi
    # shellcheck disable=SC2016  # literal "$f" is the pattern being searched for
    if ! grep -qE '^\s*load_env_file /etc/default/(stellarindex|stellarindex-ops)( export)?$|load_env_file "\$f" export' "$f"; then
      bad "$f defines load_env_file but never calls it on its env file"
    fi
  done
fi

# ─── 3. the loader round-trips metacharacters verbatim ────────────────
cat > "$TMP/env" <<'ENV'
# Rendered by Ansible. Do not edit by hand.
STELLARINDEX_POSTGRES_DSN=postgres://stellarindex:Qx7$k9;abc@127.0.0.1:5432/stellarindex?sslmode=disable

AWS_SECRET_ACCESS_KEY=sp ace'quo"te\back#hash=eq
EMPTY_KEY=
ENV
# shellcheck disable=SC2016  # the literal $k9 is the point
WANT_DSN='postgres://stellarindex:Qx7$k9;abc@127.0.0.1:5432/stellarindex?sslmode=disable'
WANT_SEC='sp ace'"'"'quo"te\back#hash=eq'

if [[ -n "$canon" ]]; then
  printf '%s\n' "$canon" > "$TMP/loader.sh"
  # non-export mode: value is set in the shell, NOT exported to children
  got="$(bash -c '. "$1"; load_env_file "$2"; printf "%s\n%s\n%s\n" "$STELLARINDEX_POSTGRES_DSN" "$AWS_SECRET_ACCESS_KEY" "$(env | grep -c "^STELLARINDEX_POSTGRES_DSN=")"' _ "$TMP/loader.sh" "$TMP/env")"
  expect "$(sed -n 1p <<<"$got")" "$WANT_DSN" "loader: DSN with \$ ; round-trips verbatim (non-export)"
  expect "$(sed -n 2p <<<"$got")" "$WANT_SEC" "loader: space/quotes/backslash/#/= round-trip verbatim (non-export)"
  expect "$(sed -n 3p <<<"$got")" "0" "loader: non-export mode does not export (preserves plain \`.\` semantics)"
  # export mode: children see it
  got="$(bash -c '. "$1"; load_env_file "$2" export; env | grep "^STELLARINDEX_POSTGRES_DSN="; env | grep "^AWS_SECRET_ACCESS_KEY="; env | grep -c "^EMPTY_KEY=$"' _ "$TMP/loader.sh" "$TMP/env")"
  expect "$(sed -n 1p <<<"$got")" "STELLARINDEX_POSTGRES_DSN=$WANT_DSN" "loader: export mode hands the verbatim DSN to children"
  expect "$(sed -n 2p <<<"$got")" "AWS_SECRET_ACCESS_KEY=$WANT_SEC" "loader: export mode hands the verbatim secret to children"
  expect "$(sed -n 3p <<<"$got")" "1" "loader: empty value exported as empty"
  # the class is real: the shell parser gives a DIFFERENT value
  sourced="$(bash -c 'set -a; . "$1" 2>/dev/null; set +a; printf "%s" "$STELLARINDEX_POSTGRES_DSN"' _ "$TMP/env")"
  if [[ "$sourced" != "$WANT_DSN" ]]; then
    ok "control: \`.\`-sourcing the same file yields a different DSN ($sourced)"
  else
    bad "control: sourcing unexpectedly preserved the DSN — fixture no longer exercises the class"
  fi
fi

# ─── 4. the POSIX one-liners under sh ─────────────────────────────────
posix_check() {  # $1=label $2=loop-text (must read from "$3" after substitution)
  local label="$1" loop="$2"
  got="$(sh -c "$loop"'; printf "%s\n" "$STELLARINDEX_POSTGRES_DSN"; env | grep -c "^AWS_SECRET_ACCESS_KEY=sp ace"' 2>&1)"
  if [[ "$(sed -n 1p <<<"$got")" == "$WANT_DSN" && "$(sed -n 2p <<<"$got")" == "1" ]]; then
    ok "$label: POSIX loop reads + exports verbatim under sh"
  else
    bad "$label: POSIX loop wrong under sh: $got"
  fi
}
# deploy-binary.yml migrate task
loop="$(python3 - "$PLAYBOOK" <<'PY'
import sys, yaml
plays = yaml.safe_load(open(sys.argv[1]))
for play in plays:
    for t in play.get("pre_tasks") or []:
        if t.get("name") == "Apply outstanding migrations":
            cmd = t["ansible.builtin.shell"]["cmd"]
            # take the non-migrate_dsn branch between {% else %} and {% endif %}
            body = cmd.split("{% else %}")[1].split("{% endif %}")[0]
            print(body.strip())
            sys.exit(0)
sys.exit(1)
PY
)"
if [[ -z "$loop" ]]; then
  bad "deploy-binary.yml: could not extract the migrate task's env reader"
elif grep -qE '(^|[;[:space:]])\.[[:space:]]+/etc/default/stellarindex' <<<"$loop"; then
  bad "deploy-binary.yml migrate task still \`.\`-sources /etc/default/stellarindex under /bin/sh as root"
else
  posix_check "deploy-binary.yml migrate task" "${loop//\/etc\/default\/stellarindex/$TMP/env}"
fi
# tier-D cron one-liner
cron="$(python3 - "$TASKS" <<'PY'
import sys, yaml
for t in yaml.safe_load(open(sys.argv[1])):
    c = t.get("ansible.builtin.cron") or {}
    if c.get("name") == "stellarindex-verify-archive-tier-d":
        print(c["job"]); sys.exit(0)
sys.exit(1)
PY
)"
if [[ -z "$cron" ]]; then
  bad "14-stellarindex-services.yml: tier-D cron not found"
elif grep -qE 'source /etc/default/stellarindex-ops' <<<"$cron"; then
  bad "tier-D root cron still \`source\`s /etc/default/stellarindex-ops"
elif grep -q '%' <<<"$cron"; then
  bad "tier-D cron job contains '%' — cron turns it into a newline"
else
  inner="${cron#bash -c \'}"; inner="${inner%; /usr/local/bin/stellarindex-ops verify-archive*}"
  inner="${inner//\/etc\/default\/stellarindex-ops/$TMP/env}"
  posix_check "tier-D cron" "$inner"
fi

echo "envfile-loader-test: $pass passed, $fail failed across ${#ALL_CONSUMERS[@]} consumers"
[[ $fail -eq 0 ]]
