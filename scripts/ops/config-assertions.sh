#!/usr/bin/env bash
# config-assertions.sh — asserts that the load-bearing hand-applied /
# ansible-codified guard configs are actually LIVE on this host.
#
# Born from the 2026-07-03 finding that the 2026-06-11 incident's
# rsyslog suppression rules were codified in ansible but NEVER applied
# to r1 (the postmortem recorded codified-as-applied), and the reverse
# audit that found live-only fixes ansible would erase. Ansible does
# not auto-run against r1, so neither direction is self-healing —
# this check makes a gap in EITHER direction visible within an hour
# instead of at the next incident.
#
# Emits node_exporter textfile gauges:
#   stellarindex_config_assertion_ok{assertion="..."} 0|1
# Alert: stellarindex_config_assertion_failed (storage.yml, both trees).
#
# Adding an assertion: one `assert_*` line in main(). Assert the
# CONTENT that matters (grep), not just file existence — a truncated
# or reverted file should fail.
set -u

OUT="${TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}/config_assertions.prom"
TMP="$(mktemp)"
fails=0

emit() { # emit <assertion> <ok:0|1>
  echo "stellarindex_config_assertion_ok{assertion=\"$1\"} $2" >> "$TMP"
  [[ "$2" == "1" ]] || { fails=$((fails + 1)); echo "config-assertions: FAIL $1" >&2; }
}

assert_grep() { # assert_grep <assertion> <file> <pattern>
  if [[ -f "$2" ]] && grep -qE "$3" "$2"; then emit "$1" 1; else emit "$1" 0; fi
}

assert_cmd() { # assert_cmd <assertion> <command...>
  if "${@:2}" >/dev/null 2>&1; then emit "$1" 1; else emit "$1" 0; fi
}

{
  echo "# HELP stellarindex_config_assertion_ok 1 when a load-bearing guard config is live with expected content."
  echo "# TYPE stellarindex_config_assertion_ok gauge"
} > "$TMP"

# ── 2026-06-11 root-fill loop guards ─────────────────────────────────
assert_grep rsyslog_ch_suppress /etc/rsyslog.d/10-suppress-noisy-units.conf \
  'programname == "clickhouse-server" then stop'
assert_grep rsyslog_loki_suppress /etc/rsyslog.d/10-suppress-noisy-units.conf \
  'programname == "loki" then stop'
assert_grep journald_cap /etc/systemd/journald.conf.d/00-cap.conf \
  '^SystemMaxUse='
assert_grep ch_logs_on_zfs /etc/clickhouse-server/config.d/zzz-logpath.xml \
  '/var/lib/clickhouse/logs'
assert_grep syslog_maxsize /etc/logrotate.d/rsyslog \
  'maxsize'

# ── ZFS integrity (2026-07-03: an ansible apply downgrade-broke the
# userspace and deleted the dkms module — pool one reboot from gone) ──
assert_cmd zfs_userspace_works zpool status data
assert_cmd zfs_module_on_disk sh -c 'ls /lib/modules/$(uname -r)/updates/dkms/zfs.ko* >/dev/null'
assert_cmd zfs_packages_held sh -c 'apt-mark showhold | grep -q zfs-dkms'

# ── public edge stays open (a firewall re-render must keep Caddy) ────
assert_cmd nft_https_open sh -c 'nft list ruleset | grep -qE "dport \{? ?(80, 443|443)"'

# ── 2026-06-16 incident-sweep fixes ──────────────────────────────────
assert_grep redis_maxmemory /etc/redis/redis.conf '^maxmemory [0-9]'

# ── CS-010 supply config (erased if ansible renders without its vars) ─
assert_grep supply_reserve_accounts /etc/stellarindex.toml \
  'sdf_reserve_accounts = \['
assert_cmd supply_reserve_accounts_nonempty sh -c \
  'sed -n "/^sdf_reserve_accounts/,/^\]/p" /etc/stellarindex.toml | grep -cqE "G[A-Z0-9]{55}"'

# ── MinIO galexie-writer credential drift (BACKLOG #66, 2026-07-03
# rotation follow-up: docs/operations/credential-rotation.md) ────────
# There's no way to read a MinIO/S3 secret back and diff it against
# the vault — servers only ever accept/reject a signed request, they
# never expose a stored secret for comparison. So instead of an
# assert_grep content check, this probes whether the AWS_* creds
# currently templated into /etc/default/galexie still authenticate
# against the live MinIO server with ListBucket rights on
# galexie-live — exactly the capability galexie itself needs on
# every upload. A rotation that updated one side (the vault or the
# live MinIO user) but not the other fails this within the hour
# instead of surfacing as galexie's SignatureDoesNotMatch / upload
# stall (feedback_minio_cred_drift). Never prints the secret: mc
# reads it from the MC_HOST_* env var directly, nothing is written
# to a config file on disk or echoed, and assert_cmd itself already
# redirects all output to /dev/null.
assert_cmd galexie_writer_creds_valid sh -c '
  export HOME=/root
  [[ -r /etc/default/galexie ]] || exit 1
  set -a; . /etc/default/galexie; set +a
  [[ -n "${AWS_ACCESS_KEY_ID:-}" && -n "${AWS_SECRET_ACCESS_KEY:-}" && -n "${AWS_ENDPOINT_URL:-}" ]] || exit 1
  host="${AWS_ENDPOINT_URL#http://}"; host="${host#https://}"
  export MC_HOST_cfgassert="http://${AWS_ACCESS_KEY_ID}:${AWS_SECRET_ACCESS_KEY}@${host}"
  /usr/local/bin/mc ls cfgassert/galexie-live >/dev/null 2>&1
'

mv "$TMP" "$OUT"
chmod 644 "$OUT"
echo "config-assertions: $fails failure(s)" >&2
exit "$fails"
