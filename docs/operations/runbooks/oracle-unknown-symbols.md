---
title: Runbook — oracle-unknown-symbols
last_verified: 2026-08-29
status: draft
severity: P3
---

# Runbook — `stellarindex_ingestion_oracle_unknown_symbols`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingestion_oracle_unknown_symbols`, `stellarindex_ingestion_oracle_unrepresentable_symbols` |
| Severity | P3 (ticket) |
| Detected by | Prometheus rule in `deploy/monitoring/rules/ingestion.yml` (and `configs/prometheus/rules.r1/ingestion.yml` R1 overlay) |
| Typical MTTR | Hours — the fix is an allow-list / feed-registry amendment plus a replay, not a restart |
| Impact | No serving-path breakage. An on-chain oracle (Reflector / RedStone / Band) is publishing a symbol or feed_id we cannot map to a canonical asset. Until the decoders record such slots verbatim (`raw:<symbol>`, `canonical.AssetOracleRaw`) the slot is **dropped** from `oracle_updates`; afterwards it is recorded but is invisible to every keyed price surface until mapped. Either way the gap widens on every event until acted on. |

## Why this exists

`stellarindex_source_unknown_symbols_total{source}` was added for
F-1234 (codex audit-2026-05-12) precisely so an oracle expanding its
feed set would not be silent. The 2026-08-04 cold audit found the
counter had **no consumer in either rule tree** — the metric's own
godoc claimed an alert in `external-pollers.yml` that never existed —
while r1 already carried `{source="reflector"} 7794`, i.e. 7,794 oracle
price slots dropped with nothing evaluating it. The 2026-07-24 RedStone
relayer expansion lost ~5,600 events the same way until
`internal/sources/redstone/feeds.go` caught up.

The oracle capture-totality design
(`docs/design/oracle-capture-totality-design.md`) makes the record
layer total: unmapped slots become `raw:` rows instead of being
dropped. This alert is the operator signal in both regimes — before the
decoders switch it means data loss; after, it means "a raw series exists
that the allow-list owner should promote".

## Symptoms

- `sum by (source) (increase(stellarindex_source_unknown_symbols_total[25h])) > 0`
  sustained 30 min for one of `reflector`, `redstone`, `band`.
- The indexer log carries a per-slot WARN from the decoder
  (`unknown symbol` / `unknown feed_id`).
- `stellarindex_source_decode_errors_total` may ALSO rise for the same
  source when a whole event is unmapped (`ErrEmptyPrices` /
  `ErrEmptyUpdates` / `ErrEmptyRates`) — that is the all-unknown
  variant of the same cause, not a second incident.

## Quick diagnosis (≤ 5 min)

1. **Which source, and is it still growing?**

   ```sh
   curl -s http://localhost:9100/metrics | grep source_unknown_symbols_total
   journalctl -u stellarindex-indexer --since -1d | grep -i 'unknown' | sort | uniq -c | sort -rn | head
   ```

   The log line names the actual symbol / feed_id. One symbol
   recurring on every event of the source = a mapping gap (real). Many
   different symbols on one event = the oracle changed its schema; treat
   as a decoder regression (see [decode-errors](decode-errors.md)).

2. **Is it a symbol we WANT?** Reflector/Band symbols are `ScSymbol`
   tickers; RedStone feed_ids are `ScString` and may carry suffixes
   (`_FUNDAMENTAL`, `/USD`, `/EUR`). Decide the variant: fiat
   (ADR-0010), crypto (ADR-0014), RWA (ADR-0028).

3. **Size the hole** (once `raw:` rows are being recorded):

   ```sql
   SELECT source, asset, count(*), min(ledger), max(ledger)
     FROM oracle_updates
    WHERE asset LIKE 'raw:%'
    GROUP BY 1, 2 ORDER BY 3 DESC;
   ```

## Mitigation (≤ 15 min)

There is no runtime mitigation — the fix is a code change:

- [ ] Add the code to the matching allow-list
      (`internal/canonical/asset_fiat.go` / `asset_crypto.go` /
      `asset_rwa.go`) with a one-line ADR amendment, or the feed_id →
      asset entry in `internal/sources/redstone/feeds.go`.
- [ ] Ship the indexer. The counter stops incrementing for that source
      and the alert clears after the 25 h window rolls off (it is
      `increase[25h]`, so expect it to stay red for up to a day after
      the fix — that is the window, not a regression).
- [ ] Replay the affected ledger range so the historical slots land
      (`stellarindex-ops ch-rebuild -sources <src> -from <first bad ledger> -to <tip>`;
      once `raw:` rows exist the gen-N re-derive promotes them in place
      on the same PK). Monitor to completion.
- [ ] Verification: `increase(stellarindex_source_unknown_symbols_total{source="<src>"}[1h]) == 0`
      and, post-replay, the `SELECT … WHERE asset LIKE 'raw:%'` count
      for that symbol is 0.

## Sibling alert — `stellarindex_ingestion_oracle_unrepresentable_symbols`

Same runbook, one rung worse. `stellarindex_source_unknown_symbols_total`
means the slot **was written** as `raw:<symbol>`; a later registry entry
promotes it in place. `stellarindex_source_unrepresentable_symbols_total`
means the slot was **dropped with no row at all** — the published
symbol / feed_id fails even the permissive `raw:` validator
(`canonical.NewOracleRawAsset`: empty, > 64 bytes, or a byte outside
printable ASCII `0x21-0x7E`).

Only an ScString-keyed oracle can realistically reach it: RedStone
feed_ids are `ScString` (arbitrary bytes, unbounded length) while
Reflector / Band symbols are `ScSymbol`. The refusal is per-SLOT, not
per-event (#291) — `write_prices` batches every updated feed into one
event, so refusing the event would take all ~19 RedStone feeds dark.

Diagnose from the WARN line, which carries the slot and the offending
bytes (slog escapes them; they are unvalidated relayer input):

```sh
curl -s http://localhost:9100/metrics | grep source_unrepresentable_symbols_total
journalctl -u stellarindex-indexer --since -1d | grep 'unrepresentable feed_id' | tail
```

Mitigation is the Mitigation checklist above with two differences:

- The `raw:%` query in step 3 of Quick diagnosis will find **nothing**
  for this feed — there is no row to size the hole with. Size it from
  the WARN-line ledger range instead.
- The replay is mandatory, not an optimisation: no row exists to
  promote, so only a re-derive of the affected ledgers can land the
  missing prices.

If the feed_id is genuinely un-mappable (relayer garbage rather than a
real feed), the correct outcome is that the counter keeps rising and
the slot stays absent — record that decision on the ticket rather than
widening `canonical.validateRawSymbol`, which bounds what a buggy or
malicious relayer can make us persist.

## Root cause analysis

- The offending symbol(s) from the indexer log and the first ledger
  they appeared at.
- The oracle's own announcement (RedStone relayer changelog, Reflector
  asset list, Band symbol set) — was this an expansion we should have
  tracked?
- Row growth per source over the incident window
  (`SELECT source, count(*) FROM oracle_updates WHERE ts > now() - interval '1 day' GROUP BY 1`).

## Known false-positive patterns

- **Counter reset on indexer restart** — `increase()` handles resets;
  no false fire.
- **Alert lingering ≤ 25 h after the allow-list fix ships** — expected;
  the window is deliberately longer than Band's daily publish cadence so
  it cannot flap. Silence for 25 h once the fix is deployed.
- **Band `USD` self-quote and zero-rate entries** are skipped by design
  and do NOT count here (they are contract-invariant skips, not mapping
  gaps).

## Related

- Metric: `internal/obs/metrics.go` `SourceUnknownSymbolsTotal`;
  emitters `internal/sources/reflector/decode.go`,
  `internal/sources/redstone/decode.go`, `internal/sources/band/decode.go`.
- Metric: `internal/obs/metrics.go` `SourceUnrepresentableSymbolsTotal`;
  sole emitter `internal/sources/redstone/decode.go`
  (`noteUnrepresentableFeed`).
- Design: `docs/design/oracle-capture-totality-design.md`; the
  `canonical.AssetOracleRaw` variant in `internal/canonical/asset_raw.go`.
- Companion runbook (whole-event decode failures, the all-unknown
  variant of the same cause): [decode-errors](decode-errors.md).
- Feed registries: ADR-0010 (fiat), ADR-0014 (crypto), ADR-0028 (RWA);
  `internal/sources/redstone/README.md` §RWA feeds.

## Changelog

- 2026-08-28 — initial draft (oracle capture-totality PR-1; the counter
  had no alert consumer since F-1234).
- 2026-08-29 — cover the sibling
  `stellarindex_ingestion_oracle_unrepresentable_symbols` alert (#291):
  a RedStone `ScString` feed_id the raw validator refuses now drops
  ONE slot instead of blacking out the whole write_prices batch.
