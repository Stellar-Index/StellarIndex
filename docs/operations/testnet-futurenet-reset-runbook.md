---
title: Testnet / Futurenet reset runbook
last_verified: 2026-11-20
status: current
---

# Testnet / Futurenet reset runbook

Testnet resets ~quarterly, Futurenet on protocol bumps. The ledger sequence
restarts at 1, the passphrase is unchanged. Indexer resume is forward-only, so
ingest stalls (cursor high, live tip near 1). Mainnet never resets. Companion:
[testnet-futurenet-deployment.md](./testnet-futurenet-deployment.md).

Flow: detect, halt, wipe, re-ingest from a chosen start ledger. A Futurenet
protocol bump adds one step first: swap the stack.

## Detect

- Ingest stalled with the live tip far below the persisted cursor (cursor-stuck alert).
- A prev_hash discontinuity at the tip: the live `PrevLedgerHash` does not chain
  onto the last recorded one (`internal/dispatcher/census.go`). This is the authoritative signal.
- An announced reset or upgrade.

Never wipe on tip-regression alone: bounded ops backfills legitimately race the
live tip. Require the prev_hash break or a human-confirmed reset.

## Halt

Every command below is destructive and the same shell could be on r1 (mainnet, never reset). Run them only on the test-net VM, reached through the jump host (`scripts/dev/region-binaries.tsv`: testnet `192.168.122.10`, futurenet `192.168.122.20`). First confirm where you are: `hostname -I` shows that VM's address, and r1's `136.243.90.96` is NOT in the output. The passphrase is unchanged across a reset, so it cannot tell the networks apart.

The role also starts unprefixed ops-job timers (some enabled by hand, outside
its loop) and the `si-catchup-*` services on every network, test nets
included; they write the DBs and the lake. Stop every active timer the role
installed. The list comes from systemd, not from a copy of the role: every
role unit file lives under `/etc/systemd/system`, the OS timers (`apt-daily`,
`logrotate`, ...) under `/usr/lib/systemd/system`.

```sh
# active timers whose unit file the role installed; kept for the restart in Re-ingest
systemctl list-timers --no-legend --plain | grep -oE '[^ ]+\.timer' | sort -u | while read -r t; do
  case "$(systemctl show -p FragmentPath --value "$t")" in /etc/systemd/system/*) echo "$t" ;; esac
done > /root/reset-halted-timers.txt
cat /root/reset-halted-timers.txt
while read -r t; do systemctl stop "$t" "$(systemctl show -p Unit --value "$t")"; done < /root/reset-halted-timers.txt
systemctl stop stellarindex-indexer galexie cap67-movements.service stellarindex-api stellarindex-sla-probe 'si-catchup-*'

# nothing may still write or read the DBs: both must print nothing
systemctl list-units 'stellarindex-*' 'cap67-*' 'si-catchup-*' galexie.service --state=active --no-legend
while read -r t; do systemctl list-units "$t" "$(systemctl show -p Unit --value "$t")" --state=active --no-legend; done < /root/reset-halted-timers.txt
```

The aggregator is off on testnet and absent on futurenet; stop it if the listing shows it active.

## Wipe all three stores and every watermark

A partial wipe strands watermarks/CAGGs and re-stalls.

- PostgreSQL: drop and recreate the `stellarindex` DB, re-run migrations. Do not
  truncate ledger-keyed tables (strands the CAGGs and derive watermarks).
  - Before the drop, export the non-chain tables with `\copy` (a `pg_dump` cannot
    carry the two hypertables): `accounts`, `users`, `api_keys`, `subscriptions`,
    `invites`, `webauthn_credentials`, `price_alerts`, `customer_webhooks`,
    `webhook_deliveries`, `stripe_event_log`, `audit_log`, `erased_account_slugs`,
    `source_exclusions`, `usage_daily`, `api_usage_events`. Restore them after
    the migrations in one transaction (`ON CONFLICT DO NOTHING`), compare row
    counts, then delete the export (it holds credential hashes).
  - Recreate the DB as `05-postgres.yml` does: owner `stellarindex`, UTF8 from
    `template0`, `jit = off`, extensions `timescaledb` and `pg_stat_statements`.
- ClickHouse: drop and recreate the `stellar.*` lake tables (or the DB).
- MinIO: `mc rb --force` then `mc mb` each of `galexie-live` and `galexie-archive`
  on root's `local` alias (no credentials to handle).
- Disk: remove every `captive-core*` directory under `/var/lib/galexie` (galexie's
  captive core creates it at run time and it holds the state), empty
  `/var/lib/galexie/captive` (created by the role), move the `si-catchup` queue files
  (`/var/lib/stellarindex/catchup/*.queue|done|failed|deferred`) and
  `verify-archive-state.json` aside so old-cycle work is not replayed.
- Redis: flush it. API keys live here under the default `auth_backend`
  (`internal/auth/apikey_redis.go`), so the flush deletes every key, including
  the explorer build key; see Verify.
- Watermarks to genesis: `stellar.cap67_movements_watermark`, the SEP-41 supply
  watermark, the ingestion cursor.
- The ledger-hash baseline: `rm -f /var/lib/stellarindex/hashdb.bin` (the indexer's
  `hashdb.path`). Its records are keyed by sequence alone, so the old cycle's file
  reads every new-cycle ledger as drift and refuses to record it.

Cleanest: re-provision the VM (`configs/libvirt/provision-vms.sh` for that
domain, or `virsh snapshot-revert` to a bare post-install snapshot). That wipes
all three stores and every watermark, and empties `galexie-live` so galexie
honors the new `GALEXIE_START`.

## Futurenet only: swap the stack first

- Bump `galexie_version` (+ expected version string + sha256) and captive stellar-core.
- Bump `go-stellar-sdk` (XDR) in the indexer if the protocol adds op/event types.

## Re-ingest

After a reset the committed `galexie_start_ledger` / `stellarindex_backfill_from_ledger`
are above the new tip and galexie refuses to start. Set both together in the
inventory; they MUST be equal. Galexie never exports a ledger below its start,
and the indexer's live-hole check ends the read when its first ledger is absent
below the tip (`internal/ledgerstream/live_hole.go`), so unequal values crash-loop
the indexer.

- Full history of the new cycle (while the chain is small): both `2`
  (`galexie-append.sh` accepts any start above 1).
- Recent start (cycle grown to millions of ledgers): both ~10k below the archive tip.

The role starts services itself, so run it three times, in order, from a
checkout of the deployed release tag (the role syncs `migrations/` from it):

```sh
# GS == BF; keep the -e pair on every run until the inventory carries it
ansible-playbook ... playbooks/archival-node.yml -e galexie_start_ledger=$GS -e stellarindex_backfill_from_ledger=$BF \
  --tags clickhouse-install -e clickhouse_apply_schema=true   # 1: lake DDL, tier1_schema.sql only
# 2: --tags galexie; wait until galexie-live's first exported ledger == BF
# 3: --tags stellarindex -e stellarindex_apply_migrations=true
```

Apply no operator-scope ClickHouse file in run 1: `tier1_schema.sql` alone creates
every object. Never run the `galexie` and `stellarindex` tags together after a wipe.
Run 3 starts the indexer, API, `si-catchup` lanes, `cap67-movements` and the role's
timers; restore the preserved Postgres rows once it finishes, then
`xargs systemctl start < /root/reset-halted-timers.txt` for any halted timer still stopped.

After the reset, commit the new pair in the host's inventory; until then every
role run needs both as `-e` overrides, or it re-renders the old values.

`stellar.movements_floor_ledger` / `soroban_genesis_ledger` are already 1 on test
nets and `cap67-movements` runs `-floor-ledger 1`, so the derive never floors above the chain.

## Verify

- Ingest advances from the chosen start ledger.
- `/v1/accounts/{g}/movements` returns rows for a fresh post-reset tx.
- No prev_hash-break warnings after the first post-reset ledger.
- Re-mint the explorer build key on that host (the flush deleted it) and update the
  repo secret `EXPLORER_BUILD_API_KEY_<NETWORK>`
  ([explorer-deployment.md](./explorer-deployment.md#test-nets-testnet--futurenet)), then run
  `explorer-deploy.yml` for that network. Without it the build fails with HTTP 401
  from `web/explorer/next.config.mjs`.
- The daily `fleet-release-drift.yml` reports the host `DEGRADED` while the
  region API is down; the tracking issue closes itself once it serves r1's release.

### The archive bucket after a reset

The wipe leaves `galexie-archive` empty and nothing writes it on a test net
(`galexie-archive-fill` is pubnet-only); it is only ever filled by an operator-run
`galexie scan-and-fill` of `[1, first-live - 1]`, which is empty when live starts at ledger 2.
An archive empty for that reason is healthy, and the checks say so instead of failing:

- Condition (all three checks): the archive listing succeeded and returned no object
  at all (no ledger file, no `.config.json`), and the oldest live object starts at or
  below ledger 2 (galexie names its first file `--0-63`, or `--2` for 1-ledger files).
  A live bucket whose first file is `--64-127` (galexie started late) leaves ledgers 2-63 in
  neither bucket, so it fails.
- `galexie-archive-tip-lag`: `probe_success 1`, `galexie_archive_tip_lag_expected_empty 1`,
  the live tip published, the archive tip and lag absent.
- `galexie-archive-contiguity`: `galexie_archive_expected_empty 1`, `unexpected_gaps 0`,
  `partition_count 0`.
- `verify-archive-tier-a`: logs "archive bucket ... is empty by design" and exits 0 without walking.
  The checkpoint tier (`-tier checkpoint`, tier B) skips the same way: it anchors the hashes of
  the ledgers the walk reads, so with no archive ledgers there is nothing for it to check
  (tier B is not installed on test nets anyway).
- Still failing, as before: archive bucket unreadable, archive holding any object
  (a lone manifest included), a live bucket starting above ledger 2 (the archive range is then
  non-empty and unfilled), or a live start that cannot be measured.

Once an operator fills the archive, the normal checks apply.

Daily jobs that need
chain data fail or alert until the chain fills; worry if one still fails after the first
daily cycle with activity on chain.

### A source reports incomplete after a reset

Read the verdict's `detail`, which names the per-target delta and window or says PENDING
(`stellarindex_completeness_incomplete`, [data-freshness runbook](./runbooks/data-freshness.md#stellarindex_completeness_incomplete)):

```sh
sudo -u postgres psql -d stellarindex -c \
 "SELECT source, complete, watermark_ledger, detail FROM (SELECT DISTINCT ON (source) * \
  FROM completeness_snapshots ORDER BY source, computed_at DESC) s WHERE NOT complete;"
```

Floors from the old chain live in two PostgreSQL tables, created by migrations 0116 and 0125:
`completeness_target_floors` (lowest ledger each target was ever verified from; it only falls,
so an old-chain value above the new start would be read as loss) and `projection_dirty_windows`
(old-cycle replay windows). Neither is in the preserved-table list, and the drop and recreate of the
`stellarindex` DB with migrations re-run leaves both empty, so no old-chain floor survives a
full wipe. They survive only after a partial wipe (DB kept): check with
`SELECT source, target_table, projection_verified_from FROM completeness_target_floors;` and
`SELECT * FROM projection_dirty_windows;`. The remedy is the full wipe above, not editing
rows. After the wipe, the first `compute-completeness` run seeds fresh floors; to re-verify a
source sooner use the existing command, `stellarindex-ops compute-completeness -config
/etc/stellarindex.toml -ch -source <X> -from <recent> -write`.

Code: `cmd/stellarindex-indexer/main.go` `resolveStartLedger` (forward-only resume).
