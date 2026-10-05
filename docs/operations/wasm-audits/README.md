---
title: Per-WASM-hash decoder audits — procedure
last_verified: 2026-05-03
status: living procedure
---

# WASM audits

One audit log per on-chain Soroban source, as the evidence trail for a single
`internal/sources/external.Registry` `BackfillSafe` flip `false` → `true`.
Until it lands, the source's `stellarindex-ops backfill` runs are refused; once
the audit shows the decoder handles every WASM version in the replay range, the
flag flips in the same PR.

Why (AGENTS.md "Soroban DeFi contracts upgrade in place"): Soroswap, Aquarius,
Phoenix, Comet, Reflector\*, Redstone, and Band can each `update_contract` at
the same address, changing event body schemas / topic shapes. Live ingest sees
only the current WASM; a since-inception backfill replays every prior version,
and a current-only decoder silently produces wrong trades.

## Files in this directory

- `README.md` — this file. Procedure + checklist.
- `internal/wasmaudit/audited_wasm.json` — the machine-readable audited set: every hash these logs string-checked, which `stellarindex-ops wasm-drift` checks each gated contract against ([runbook](../runbooks/wasm-drift.md)).
- `soroswap.md` — Soroswap audit (in progress).
- `sushiswap_v3.md` — SushiSwap V3 factory + 58 pools, four hashes string-checked from the lake's instance lineage.
- `upshift.md` — Upshift earnUSDC / earnXLM vaults, one hash string-checked from the lake's instance lineage.
- (`aquarius.md`, `phoenix.md`, `comet.md`, `reflector-{dex,cex,fx}.md`,
  `redstone.md`, `band.md` — to land per source.)

## Procedure

### 1. Identify the contracts to audit

- The **factory contract** (if any) — emits `new_pair` / pool-creation events;
  topic schema changes here add or remove pools we'd want to ingest.
- The **router contract** (if any) — routed-swap aggregation events and
  multi-hop accounting.
- The **pair / pool contract WASM hash** — per-instance contracts emitting the
  `swap` events. Instances share one hash (the factory deploys from a
  registered hash); upgrading the factory's stored pair WASM hash is the
  typical failure mode.

Contract IDs: search for `Mainnet*` constants in
`internal/sources/<source>/events.go`.

### 2. Collect the WASM-version timeline

Run `stellarindex-ops wasm-history` against a galexie data store covering the
replay range (output location: "Where walk output goes" below):

    mkdir -p /var/log/wasm-audit
    stellarindex-ops wasm-history \
      -config /etc/stellarindex.toml \
      -from 2 -to <r1-archive-tip> \
      -contracts <factory>,<router>,<pair-instance-A>,<pair-instance-B>... \
      > /var/log/wasm-audit/<source>-wasm-history.json \
      2> /var/log/wasm-audit/<source>-wasm-history.stderr

> **`-to` upper bound on r1.** Use the verified tip of `galexie-archive`, NOT
> the network tip. As of 2026-05-01 the archive is frozen at **62,249,727**
> (last ledger fully exported during the historical fill); live galexie writes
> to `galexie-live`, which the walker does not consult. A `-to` past that
> fails partway with `ledger object containing sequence X is missing` (the
> partial trailing partition, not corruption). Cross-check the safe boundary
> against `/var/lib/galexie/detect-gaps.json` on r1 (`scan_to` field) and
> [docs/operations/r1-deployment-state.md §3a](../r1-deployment-state.md).

Output is a JSON timeline `(contract_id → [(active_from, active_to,
wasm_hash)])`; save it verbatim into the audit log under "WASM timeline".

For pairs you can't enumerate ahead of time (Soroswap pairs are deployed at
runtime), take every pair ID from the factory's `new_pair` events and pass all
as `-contracts`. For an MVP audit the dominant pairs by volume suffice; full
coverage is the v2 upgrade.

**Where to run `wasm-history`.**

- **r1 directly** — only when r1's verify-archive walk is idle (the scan reads
  ~46 MB/sec from MinIO and competes with verify-archive on ZFS ARC).
- **A separate workstation pointed at AWS public bucket.** Set galexie's
  `cfg.Storage.S3BucketArchive = "aws-public-blockchain"`
  + `S3Endpoint = "https://s3.us-east-1.amazonaws.com"` +
  `S3BucketArchivePrefix = "v1.1/stellar/ledgers/pubnet/"`. Costs bandwidth
  (Hetzner-out → AWS-in is free, AWS-out → home is the paid leg).

**Where walk output goes.** Every artefact (stdout JSON, stderr progress log,
`-checkpoint-dir` JSONL) goes under `/var/log/wasm-audit/`, never loose in
`/var/log/` or `/tmp/` (a multi-hour walk's stderr runs to gigabytes). Once the
audit log records the timeline (and any JSON worth keeping is committed under
[`evidence/`](evidence/)), delete the walk's files:

    rm -rf /var/log/wasm-audit/<source>-*

**Crash-resilience for long walks.** Pass `-checkpoint-dir DIR` on every
multi-hour walk; the walker refuses a dir that does not exist, so create it
first:

    mkdir -p /var/log/wasm-audit/<source>-checkpoint
    stellarindex-ops wasm-history \
      -config /etc/stellarindex.toml \
      -from 50457424 -to 62249727 \
      -parallel 8 \
      -checkpoint-dir /var/log/wasm-audit/<source>-checkpoint \
      -contracts ... > /var/log/wasm-audit/<source>-wasm-history.json \
      2> /var/log/wasm-audit/<source>-wasm-history.stderr

Each parallel worker writes transitions to `<DIR>/wasm-history-w<i>.jsonl`. If
the walk dies before its end-of-run JSON write (crash, reboot, missing ledger
in the archive — see [r1-deployment-state.md §3a](../r1-deployment-state.md)),
recover the JSON from the partial transitions:

    stellarindex-ops wasm-history-merge-jsonl \
      -checkpoint-dir /var/log/wasm-audit/<source>-checkpoint \
      -to 62249727 \
      -output /var/log/wasm-audit/<source>-wasm-history-recovered.json

`-to` MUST match the original walk's `-to` so the last open range per contract
closes correctly. The merge tool skips a half-written trailing line.

The recovered JSON shape matches `wasm-history`'s, for contracts with at least
one observed transition. Contracts scanned with no transitions ARE NOT emitted
(the JSONL records only transitions); if the audit needs that "ran but saw
nothing" signal, restart the walk from the gap region.

### 3. Per-WASM-hash decoder review

For each unique hash in the timeline, fetch the WASM and inspect the
event-emitting code paths:

- **stellar-core**: `stellar-core get-wasm <hash> > /tmp/<hash>.wasm`
  if a captive-core's bucket dir has it cached.
- **stellar-rpc** (if running): `stellar-rpc getLedgerEntry`
  with the WASM-storage key.
- **galexie LCM** (last resort): walk LedgerEntryChange entries for the install
  ledger and extract the WASM bytes.

Disassemble with `wasm2wat`:

    wasm2wat <hash>.wasm | grep -A 5 'events.publish'

…or read the source contract repo at the git tag for that hash (Soroswap
publishes at github.com/soroswap/core; tags map to releases).

Verify each emitted event against the decoder's expectations ("Decoder
expectations" in each per-source log):

| failure mode | what to check |
| --- | --- |
| New event topic added | does the decoder skip it cleanly (good) or trip a parse error (bad)? |
| Existing topic name changed | does our `Topic*` constant still match the new wire bytes? |
| Topic[0] prefix string changed (e.g. "SoroswapPair" → "SoroswapPairV2") | byte-equal classification stops matching — silent drop |
| Event body field renamed | by-name extraction (per ADR-0007) returns missing-field error — explicit failure, but every trade in the range is dropped |
| Event body field added | by-name extraction ignores the new field — fine |
| Event body field removed | extraction errors out — every trade dropped |
| i128 / u128 sign or scale change | decoder may produce wrong magnitudes — caught only by Hubble cross-check, not by a parse error |
| Event split into multiple events (e.g. `swap` → `swap_in` + `swap_out`) | correlation logic breaks; decoder may emit 0 or 2 trades per swap |
| Topic arity changed (2-tuple → 3-tuple) | classification matches on `topic[0..2]` so a longer topic still matches — but our position-based assumptions about further topic slots break |

If every hash passes, document the findings and flip `BackfillSafe: true` in
`internal/sources/external/registry.go` in the same PR as the audit log.

If any hash diverges, fix the decoder (plus a fixture test under
`internal/sources/<source>/` against that hash) in the same PR. Don't flip
`BackfillSafe` if a hash needs a decoder change that isn't deployed yet.

### 4. Hubble cross-check (where applicable)

Where Hubble has a decoded view (currently SDEX only — see
`cmd/stellarindex-ops/hubble_check.go`), running `hubble-check` over the
replay range proves decoder + audit produce correct output. Soroban sources
have no Hubble view; the WASM audit is the load-bearing check.

### 5. Document + flip

Update the audit log with:

- The full `wasm-history` JSON output.
- The unique WASM hashes seen.
- Per-hash findings (one line each: "matches current decoder" or "diverges,
  fix landed in PR #N").
- Active ledger ranges per hash, so future audits resume from `(last audited ledger + 1)`.
- The decision: `BackfillSafe: true | false`.

Then flip the registry entry in the same PR.

## Audit log lifecycle

An audit log is **append-only** with respect to history. A hash audited as
"matches current decoder" for a ledger range keeps that finding; only the upper
bound extends as the network closes ledgers. New hashes get appended; if any
diverges, flip `BackfillSafe: false` and ship the fix.

`last_verified` in each audit doc's frontmatter MUST be updated whenever the
audit extends — the docs CI (`scripts/ci/lint-docs.sh`) fails on stale
freshness markers.
