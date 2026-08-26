# Testnet / Futurenet reset runbook

Test networks **reset**: Testnet ~quarterly; Futurenet on protocol bumps.
On a reset the ledger sequence restarts at **1** and the passphrase is
**unchanged**. This is the main operational risk for an unattended test
net, because indexer resume is **forward-only** — after a reset the cursor
sits at a high ledger while the live net is back near 1, so ingest
**stalls**. This runbook restores it. Companion:
[testnet-futurenet-deployment.md](./testnet-futurenet-deployment.md).

Mainnet never resets — this runbook is test-net only.

## Two flavors, one primitive

| Flavor | When | Extra step |
| --- | --- | --- |
| **Plain reset** | Testnet, ~quarterly, same protocol | none |
| **Upgrade-reset** | Futurenet, on a protocol bump | swap core / galexie / SDK **first** |

Both resolve to: **detect → halt → wipe → re-ingest from a chosen start ledger.**

## Detect

A reset shows up as one or more of:

- Ingest **stalled** with the live tip far **below** the persisted cursor
  (the cursor-stuck alert fires).
- A **prev_hash discontinuity** at the tip — the authoritative signal. The
  live ledger's `PrevLedgerHash` does not chain onto the last one we
  recorded (census `LedgerHash`/`PrevLedgerHash`, `dispatcher/census.go`).
- An announced testnet reset / a futurenet protocol upgrade.

> ⚠ **tip-regression ALONE false-positives.** Bounded ops backfills
> legitimately race the live tip, so "tip < cursor" by itself is not proof.
> Require the **prev_hash break** (or a human-confirmed reset) before
> wiping — never wipe on tip-regression alone.

## Halt (fail-safe — never overwrite)

Stop ingest so a post-reset galexie can't overwrite good data mid-flight:

```sh
systemctl stop stellarindex-indexer galexie cap67-movements.service
```

## Wipe — THREE stores + ALL watermarks

A reset invalidates the network's entire derived + source state. Wipe all
three stores; a partial wipe strands watermarks/CAGGs and re-stalls:

- [ ] **PostgreSQL** — drop + recreate the `stellarindex` DB (do NOT just
      truncate ledger-keyed tables: that strands the TimescaleDB CAGGs and
      the derive watermarks). Re-run migrations.
- [ ] **ClickHouse** — drop + recreate the `stellar.*` lake tables (or the
      DB). Truncation alone leaves the cap67 watermark past the wipe.
- [ ] **MinIO** — empty `galexie-live` and `galexie-archive` (post-reset
      LCM history is worthless; galexie would otherwise read stale
      high-sequence objects until overwritten).
- [ ] **Watermarks** — `stellar.cap67_movements_watermark`, the SEP-41
      supply watermark, and the ingestion cursor → back to genesis.

**Nuclear option (cleanest): re-provision the VM.** Because each network is
its own libvirt/KVM VM, destroying + recreating it (re-run
`configs/libvirt/provision-vms.sh` for that domain, or `virsh snapshot-revert`
to a bare post-install snapshot) wipes all three stores + every watermark
atomically. On a tiny test net this is often faster and less error-prone than
the surgical wipe above. (Re-provisioning also empties `galexie-live`, so
galexie then honors the updated `GALEXIE_START` — see below.)

## Futurenet only — swap the stack first

A futurenet reset is usually a **protocol upgrade**. Before re-ingesting:

- [ ] Bump `galexie_version` (+ expected version string + sha256) and the
      captive stellar-core to the new-protocol build.
- [ ] Bump the `go-stellar-sdk` (XDR) version in the indexer if the new
      protocol adds op/event types — this is the early-warning payoff:
      decode failures here surface **before** the protocol reaches Mainnet.
- [ ] Then wipe + re-ingest as above.

## Re-ingest — pick a start ledger FIRST

A reset restarts the chain at ledger 1, so the committed `galexie_start_ledger`
/ `stellarindex_backfill_from_ledger` (a recent value like 4340000) are now
**above the new tip** and would make galexie refuse to start. Update **both,
together** in the inventory before re-ingesting — they MUST stay equal:

- **Full history of the new cycle** (recommended right after a reset, while the
  chain is small): set both low — `galexie_start_ledger: 64` (a checkpoint
  boundary) and `stellarindex_backfill_from_ledger: 2`.
- **Recent start** (once the cycle has grown to millions of ledgers): set both
  to ~10k below the current archive tip (see the deployment doc).

Then re-render config (targeted ansible `--tags galexie,stellarindex`) and:

```sh
# galexie-live was wiped above, so galexie starts fresh from GALEXIE_START.
systemctl start galexie
# confirm galexie-live is filling from your chosen start, THEN start the rest:
systemctl start stellarindex-indexer cap67-movements.service
```

`stellar.movements_floor_ledger` / `soroban_genesis_ledger` are already **1** on
test nets and `cap67-movements` runs `-floor-ledger 1`, so the movements derive
never floors above the chain regardless of the start ledger.

## Verify

- [ ] Ingest advancing from your chosen post-reset start ledger.
- [ ] `/v1/accounts/{g}/movements` returns rows for a fresh post-reset tx.
- [ ] No prev_hash-break warnings after the first post-reset ledger.

## Related

- [testnet-futurenet-deployment.md](./testnet-futurenet-deployment.md).
- `cmd/stellarindex-indexer/main.go` `resolveStartLedger` (forward-only resume).
- `internal/dispatcher/census.go` — the `PrevLedgerHash` chain used for detection.
- Future work: `stellarindex-ops reset-network` (one-command wipe) +
  automated `stellarindex_network_reset_suspected` alert (prev_hash-gated).
