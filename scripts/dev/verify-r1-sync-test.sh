#!/usr/bin/env bash
# verify-r1-sync-test.sh — pins which r1 paths verify-r1-sync.sh counts as
# drift. Host-rendered files (Caddyfile, alertmanager.yml) never byte-match
# the repo and must not fail a host that is in sync; verbatim copies such as
# prometheus.r1.yml must. ssh is stubbed over a fake host root: no network.
#
# Run: bash scripts/dev/verify-r1-sync-test.sh
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"
host="$tmp/host"

cat >"$tmp/bin/ssh" <<'STUB'
#!/usr/bin/env bash
cmd="${*: -1}"
case "$cmd" in
  *schema_migrations*) printf '%s\n' "$FAKE_MIG" ;;
  *"md5sum '"*)
    p="${cmd#*md5sum \'}"
    p="${p%%\'*}"
    [ -f "$FAKE_ROOT$p" ] && { md5sum "$FAKE_ROOT$p" 2>/dev/null || md5 -r "$FAKE_ROOT$p"; } | awk '{print $1}'
    ;;
esac
exit 0
STUB
chmod +x "$tmp/bin/ssh"

# place <repo-path> <host-path>: install the repo file verbatim on the fake host.
place() { mkdir -p "$host$(dirname "$2")" && cp "$1" "$host$2"; }

mkhost() {
  rm -rf "$host"
  place configs/prometheus/prometheus.r1.yml /etc/prometheus/prometheus.yml
  place scripts/dev/r1-smoke.sh /opt/stellarindex/healthchecks/r1-smoke.sh
  place configs/healthchecks/smoke.sh /opt/stellarindex/healthchecks/smoke.sh
  place configs/healthchecks/heartbeat.sh /opt/stellarindex/healthchecks/heartbeat.sh
  place configs/healthchecks/sla-probe.sh /opt/stellarindex/healthchecks/sla-probe.sh
  for f in configs/prometheus/rules.r1/*.yml; do
    place "$f" "/etc/prometheus/rules.r1/$(basename "$f")"
  done
  # What the renderers produce: never the repo file's bytes.
  mkdir -p "$host/etc/caddy"
  printf 'rendered by ansible\n' >"$host/etc/caddy/Caddyfile"
  printf 'rendered by apply.sh\n' >"$host/etc/prometheus/alertmanager.yml"
}

mig="$(find migrations -maxdepth 1 -name '[0-9]*_*.up.sql' | sed -E 's|migrations/0*([0-9]+)_.*|\1|' | sort -n | tail -1)"

run() {
  OUT="$(PATH="$tmp/bin:$PATH" FAKE_ROOT="$host" FAKE_MIG="$mig" bash scripts/dev/verify-r1-sync.sh 2>&1)"
  RC=$?
}

fail=0
expect() {
  local label="$1" want_rc="$2" want="$3"
  if [ "$RC" -eq "$want_rc" ] && grep -qF -- "$want" <<<"$OUT"; then
    echo "OK: $label"
  else
    echo "FAIL: $label — rc=$RC (want $want_rc), want a line containing '$want' in:" >&2
    printf '%s\n' "$OUT" >&2
    fail=1
  fi
}

mkhost
run
expect "an in-sync host with rendered Caddyfile/alertmanager.yml passes" 0 "OK: all tracked files + migrations in sync"
expect "a rendered file is reported, not counted" 0 "INFO: /etc/caddy/Caddyfile is rendered from configs/caddy/Caddyfile.api"

mkhost
printf 'hand edit\n' >>"$host/etc/prometheus/prometheus.yml"
run
expect "a drifted prometheus.yml still fails" 1 "DRIFT: configs/prometheus/prometheus.r1.yml"

mkhost
rm "$host/opt/stellarindex/healthchecks/smoke.sh"
run
expect "a missing verbatim copy fails" 1 "DRIFT: configs/healthchecks/smoke.sh"

exit "$fail"
