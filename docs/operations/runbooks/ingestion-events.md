---
title: Runbook — ingestion-events
last_verified: 2026-10-06
status: living
severity: P3
---

# Runbook — ingestion-events alerts

Ingestion event-quality alerts: orphan events, oracle symbols, uncorroborated calls. Merged from three former pages.

## At a glance

- [`stellarindex_ingestion_orphan_events`](#stellarindex_ingestion_orphan_events)
- [`stellarindex_ingestion_oracle_unknown_symbols`](#stellarindex_ingestion_oracle_unknown_symbols)
- [`stellarindex_ingestion_oracle_unrepresentable_symbols`](#stellarindex_ingestion_oracle_unrepresentable_symbols)
- [`stellarindex_ingestion_uncorroborated_calls`](#stellarindex_ingestion_uncorroborated_calls)

## stellarindex_ingestion_orphan_events

_Source page `ingestion-events.md#stellarindex_ingestion_orphan_events`: status current, severity P3, last verified 2026-08-29._

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingestion_orphan_events` |
| Severity | P3 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/ingestion.yml` (group `stellarindex.ingestion`, `severity: ticket`, `for: 15m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/ingestion.yml`. |
| Typical MTTR | hours-to-days (investigation) |
| Impact | Losing individual swap / oracle updates. Not urgent unless the rate spikes to double-digit events/sec. |

### Symptoms

- `sum by (source) (rate(stellarindex_source_orphan_events_total[10m])) > 10/60` sustained 15 min.
- Per-source breakdown in the alert label shows WHICH source is dropping events.
- `stellarindex_source_events_total` for the same source may still rise — orphans are a subset of pulled events that couldn't be completed.

### Context — what counts as an orphan?

Depends on the source:

- **soroswap** — a `swap` event without its matching `sync` (or vice versa), correlated by `(ledger, tx_hash, op_index)`. Both are emitted in the same Soroban transaction and arrive adjacent in the dispatcher's in-order per-ledger stream — so an orphan implies the decoder rejected one half or the contract's event shape shifted, not transport reordering.
- **phoenix** — one swap emits 8 separate events (one per field). An orphan is an incomplete N-of-8 set that aged past the buffer's `defaultOrphanMaxAge` (5 min) without the missing fields arriving.
- **aquarius, reflector** — N/A. These sources are 1-event-per-observation and can't produce orphans.

### Quick diagnosis (≤ 10 min)

```sh
# Which source is orphaning? (alert label also tells you this;
# 9464 = the indexer's metrics port on r1)
ssh root@136.243.90.96 'curl -s localhost:9464/metrics | grep stellarindex_source_orphan_events_total'

# Look at the indexer's logs for the affected source — orphans get
# logged at debug level with the group key.
ssh root@136.243.90.96 "journalctl -u stellarindex-indexer -n 1000 --no-pager" \
  | grep -E "orphan|evicted" | tail -20

# Compare to the decode-error rate and the event-rate over the
# same window:
#   rate(stellarindex_source_events_total[10m])
#   rate(stellarindex_source_decode_errors_total[10m])
#   rate(stellarindex_source_orphan_events_total[10m])
# Orphan-rate rising while event-rate is flat → a decoder is
# rejecting one half of the pair (check decode_errors) or the
# contract's event schema shifted (contracts upgrade in place).
# Event-rate + orphan-rate both rising → source volume spike with
# ordering happening to fall outside our buffer window.
```

### Mitigation (≤ 15 min)

**No live-fix path** — this is a `severity: ticket` alert, not a
page. Don't restart or roll back on the basis of orphan-events alone;
orphans are a subset of events that couldn't be correlated, not a
blocking failure. The conventional mitigation step is "investigate
upstream" — see the next section.

If the rate is genuinely catastrophic (`> 100/sec`, see "When to
escalate" below), promote to a `source-stopped` response: the
source is effectively not working, not just dropping a few rows.

### Investigation

This alert is a ticket, not a page; there's no live-fix path. Instead, gather:

- [ ] Sample a few orphan group-keys from the logs. Query a public
      stellar-rpc directly for their tx_hash (r1 doesn't run its
      own stellar-rpc — removed 2026-04-23, see
      [r1-deployment-state.md](../r1-deployment-state.md)):
  ```sh
  curl -X POST https://mainnet.sorobanrpc.com \
    -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"getTransaction","params":{"hash":"<tx>"}}'
  ```
  If the tx is retention-window-NOT_FOUND, RPC already dropped it.
- [ ] Check the contract ID's event stream on stellar.expert or via `getEvents` with the specific filter. A contract that changed its event shape would show up as phoenix decode_errors AND soroswap orphans simultaneously.
- [ ] If the phoenix orphan rate > soroswap's: the 5-min buffer `defaultOrphanMaxAge` may be too short. Phoenix's 8-event emission can span multiple transactions in pathological cases.

### When to escalate

- `> 100/sec` sustained — the source is effectively not working. Treat as `source-stopped`, not `orphan-events`.
- Orphan-rate matches event-rate — the correlation logic is broken (every event gets orphaned). Revert the most recent source-package change.

### Changelog

- 2026-04-23 — initial draft alongside the orphan-events metric wiring.
- 2026-04-30 — getTransaction probe URL points at a public
  stellar-rpc; r1 doesn't run its own (removed 2026-04-23).
- 2026-08-29 — re-verified against HEAD: "same RPC page" /
  "RPC drops or reorders" framing replaced (swap+sync are adjacent
  in the dispatcher's in-order per-ledger stream — an orphan means
  a decode rejection or a contract-schema shift, not transport
  reordering), r1 command shapes (indexer :9464, r1 IP), dual-tree
  Detected-by. Status draft → current.
- 2026-09-24 — corrected `severity: informational` to `severity: ticket`
  throughout; both rule trees have always set `ticket` for this alert
  (#1354).

## stellarindex_ingestion_oracle_unknown_symbols

_Source page `ingestion-events.md#stellarindex_ingestion_oracle_unknown_symbols`: status current, severity P3, last verified 2026-09-08._

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingestion_oracle_unknown_symbols` — **and** its sibling `stellarindex_ingestion_oracle_unrepresentable_symbols`. Same shape, opposite meanings; see the next section before doing anything. |
| Severity | P3 both, but they are delivered differently on purpose. `unknown_symbols` is `severity: informational` → receiver `chat-informational` → Discord **#stellarindex-informational**, a deliberately low-traffic channel. `unrepresentable_symbols` is `severity: ticket` → receiver `chat-default` → Discord **#stellarindex-alerts**. Neither pages. |
| Detected by | Prometheus rule in `configs/prometheus/rules.r1/ingestion.yml` (the R1 overlay r1 actually loads); multi-host twin in `deploy/monitoring/rules/ingestion.yml`. |
| Typical MTTR | Not an outage clock. `unknown_symbols` earns a mapping *at leisure* — hours to days is fine, and the alert stays open meanwhile. `unrepresentable_symbols` is a registry entry plus a replay. |
| Impact | `unknown_symbols`: **nothing is lost.** An oracle published a symbol / feed_id the canonical allow-list does not map, and the slot was recorded verbatim as a `raw:<symbol>` row (`canonical.AssetOracleRaw`). The observation is record-layer-only until mapped: it is not a pair leg, not a VWAP input, and not visible on any keyed price surface. `unrepresentable_symbols`: the slot was **dropped with no row written** — that one IS a hole in the record. |

### The two alerts mean opposite things

Read the alertname before anything else.

| | `unknown_symbols` | `unrepresentable_symbols` |
| --- | --- | --- |
| What happened | Symbol/feed_id maps to no canonical asset | Symbol/feed_id fails even the permissive `raw:` validator |
| Row written | **Yes** — `raw:<symbol>` at the slot's own vector position | **No** — the slot is skipped |
| Recoverable later | Yes, in place: a registry entry plus a re-derive rewrites the same PK | No — only a replay can create the missing row |
| Severity | `informational` (quiet channel) | `ticket` |
| Emitters | `reflector`, `redstone`, `band` | `redstone` only, in practice |

Everything down to the `stellarindex_ingestion_oracle_unrepresentable_symbols` heading below is about `unknown_symbols`.

### Why `unknown_symbols` is informational, not a ticket

Changed 2026-09-08 (was `ticket`). The policy is deliberate and the
runbook should not be read as an incident:

- **An oracle listing a new token is routine business, not a fault of
  ours.** We index what our sources publish; we do not hold opinions
  about which tickers deserve to exist.
- **The observation IS captured.** Since the oracle capture-totality
  change every decoder writes the unmapped slot as a `raw:<symbol>`
  row rather than skipping it, so nothing is lost while the alert is
  open. Verified at HEAD in all three decoders:
  `internal/sources/reflector/decode.go`,
  `internal/sources/band/decode.go`,
  `internal/sources/redstone/decode.go` (`resolveFeedEntry`).
- **What it earns is a mapping**, so the token gets a real identity
  instead of a raw one — worth doing, not worth waking for.
- It routes to its own low-traffic channel precisely so a routine
  notice cannot bury a real ticket in the same feed.

It is **not** a low-priority ticket queue: nothing files a ticket from
this alert. If a symbol genuinely matters, open one by hand.

Its sibling stays a `ticket` for the one reason that matters: there the
slot is dropped with **no row written**, so there is nothing to promote
later.

Delivery and the full triage rationale live in
[alerts-catalog.md](../alerts-catalog.md) — the informational delivery
register — which is the source of truth for severity and routing.

### Why this alert exists at all

`stellarindex_source_unknown_symbols_total{source}` was added for
F-1234 (codex audit-2026-05-12) so an oracle expanding its feed set
would not be silent. The 2026-08-04 cold audit found the counter had
**no consumer in either rule tree** — the metric's own godoc claimed an
alert in `external-pollers.yml` that never existed — while r1 already
carried `{source="reflector"} 7794`. At that time an unmapped slot was
*dropped*, so those 7,794 were real losses. The 2026-07-24 RedStone
relayer expansion lost ~5,600 events the same way until
`internal/sources/redstone/feeds.go` caught up.

Capture-totality (`docs/design/oracle-capture-totality-design.md`)
closed that: the record layer is now total. This alert survives as the
*mapping-gap* signal, which is why it no longer carries ticket
severity.

### Symptoms

- `sum by (source) (increase(stellarindex_source_unknown_symbols_total[25h])) > 0`
  sustained 30 min for one of `reflector`, `redstone`, `band`. (The
  Reflector decoder labels the counter with the generic `reflector`,
  not the per-variant `reflector-dex`/`-cex`/`-fx` — one decoder is
  shared across all three contracts.)
- `stellarindex_source_decode_errors_total` may ALSO rise for the same
  source, but only when an event produced no usable slot at all
  (`ErrEmptyPrices` / `ErrEmptyUpdates` / `ErrEmptyRates`). Since
  capture-totality an all-unknown batch no longer lands there, so a
  co-firing decode-error alert is now a *different* cause — see
  [decode-errors](ingestion.md#stellarindex_ingestion_decode_error).

**There is no log line to grep.** Verified at HEAD: none of the three
oracle decoders logs on the unmapped-symbol branch — they increment
the counter and emit the raw row, and that is all. The counter has no
`symbol` label either. Searching `journalctl` for `unknown symbol` /
`unknown feed_id` finds nothing and is not evidence the alert is
false. The symbol's identity comes from the **record layer**, below.
(The unrepresentable path is the exception: it *does* log — see the
sibling section.)

### Quick diagnosis (≤ 5 min)

1. **Which source, and is it still growing?** The indexer's metrics
   listener is `:9464` (`metrics_listen`); `:9100` on r1 is
   node_exporter and will return nothing for this counter.

   ```sh
   curl -s http://localhost:9464/metrics | grep source_unknown_symbols_total
   ```

2. **Which symbol?** Ask the record layer — the raw rows ARE the
   observation. Either read is fine; the SQL is authoritative (the API
   answer is cacheable and only covers the trailing 7 d).

   ```sh
   # No DB access needed. Rows with "mapped": false are the unmapped ones.
   curl -s 'http://localhost:3000/v1/oracle/streams?include_unmapped=true' \
     | python3 -c 'import sys,json;[print(r["source"], r["asset"], r["ts"]) for r in json.load(sys.stdin)["data"] if not r["mapped"]]'
   ```

   ```sql
   -- Authoritative, and it also sizes the gap. Widen the window if the
   -- counter has been climbing longer than a week.
   SELECT source, asset, count(*), min(ledger), max(ledger),
          min(ts) AS first_seen, max(ts) AS last_seen
     FROM oracle_updates
    WHERE asset LIKE 'raw:%'
      AND ts > now() - interval '7 days'
    GROUP BY 1, 2
    ORDER BY 3 DESC;
   ```

   `include_unmapped=true` is literal — `include_unmapped=1` does not
   opt in. The endpoint returns only ClassOracle sources, one row per
   `(source, asset, quote)`, latest observation in the trailing 7 d.

3. **Is it a symbol we WANT, and as what?** Reflector/Band symbols are
   `ScSymbol` tickers; RedStone feed_ids are `ScString` and may carry
   suffixes (`_FUNDAMENTAL`, `/USD`, `/EUR`) which are kept verbatim in
   the raw code. Decide the variant: fiat (ADR-0010), crypto
   (ADR-0014), RWA (ADR-0028). Many *different* symbols appearing on
   one event is a different problem — the oracle changed its schema;
   treat as a decoder regression ([decode-errors](ingestion.md#stellarindex_ingestion_decode_error)).

### Mitigation

Nothing to mitigate at runtime, and nothing is degrading while this is
open. The work is a mapping decision plus a code change, and it can
wait for a convenient moment.

- [ ] Add the symbol to the matching allow-list
      (`internal/canonical/asset_fiat.go` / `asset_crypto.go` /
      `asset_rwa.go`) with a one-line ADR amendment, or the feed_id →
      asset entry in `internal/sources/redstone/feeds.go`.
- [ ] Ship the indexer. The counter stops incrementing for that source
      and the alert clears once the 25 h window rolls off — it is
      `increase[25h]`, so expect it to stay open for up to a day after
      the fix. That is the window, not a regression.
- [ ] Promote the history already recorded under `raw:`. The oracle
      writer is `ON CONFLICT … DO UPDATE SET asset = EXCLUDED.asset …
      WHERE oracle_updates.derive_generation <= EXCLUDED.derive_generation`
      (`internal/storage/timescale/oracle.go`, migration 0109), so a
      re-derive rewrites `raw:X` → `crypto:X` on the same PK — no
      delete, no duplicate row. Every oracle source here is PROJECTED
      except `band`, so use the projected commands (see the replay
      decision rule in
      [docs/architecture/ingest-pipeline.md](../../architecture/ingest-pipeline.md#the-replay-decision-rule)):

      ```sh
      # rewind under ~1M ledgers — -config is REQUIRED
      stellarindex-ops projector-replay \
        -config /etc/stellarindex.toml -source <src> -from <first raw ledger>

      # bulk catch-up beyond that; defaults to dry-run, -write persists
      stellarindex-ops projected-rebuild \
        -config /etc/stellarindex.toml -source <src> \
        -from <first raw ledger> -write

      # band only — ContractCall-derived, NOT projected. Dry-run by default.
      stellarindex-ops ch-rebuild \
        -config /etc/stellarindex.toml -from <first> -to <last> \
        -contract-calls -sources band -write
      ```

      Run the bulk forms under `run-heavy-job.sh` on r1 and monitor to
      completion. One caveat on `projector-replay`: it rewinds the LIVE
      projector, which writes at `derive_generation` 0. That promotes
      rows the live indexer wrote (also generation 0), but it will NOT
      overwrite a row a previous re-derive stamped with a positive
      generation — use `projected-rebuild` in that case.
- [ ] Verification: `increase(stellarindex_source_unknown_symbols_total{source="<src>"}[1h]) == 0`,
      and the `asset LIKE 'raw:%'` count for that symbol is 0.

### stellarindex_ingestion_oracle_unrepresentable_symbols

One rung worse, and still a `ticket`.
`stellarindex_source_unknown_symbols_total` means the slot **was
written** as `raw:<symbol>`; a later registry entry promotes it in
place. `stellarindex_source_unrepresentable_symbols_total` means the
slot was **dropped with no row at all** — the published symbol /
feed_id fails even the permissive `raw:` validator
(`canonical.NewOracleRawAsset`: empty, > 64 bytes, or a byte outside
printable ASCII `0x21-0x7E`).

Only an ScString-keyed oracle can realistically reach it: RedStone
feed_ids are `ScString` (arbitrary bytes, unbounded length) while
Reflector / Band symbols are `ScSymbol`. The refusal is per-SLOT, not
per-event (#291) — `write_prices` batches every updated feed into one
event, so refusing the event would take all ~19 RedStone feeds dark.

Unlike the unknown case, this path **does** log, and the WARN line is
the only place the identity exists (slog escapes the bytes; they are
unvalidated relayer input):

```sh
curl -s http://localhost:9464/metrics | grep source_unrepresentable_symbols_total
journalctl -u stellarindex-indexer --since -1d | grep 'unrepresentable feed_id' | tail
```

Mitigation is the checklist above with two differences:

- The `raw:%` query finds **nothing** for this feed — there is no row
  to size the gap with. Size it from the WARN line's ledger range
  instead.
- The replay is mandatory, not an optimisation: no row exists to
  promote, so only a re-derive of the affected ledgers can land the
  missing prices.

If the feed_id is genuinely un-mappable (relayer garbage rather than a
real feed), the correct outcome is that the counter keeps rising and
the slot stays absent — record that decision on the ticket rather than
widening `canonical.validateRawSymbol`, which bounds what a buggy or
malicious relayer can make us persist.

### Root cause analysis

Only worth writing up for the `unrepresentable` case, or when a symbol
turns out to matter commercially.

- The symbol(s) and the first ledger they appeared at (from the raw
  rows, or the WARN line for the unrepresentable case).
- The oracle's own announcement (RedStone relayer changelog, Reflector
  asset list, Band symbol set) — was this an expansion we should have
  tracked?
- Row growth per source over the window
  (`SELECT source, count(*) FROM oracle_updates WHERE ts > now() - interval '1 day' GROUP BY 1`).

### Known false-positive patterns

- **Counter reset on indexer restart** — `increase()` handles resets;
  no false fire.
- **Alert lingering ≤ 25 h after the allow-list fix ships** — expected;
  the window is deliberately longer than Band's daily publish cadence so
  it cannot flap.
- **Band `USD` self-quote and zero-rate entries** are skipped by design
  and do NOT count here (they are contract-invariant skips, not mapping
  gaps).
- **"I grepped the logs and found nothing"** — not a false positive.
  The unmapped branch does not log at all; see Symptoms.

### Changelog

- 2026-08-28 — initial draft (oracle capture-totality PR-1; the counter
  had no alert consumer since F-1234).
- 2026-08-29 — cover the sibling
  `stellarindex_ingestion_oracle_unrepresentable_symbols` alert (#291):
  a RedStone `ScString` feed_id the raw validator refuses now drops
  ONE slot instead of blacking out the whole write_prices batch.
- 2026-09-08 — re-verified against HEAD. Step 1 told the operator to
  grep the indexer log for the unmapped symbol; **no decoder logs on
  that branch**, so the documented first step returned nothing and
  read as a false alarm. Replaced with the queryable observation
  (`/v1/oracle/streams?include_unmapped=true` and the `raw:%` rows).
  Metrics port corrected `:9100` → `:9464` (`:9100` is node_exporter).
  `projector-replay` example gained its required `-config`, and
  `ch-rebuild` its required `-config`/`-from`/`-to`/`-write`. Severity
  framing rewritten: `unknown_symbols` is `informational` on a quiet
  channel, not an incident; the sibling's `ticket` contrast made
  explicit. Dropped the pre-capture-totality "until the decoders record
  such slots" hedging — they do, at HEAD, in all three decoders.

## stellarindex_ingestion_uncorroborated_calls

_Source page `ingestion-events.md#stellarindex_ingestion_uncorroborated_calls`: status current, severity P3, last verified 2026-09-28._

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingestion_uncorroborated_calls` |
| Severity | P3 (`severity: ticket`) |
| Detected by | `deploy/monitoring/rules/ingestion.yml` and the R1 overlay `configs/prometheus/rules.r1/ingestion.yml` (group `stellarindex.ingestion`, `for: 0m`) |
| Typical MTTR | 15–60 min |
| Impact | An oracle-class `ContractCall` was declared in a transaction's auth tree but never executed, so the dispatcher (`internal/dispatcher/dispatcher.go`) refused it before `Decode` (W8.4a). Either a price-forgery attempt was rejected, or the legitimate routing shape changed and started refusing calls that should decode. |

### What this fires on

`stellarindex_source_uncorroborated_calls_total`, a per-source counter
incremented in `internal/dispatcher/dispatcher.go`'s `bumpUncorroborated`
whenever the auth-tree walk finds an oracle-class `ContractCallDecoder`
invocation declared but never corroborated by an executed call in the
same transaction — the defence W8.4a added against a forged auth entry
naming an oracle contract with fake price args.

`internal/dispatcher/statsflush/flusher.go` mirrors each source's delta
as a WARN log on every 5-minute flush window; this alert is the
Prometheus-side signal so a rejected forgery attempt doesn't depend on
someone tailing logs.

### Quick diagnosis (≤ 5 min)

```sh
# Which source(s) moved, and by how much?
ssh root@136.243.90.96 'curl -s localhost:9464/metrics | grep stellarindex_source_uncorroborated_calls_total'

# The exact tx is in the indexer's logs — statsflush's WARN fires the
# same window this alert does.
journalctl -u stellarindex-indexer --since -2h | grep "dispatcher: uncorroborated oracle calls"
```

- A single isolated increment on `band` (the only current oracle-class
  source) with no accompanying routing or deploy change → treat as a
  rejected forgery attempt. The dispatcher already refused it; no data
  was corrupted. Confirm by cross-referencing the tx hash from the WARN
  against the source ledger for anything else unusual in the same auth
  tree.
- A sustained climb correlated with a recent contract upgrade or a new
  legitimate call pattern → the routing shape changed and the
  corroboration check (W8.4a) is now false-positiving on real calls.
  Check the oracle contract's current WASM against the corroboration
  logic's assumptions.

### Mitigation (≤ 15 min)

- [ ] Step 1 — pull the WARN-logged tx hash(es) for the flush window and
      inspect the auth tree: does the declared call plausibly belong to
      an attacker, or does it look like normal traffic the corroboration
      check now misclassifies?
- [ ] Step 2 — forgery attempt: no mitigation needed, the call was
      already refused; file it for the security log and move on.
- [ ] Step 3 — routing-shape change: this is a code fix (adjust the
      corroboration check for the new legitimate shape), not an
      operational mitigation. Ship a dispatcher release once confirmed.

### Root cause analysis

For the postmortem, gather:
- The WARN-logged tx hash(es) and their full auth trees.
- Whether the oracle contract's WASM changed recently (ADR-0035: gate on
  contract identity, not topic alone — a WASM upgrade can change the
  shape corroboration expects).
- Whether the increment was isolated (one-off, consistent with a probed
  and rejected attack) or sustained (consistent with a routing change).

### Known false-positive patterns

- None yet. Steady state is zero; any nonzero increase needs eyes —
  either outcome (forgery or routing change) warrants review, so this
  alert does not distinguish them at fire time.

### Changelog

- 2026-09-28 — created (the counter existed with no metric, WARN, or
  alert consumer; added all three).

## Related

**stellarindex_ingestion_orphan_events**

- `ingestion.md#stellarindex_ingestion_source_stopped` — adjacent alert for the "no events at all" case.
- `ingestion.md#stellarindex_ingestion_decode_error` — different failure mode (events arrive but don't parse).
- `internal/sources/soroswap/consumer.go` — correlation buffer + age eviction.
- `internal/sources/phoenix/consumer.go` — same, for the 8-field fan-in.

**stellarindex_ingestion_oracle_unknown_symbols**

- Metric: `internal/obs/metrics.go` `SourceUnknownSymbolsTotal`;
  emitters `internal/sources/reflector/decode.go`,
  `internal/sources/redstone/decode.go`, `internal/sources/band/decode.go`.
- Metric: `internal/obs/metrics.go` `SourceUnrepresentableSymbolsTotal`;
  sole emitter `internal/sources/redstone/decode.go`
  (`noteUnrepresentableFeed`).
- Severity + delivery source of truth: [alerts-catalog.md](../alerts-catalog.md),
  including the informational delivery register.
- Design: `docs/design/oracle-capture-totality-design.md`; the
  `canonical.AssetOracleRaw` variant in `internal/canonical/asset_raw.go`.
- Companion runbook (whole-event decode failures, a *different* cause
  since capture-totality): [decode-errors](ingestion.md#stellarindex_ingestion_decode_error).
- Companion runbook (a stored row whose asset text will not parse on
  read): [oracle-stream-rows-unparsed](divergence.md#stellarindex_oracle_stream_rows_unparsed).
- Feed registries: ADR-0010 (fiat), ADR-0014 (crypto), ADR-0028 (RWA);
  `internal/sources/redstone/README.md` §RWA feeds.

**stellarindex_ingestion_uncorroborated_calls**

- `ingestion.md#stellarindex_ingestion_dispatcher_tx_skips` — the sibling dispatcher-level tripwire for
  whole-transaction skips; this one is per-source and oracle-specific.
- ADR-0035 (contract-identity gating) — why a shared topic across
  deployments can't be trusted alone, the same class of assumption
  W8.4a's corroboration check protects.
