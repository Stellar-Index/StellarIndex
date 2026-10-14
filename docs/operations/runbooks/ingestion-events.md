---
title: Runbook — ingestion-events
last_verified: 2026-10-06
status: living
severity: P3
---

# Runbook — ingestion-events alerts

Ingestion event-quality alerts: orphan events, oracle symbols, uncorroborated calls. Rules: `configs/prometheus/rules.r1/ingestion.yml` (group `stellarindex.ingestion`; twin `deploy/monitoring/rules/ingestion.yml`). Indexer metrics port on r1 is `:9464` (`:9100` is node_exporter).

## At a glance

- [`stellarindex_ingestion_orphan_events`](#stellarindex_ingestion_orphan_events)
- [`stellarindex_ingestion_oracle_unknown_symbols`](#stellarindex_ingestion_oracle_unknown_symbols)
- [`stellarindex_ingestion_oracle_unrepresentable_symbols`](#stellarindex_ingestion_oracle_unrepresentable_symbols)
- [`stellarindex_ingestion_uncorroborated_calls`](#stellarindex_ingestion_uncorroborated_calls)

## stellarindex_ingestion_orphan_events

P3 ticket (`for: 15m`), MTTR hours-to-days. Symptom: `sum by (source) (rate(stellarindex_source_orphan_events_total[10m])) > 10/60` sustained 15 min; the label names the source. Individual swap / oracle updates are being lost; not urgent unless the rate reaches double-digit events/sec.

An orphan is, for **soroswap**, a `swap` without its matching `sync` (or vice versa) by `(ledger, tx_hash, op_index)`; for **phoenix**, an incomplete N-of-8 field set that aged past `defaultOrphanMaxAge` (5 min). aquarius and reflector cannot orphan. Swap and sync arrive adjacent in the in-order per-ledger stream, so an orphan means a decoder rejected one half or the contract's event shape shifted, not transport reordering.

Check:

```sh
# Which source is orphaning?
ssh root@136.243.90.96 'curl -s localhost:9464/metrics | grep stellarindex_source_orphan_events_total'

# Orphans are logged at debug level with the group key.
ssh root@136.243.90.96 "journalctl -u stellarindex-indexer -n 1000 --no-pager" \
  | grep -E "orphan|evicted" | tail -20

# Compare over the same window:
#   rate(stellarindex_source_events_total[10m])
#   rate(stellarindex_source_decode_errors_total[10m])
#   rate(stellarindex_source_orphan_events_total[10m])
```

Likely causes:

- Orphan rate rising, event rate flat: a decoder rejects one half of the pair (check decode_errors) or the contract upgraded in place.
- Both rising: a volume spike that falls outside the buffer window (phoenix's 8-event emission can span transactions; `defaultOrphanMaxAge` may be too short).

Fix (no live fix; do not restart or roll back on orphans alone):

1. Sample orphan group keys from the logs and look the tx up on a public RPC (r1 runs none; see [r1-deployment-state.md](../r1-deployment-state.md)); a NOT_FOUND means it left the retention window:

   ```sh
   curl -X POST https://mainnet.sorobanrpc.com \
     -H 'Content-Type: application/json' \
     -d '{"jsonrpc":"2.0","id":1,"method":"getTransaction","params":{"hash":"<tx>"}}'
   ```

2. Check the contract's event stream (stellar.expert or `getEvents`). A changed event shape shows as phoenix decode_errors AND soroswap orphans together.

Escalate: > 100/s sustained (treat as `source-stopped`), or orphan rate equal to event rate (correlation broken; revert the latest source-package change).

## stellarindex_ingestion_oracle_unknown_symbols

Covers `unknown_symbols` and its sibling `unrepresentable_symbols`; read the alert name first.

| | `unknown_symbols` | `unrepresentable_symbols` |
| --- | --- | --- |
| What happened | Symbol/feed_id maps to no canonical asset | Symbol/feed_id fails even the permissive `raw:` validator |
| Row written | Yes: `raw:<symbol>` at the slot's own position | No: the slot is skipped |
| Recoverable later | In place: registry entry plus re-derive rewrites the same PK | Only a replay creates the row |
| Severity / channel | `informational`, Discord #stellarindex-informational | `ticket`, #stellarindex-alerts |
| Emitters | `reflector`, `redstone`, `band` | `redstone` only, in practice |

Neither pages. `unknown_symbols` is not an incident: an oracle listing a new token is routine, nothing is lost (all three decoders write `raw:<symbol>`: `internal/sources/reflector/decode.go`, `band/decode.go`, `redstone/decode.go`), and it earns a mapping at leisure. The raw row is record-layer only (not a pair leg, not a VWAP input). Severity and delivery source of truth: [alerts-catalog.md](../alerts-catalog.md).

Symptom: `sum by (source) (increase(stellarindex_source_unknown_symbols_total[25h])) > 0` for 30 min on `reflector`, `redstone` or `band` (the Reflector decoder uses the generic `reflector` label). There is no log line to grep on the unmapped branch and the counter has no `symbol` label; the identity comes from the record layer. A co-firing decode-error alert is a different cause: [decode-errors](ingestion.md#stellarindex_ingestion_decode_error).

Check:

1. Which source, still growing?

   ```sh
   curl -s http://localhost:9464/metrics | grep source_unknown_symbols_total
   ```

2. Which symbol? Either read; the SQL is authoritative (the API covers the trailing 7 d and is cacheable):

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

   `include_unmapped=true` is literal (`=1` does not opt in).
3. Is it a symbol we want, and as what? Reflector/Band symbols are `ScSymbol` tickers; RedStone feed_ids are `ScString` and may carry suffixes (`_FUNDAMENTAL`, `/USD`) kept verbatim. Pick the variant: fiat (ADR-0010), crypto (ADR-0014), RWA (ADR-0028). Many different symbols on one event means a schema change: a decoder regression.

Fix (nothing degrades while open; no runtime mitigation):

1. Add the symbol to `internal/canonical/asset_fiat.go` / `asset_crypto.go` / `asset_rwa.go` with a one-line ADR amendment, or to the feed_id map in `internal/sources/redstone/feeds.go`.
2. Ship the indexer. The alert is `increase[25h]`, so it stays open up to a day after the fix.
3. Promote recorded history: the oracle writer's `ON CONFLICT ... DO UPDATE ... WHERE oracle_updates.derive_generation <= EXCLUDED.derive_generation` (`internal/storage/timescale/oracle.go`) lets a re-derive rewrite `raw:X` to `crypto:X` on the same PK. All oracle sources are PROJECTED except `band` (see the [replay decision rule](../../architecture/ingest-pipeline.md#the-replay-decision-rule)). Run bulk forms under `run-heavy-job.sh` on r1:

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

   `projector-replay` rewinds the LIVE projector (`derive_generation` 0): it will not overwrite a row an earlier re-derive stamped with a positive generation; use `projected-rebuild` then.
4. Verify `increase(stellarindex_source_unknown_symbols_total{source="<src>"}[1h]) == 0` and no `asset LIKE 'raw:%'` rows remain for that symbol.

False positives: none. Counter resets are handled by `increase()`; Band `USD` self-quote and zero-rate entries are skipped by design and not counted.

### stellarindex_ingestion_oracle_unrepresentable_symbols

Ticket. The slot was **dropped with no row**: the symbol/feed_id fails `canonical.NewOracleRawAsset` (empty, > 64 bytes, or a byte outside printable ASCII `0x21-0x7E`). Realistically only RedStone (`ScString` feed_ids). The refusal is per slot, not per event. This path does log, and the WARN line is the only place the identity exists:

```sh
curl -s http://localhost:9464/metrics | grep source_unrepresentable_symbols_total
journalctl -u stellarindex-indexer --since -1d | grep 'unrepresentable feed_id' | tail
```

Fix is the unknown-symbols checklist with two differences: the `raw:%` query finds nothing (size the gap from the WARN line's ledger range), and the replay is mandatory since no row exists to promote. If the feed_id is relayer garbage, leave the counter rising and record that decision on the ticket; do not widen `canonical.validateRawSymbol`.

## stellarindex_ingestion_uncorroborated_calls

P3 ticket (`for: 0m`), MTTR 15-60 min. Symptom: `stellarindex_source_uncorroborated_calls_total` moved. An oracle-class `ContractCall` was declared in a transaction's auth tree but never executed, so the dispatcher (`internal/dispatcher/dispatcher.go`, `bumpUncorroborated`) refused it before `Decode`. Either a forged auth entry was rejected, or a legitimate routing shape changed and real calls are being refused. Steady state is zero.

Check:

```sh
# Which source(s) moved, and by how much?
ssh root@136.243.90.96 'curl -s localhost:9464/metrics | grep stellarindex_source_uncorroborated_calls_total'

# The exact tx is in the indexer's logs (statsflush WARN, every 5-minute flush window).
journalctl -u stellarindex-indexer --since -2h | grep "dispatcher: uncorroborated oracle calls"
```

Likely causes:

- One isolated increment on `band` (the only oracle-class source) with no deploy or routing change: a rejected forgery attempt; no data was corrupted.
- A sustained climb after an oracle contract upgrade: the corroboration check now misclassifies real calls (ADR-0035: a WASM upgrade can change the expected shape).

Fix:

1. Pull the WARN-logged tx hashes and inspect each auth tree: attacker-shaped or normal traffic?
2. Forgery: nothing to do; file it in the security log.
3. Routing change: a code fix to the corroboration check, shipped as a dispatcher release.

## Related

- [ingestion.md#stellarindex_ingestion_source_stopped](ingestion.md#stellarindex_ingestion_source_stopped), [ingestion.md#stellarindex_ingestion_decode_error](ingestion.md#stellarindex_ingestion_decode_error), [ingestion.md#stellarindex_ingestion_dispatcher_tx_skips](ingestion.md#stellarindex_ingestion_dispatcher_tx_skips).
- [oracle-stream-rows-unparsed](divergence.md#stellarindex_oracle_stream_rows_unparsed): a stored row whose asset text will not parse on read.
- Code: `internal/sources/soroswap/consumer.go`, `internal/sources/phoenix/consumer.go`, `internal/obs/metrics.go` (`SourceUnknownSymbolsTotal`, `SourceUnrepresentableSymbolsTotal`), `internal/canonical/asset_raw.go`; design `docs/design/oracle-capture-totality-design.md`.
