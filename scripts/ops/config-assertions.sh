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

# ── host-shape applicability ─────────────────────────────────────────
# Some assertions guard a layer a given host does not HAVE. A test-net
# libvirt VM (and r2/r3 on EBS/Object-Storage) runs with
# zfs_data_pool_type="" and no pool at all — asserting ZFS integrity
# there reports FAIL forever for a layer that is absent by design, which
# trains the operator to ignore a page that exists to catch a pool one
# reboot from gone (2026-07-03).
#
# `skip` emits an EXPLICIT _skipped series rather than omitting the
# assertion silently, so "deliberately not applicable on this host" stays
# distinguishable from "this check vanished in a refactor". The alert is
# `stellarindex_config_assertion_ok == 0`, so a skipped assertion cannot
# fire it, and the staleness alert reads the file mtime, which still
# updates.
skip() { # skip <assertion>
  echo "stellarindex_config_assertion_skipped{assertion=\"$1\"} 1" >> "$TMP"
  echo "config-assertions: SKIP $1 (layer not present on this host)" >&2
}

# True only where a real ZFS data pool is live. Deliberately checks the
# POOL, not just the binary: a host with zfsutils installed but no pool
# is still a no-ZFS host for these assertions' purposes.
have_zfs() { command -v zpool >/dev/null 2>&1 && zpool list data >/dev/null 2>&1; }

# The Stellar network this host indexes, read from the same config the
# binaries read. Empty if unreadable — callers must treat empty as
# "assume pubnet" so an unreadable config can never SILENCE a check on
# r1 (fail-closed: unknown host shape keeps the strict assertions).
stellar_network() {
  sed -nE 's/^[[:space:]]*network[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p' \
    /etc/stellarindex.toml 2>/dev/null | head -1
}
is_pubnet() { [[ "$(stellar_network)" != "testnet" && "$(stellar_network)" != "futurenet" ]]; }

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
if have_zfs; then
  assert_cmd zfs_userspace_works zpool status data
  # Requires the unit to NOT set ProtectKernelModules (see the comment in
  # deploy/systemd/config-assertions.service): that flag makes
  # /lib/modules inaccessible, and every formulation of this check — ls,
  # modinfo, dkms status — reads that tree, so the assertion reported FAIL
  # on every service run while passing by hand. Checking the module is
  # merely LOADED would not substitute: a deleted module stays resident
  # until reboot, which is exactly the 2026-07-03 trap.
  assert_cmd zfs_module_on_disk sh -c 'ls /lib/modules/$(uname -r)/updates/dkms/zfs.ko* >/dev/null'
  assert_cmd zfs_packages_held sh -c 'apt-mark showhold | grep -q zfs-dkms'
else
  skip zfs_userspace_works
  skip zfs_module_on_disk
  skip zfs_packages_held
fi

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
# bash -c, not sh -c: the body uses [[ ]], which dash rejects with
# "[[: not found" (exit 127) — so this assertion reported FAIL on every
# run regardless of the creds' actual validity. Same bashism-under-dash
# class as the deploy backup-freshness gate (7609dce4).
assert_cmd galexie_writer_creds_valid bash -c '
  export HOME=/root
  [[ -r /etc/default/galexie ]] || exit 1
  # Read verbatim — systemd EnvironmentFile syntax is not shell; a secret
  # with `$`/`;`/quotes would be mangled by `.` (deploy-ansible-secrets-5).
  while IFS= read -r line || [[ -n "$line" ]]; do
    case "$line" in [A-Za-z_]*=*) export "$line" ;; esac
  done < /etc/default/galexie
  [[ -n "${AWS_ACCESS_KEY_ID:-}" && -n "${AWS_SECRET_ACCESS_KEY:-}" && -n "${AWS_ENDPOINT_URL:-}" ]] || exit 1
  host="${AWS_ENDPOINT_URL#http://}"; host="${host#https://}"
  export MC_HOST_cfgassert="http://${AWS_ACCESS_KEY_ID}:${AWS_SECRET_ACCESS_KEY}@${host}"
  /usr/local/bin/mc ls cfgassert/galexie-live >/dev/null 2>&1
'

# ── Compression-policy backfill status (DAT-03, audit-2026-07-23) ────
# scripts/ops/add-missing-compression-policies.sql is a deliberate
# NON-migration operator script (must run AFTER the Phase D backfill —
# see its own header for why) that shipped with no execution tracking:
# nothing noticed if an operator forgot to run it, or if a table was
# added to its eligible-tables list without a matching policy actually
# landing. The want-list MUST track that script's list exactly: migration
# 0152 (#358) dropped aggregator_exposures / classic_asset_stats_5m /
# tvl_observations as never-wired scaffolds, and a dropped table has no
# policy job, so leaving it here would fail this assertion on r1 forever
# for a table that is gone on purpose.
# Reuses the SAME stellarindex_config_assertion_ok gauge +
# the existing stellarindex_config_assertion_failed alert every other
# assertion here uses — no new alert rule needed. Expected to page
# (severity: ticket, not page) until the operator runs that script
# post-Phase-D; that visibility IS the fix — today the gap is silent.
# shellcheck disable=SC2329  # invoked indirectly via assert_cmd's "${@:2}"
compression_policies_applied() {
  [[ -r /etc/stellarindex/postgres-password.txt ]] || return 1
  PGPASSWORD="$(cat /etc/stellarindex/postgres-password.txt)" \
    psql -h 127.0.0.1 -U stellarindex -d stellarindex -tAc "
      SELECT count(*) FROM unnest(ARRAY[
        'account_observations','blend_backstop_events',
        'cctp_events','claimable_observations',
        'decoder_stats_5m','defindex_flows','divergence_observations',
        'freeze_events','lp_reserve_observations','price_source_contributions',
        'rozo_events','sac_balance_observations','sdex_offer_events',
        'sep41_supply_events','soroswap_router_swaps','trustline_observations'
      ]) AS want(tbl)
      WHERE NOT EXISTS (
        SELECT 1 FROM timescaledb_information.jobs j
        WHERE j.hypertable_name = want.tbl
          AND j.proc_name = 'policy_compression'
      );
    " 2>/dev/null | grep -qx 0
}
# The want-list above is MAINNET-shaped: it names protocol-integration
# hypertables (blend/cctp/rozo/defindex …) that a lean SDEX-only test net
# never creates, so on those hosts the check fails for tables that are
# absent by design rather than for a missing compression policy. Gated on
# pubnet rather than "only check tables that exist", because the latter
# would ALSO weaken r1 — a hypertable silently disappearing there would
# start passing. r1 semantics are unchanged; is_pubnet treats an
# unreadable config as pubnet so this can never silence r1.
if is_pubnet; then
  assert_cmd compression_policies_applied compression_policies_applied
else
  skip compression_policies_applied
fi

# ── tx_hash_index parity probe (explorer 404 authority, 2026-08-01) ──
# GET /v1/tx/{hash} treats a stellar.tx_hash_index MISS as an
# AUTHORITATIVE not-found (the bloom-scan fallback for index misses was
# retired in the 2026-07-30 account-filter class audit, once the 10.2B
# ch-txindex-backfill verified genesis→tip parity — a one-time check).
# The index is maintained by a materialized view over
# stellar.transactions, and NOTHING re-detects future divergence: a
# tx_hash_index_mv DROP/recreate window, or any load path that inserts
# into stellar.transactions without firing MVs (ATTACH-PARTITION-style
# loads), silently turns real transactions into 404s.
#
# Probe shape: sampled parity over the trailing 10k ledgers, NOT a
# windowed count comparison — tx_hash_index is ORDER BY tx_hash with no
# ledger_seq index, so "count rows WHERE ledger_seq > tip-10k" on the
# index side is an unbounded full-column scan over 20B+ rows, exactly
# the read class the index exists to avoid. Instead: draw 500 random
# tx hashes from the window's transactions partitions (partition-pruned,
# sub-second) and require EVERY one to resolve in the index via
# primary-key point lookups (µs each; 500 scattered lookups measured
# 0.77s on r1). A dropped-MV window of W ledgers inside the probe window
# escapes one run with probability (1-W/10000)^500 — negligible by the
# second hourly run for any outage worth catching. DISTINCT on the
# sample because transactions is a ReplacingMergeTree (unmerged
# duplicate rows must not double-count). CH via HTTP :8123 with curl —
# dependency-light, and the clickhouse-client default native port on r1
# famously hits MinIO (:9000), not CH (:9300).
# shellcheck disable=SC2329  # invoked indirectly via assert_cmd's "${@:2}"
tx_hash_index_parity() {
  local ch="http://127.0.0.1:8123/"
  local tip floor sample n in_list found
  tip=$(curl -sS --max-time 15 "$ch" --data-binary \
    'SELECT max(ledger_seq) FROM stellar.ledgers') || return 1
  [[ "$tip" =~ ^[0-9]+$ && "$tip" -gt 10000 ]] || return 1
  floor=$((tip - 10000))
  sample=$(curl -sS --max-time 60 "$ch" --data-binary "
    SELECT DISTINCT tx_hash FROM stellar.transactions
    WHERE ledger_seq > ${floor}
    ORDER BY rand() LIMIT 500
    SETTINGS max_threads = 4, max_memory_usage = 4294967296") || return 1
  # An empty sample means the lake has no transactions in the trailing
  # 10k ledgers — itself never-true on mainnet, so fail loud.
  n=$(printf '%s\n' "$sample" | grep -cE '^[0-9a-f]{64}$')
  [[ "$n" -gt 0 ]] || return 1
  in_list=$(printf '%s\n' "$sample" | grep -E '^[0-9a-f]{64}$' \
    | sed "s/.*/'&'/" | paste -sd, -)
  found=$(curl -sS --max-time 60 "$ch" --data-binary "
    SELECT uniqExact(tx_hash) FROM stellar.tx_hash_index
    WHERE tx_hash IN (${in_list})
    SETTINGS max_threads = 4, max_memory_usage = 4294967296") || return 1
  [[ "$found" == "$n" ]]
}
assert_cmd tx_hash_index_parity tx_hash_index_parity

# ── ClickHouse ops-batch identity, both halves live (2026-08-28 r1) ──
# The low-priority `ops_batch` profile only protects the aggregator /
# indexer from a heavy stellarindex-ops job when BOTH the CH-side user
# (users.d/ops-batch.xml, --tags clickhouse-ops-batch-profile) and the
# env pair the jobs authenticate with (/etc/default/stellarindex-ops,
# --tags minio) are in place. Applying one tag without the other is
# either a silent no-op (user, no env: every job still runs as
# `default` at serving priority — the incident) or a hard break (env,
# no user: the next job fails auth). Skipped, not failed, while the
# profile is not provisioned on this host (the flag defaults false).
# Third leg: the pair must NEVER reach the service env — the
# ansible-templated stellarindex-{indexer,aggregator,api} units source
# /etc/default/stellarindex, and the same package seam would demote the
# live sink + supply refresher to lowest priority (the inverse fix).
if [[ -f /etc/clickhouse-server/users.d/ops-batch.xml ]]; then
  assert_grep ch_ops_batch_env_pair /etc/default/stellarindex-ops \
    '^STELLARINDEX_CLICKHOUSE_OPS_USER=.+'
else
  skip ch_ops_batch_env_pair
fi
assert_cmd ch_ops_batch_not_in_service_env bash -c \
  '! grep -qE "^STELLARINDEX_CLICKHOUSE_OPS_(USER|PASSWORD)=" /etc/default/stellarindex 2>/dev/null'

mv "$TMP" "$OUT"
chmod 644 "$OUT"
echo "config-assertions: $fails failure(s)" >&2
exit "$fails"
