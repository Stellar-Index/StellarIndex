---
title: Runbook — oracle-unknown-symbols
last_verified: 2026-09-08
status: current
severity: P3
---

# Runbook — `stellarindex_ingestion_oracle_unknown_symbols`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingestion_oracle_unknown_symbols` — **and** its sibling `stellarindex_ingestion_oracle_unrepresentable_symbols`. Same shape, opposite meanings; see the next section before doing anything. |
| Severity | P3 both, but they are delivered differently on purpose. `unknown_symbols` is `severity: informational` → receiver `chat-informational` → Discord **#stellarindex-informational**, a deliberately low-traffic channel. `unrepresentable_symbols` is `severity: ticket` → receiver `chat-default` → Discord **#stellarindex-alerts**. Neither pages. |
| Detected by | Prometheus rule in `configs/prometheus/rules.r1/ingestion.yml` (the R1 overlay r1 actually loads); multi-host twin in `deploy/monitoring/rules/ingestion.yml`. |
| Typical MTTR | Not an outage clock. `unknown_symbols` earns a mapping *at leisure* — hours to days is fine, and the alert stays open meanwhile. `unrepresentable_symbols` is a registry entry plus a replay. |
| Impact | `unknown_symbols`: **nothing is lost.** An oracle published a symbol / feed_id the canonical allow-list does not map, and the slot was recorded verbatim as a `raw:<symbol>` row (`canonical.AssetOracleRaw`). The observation is record-layer-only until mapped: it is not a pair leg, not a VWAP input, and not visible on any keyed price surface. `unrepresentable_symbols`: the slot was **dropped with no row written** — that one IS a hole in the record. |

## The two alerts mean opposite things

Read the alertname before anything else.

| | `unknown_symbols` | `unrepresentable_symbols` |
| --- | --- | --- |
| What happened | Symbol/feed_id maps to no canonical asset | Symbol/feed_id fails even the permissive `raw:` validator |
| Row written | **Yes** — `raw:<symbol>` at the slot's own vector position | **No** — the slot is skipped |
| Recoverable later | Yes, in place: a registry entry plus a re-derive rewrites the same PK | No — only a replay can create the missing row |
| Severity | `informational` (quiet channel) | `ticket` |
| Emitters | `reflector`, `redstone`, `band` | `redstone` only, in practice |

Everything down to "Sibling alert" below is about `unknown_symbols`.

## Why `unknown_symbols` is informational, not a ticket

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

## Why this alert exists at all

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

## Symptoms

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
  [decode-errors](decode-errors.md).

**There is no log line to grep.** Verified at HEAD: none of the three
oracle decoders logs on the unmapped-symbol branch — they increment
the counter and emit the raw row, and that is all. The counter has no
`symbol` label either. Searching `journalctl` for `unknown symbol` /
`unknown feed_id` finds nothing and is not evidence the alert is
false. The symbol's identity comes from the **record layer**, below.
(The unrepresentable path is the exception: it *does* log — see the
sibling section.)

## Quick diagnosis (≤ 5 min)

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
   treat as a decoder regression ([decode-errors](decode-errors.md)).

## Mitigation

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

## Sibling alert — `stellarindex_ingestion_oracle_unrepresentable_symbols`

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

## Root cause analysis

Only worth writing up for the `unrepresentable` case, or when a symbol
turns out to matter commercially.

- The symbol(s) and the first ledger they appeared at (from the raw
  rows, or the WARN line for the unrepresentable case).
- The oracle's own announcement (RedStone relayer changelog, Reflector
  asset list, Band symbol set) — was this an expansion we should have
  tracked?
- Row growth per source over the window
  (`SELECT source, count(*) FROM oracle_updates WHERE ts > now() - interval '1 day' GROUP BY 1`).

## Known false-positive patterns

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

## Related

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
  since capture-totality): [decode-errors](decode-errors.md).
- Companion runbook (a stored row whose asset text will not parse on
  read): [oracle-stream-rows-unparsed](oracle-stream-rows-unparsed.md).
- Feed registries: ADR-0010 (fiat), ADR-0014 (crypto), ADR-0028 (RWA);
  `internal/sources/redstone/README.md` §RWA feeds.

## Changelog

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
