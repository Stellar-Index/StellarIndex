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
- ClickHouse: drop and recreate the `stellar.*` lake tables (or the DB).
- MinIO: empty `galexie-live` and `galexie-archive`.
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
inventory; they must stay equal:

- Full history of the new cycle (while the chain is small): `galexie_start_ledger: 64`
  (checkpoint boundary), `stellarindex_backfill_from_ledger: 2`.
- Recent start (cycle grown to millions of ledgers): both ~10k below the archive tip.

Re-render config (`ansible --tags galexie,stellarindex`), then:

```sh
systemctl start galexie   # fresh from GALEXIE_START; confirm galexie-live fills, THEN:
systemctl start stellarindex-indexer cap67-movements.service
xargs systemctl start < /root/reset-halted-timers.txt   # the timers the halt stopped
```

`stellar.movements_floor_ledger` / `soroban_genesis_ledger` are already 1 on test
nets and `cap67-movements` runs `-floor-ledger 1`, so the derive never floors above the chain.

## Verify

- Ingest advances from the chosen start ledger.
- `/v1/accounts/{g}/movements` returns rows for a fresh post-reset tx.
- No prev_hash-break warnings after the first post-reset ledger.
- The daily `fleet-release-drift.yml` reports the host `DEGRADED` while the
  region API is down; the tracking issue closes itself once it serves r1's release.

Code: `cmd/stellarindex-indexer/main.go` `resolveStartLedger` (forward-only resume).
