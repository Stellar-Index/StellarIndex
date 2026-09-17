---
title: Runbook — stellarindex_ingestion_sink_undrained_rows
last_verified: 2026-09-18
status: active
severity: ticket
---

# Runbook — `stellarindex_ingestion_sink_undrained_rows`

## At a glance

| | |
|---|---|
| **Alert** | `stellarindex_ingestion_sink_undrained_rows` — `increase(stellarindex_sink_undrained_rows_total[1h]) > 0`, `for: 0m` |
| **Severity** | ticket |
| **What it means** | The Postgres pipeline sink (`sink="persist_events"`) was shut down with rows still buffered, its bounded drain could not land them before the budget expired, and the ledger cursor had ALREADY advanced past them. Those rows are a served-tier gap the indexer will never revisit on its own. `kind` says what was lost: `trade` rows or non-trade `event` rows. |
| **First action** | Pull the `abandoned on shutdown` ERROR lines from the indexer journal — they carry the exact ledger range (trades) or source (events) — then re-derive that range from the ClickHouse lake. |
| **Why ticket** | Nothing is on fire: live ingest resumed on restart and the rows are recoverable (ADR-0034). But the gap does not heal itself and no other alert names it — only the completeness verdict would, hours later. |

## Why this exists

The indexer upserts the per-ledger cursor when the producer has
ENQUEUED a ledger's events to the sink, before the sink writes them
(`cmd/stellarindex-indexer`, `internal/pipeline/sink.go`). On SIGTERM
the sink drains what is buffered under a budget derived from
`pipeline.ShutdownDeadline` (30 s): 20 s of bounded drain, a 5 s
best-effort final pass, 5 s reserved for the loss report, then `main`
hard-exits. A drain that trips that budget — Postgres slow (VACUUM,
lock convoy), restarting, or down during the deploy — abandons whatever
is left, and every abandoned row is a served-tier row the cursor says
was written.

Until 2026-09-18 that loss was visible only as ERROR log lines:
`trade batch abandoned on shutdown — recoverable from the CH lake
(ADR-0034); re-derive this ledger range` and `served-tier event
abandoned on shutdown — re-derive this source's tail`. Nothing alerted
on either, so a bad deploy lost rows silently. The ClickHouse live-sink
half of the same class already had
`stellarindex_ch_live_sink_ledgers_total{outcome="dropped"}` and the
[ch-live-sink-drops](ch-live-sink-drops.md) rules; this counter and
alert are the served-tier twin. The counter increments ONLY in the two
functions that emit those ERROR lines (`reportAbandonedTrades`,
`reportAbandonedEvent`), by row, so the alert's value is the size of
the gap and a steady-state flush that hands its rows to the shutdown
drain to retry is never counted.

**Scrape-window caveat.** The increment lands 20–25 s after SIGTERM and
the process exits by 30 s. r1 scrapes the indexer every 15 s, so a
single loss can fall between two scrapes and the alert stays silent.
The journal ERROR line is the authoritative record; the alert is the
machine-readable best-effort signal on top of it. After any deploy
during which Postgres was unhealthy, run the Quick diagnosis grep even
if nothing fired.

## Symptoms

- `stellarindex_ingestion_sink_undrained_rows{sink="persist_events",kind="trade"|"event"}` firing.
- Indexer journal, around the last restart, one or more of:
  - `trade batch abandoned on shutdown — recoverable from the CH lake (ADR-0034); re-derive this ledger range`
    with `phase=`, `batch_size=`, `ledger_from=`, `ledger_to=`;
  - `served-tier event abandoned on shutdown — re-derive this source's tail`
    with `kind=`, `source=`;
  - `PersistEvents drain deadline exceeded — made a final best-effort persist pass`
    with `undrained_events=`, `undrained_trades=`, `ledger_from=`, `ledger_to=`.
- Often alongside: [trade-insert-backpressure](trade-insert-backpressure.md)
  ticketing in the minutes before the deploy (`stellarindex_trade_insert_retries_total{outcome="retry"}`),
  or a Postgres restart in the deploy's own log.

## Quick diagnosis (≤ 5 min)

```sh
# The authoritative record: what was abandoned, and the range / source to re-derive.
ssh root@136.243.90.96 'journalctl -u stellarindex-indexer --since "-3h" --no-pager | grep -E "abandoned on shutdown|drain deadline exceeded"'

# What the counter saw (may be smaller than the journal — see the scrape-window caveat).
ssh root@136.243.90.96 'curl -s "http://localhost:9090/api/v1/query?query=increase(stellarindex_sink_undrained_rows_total%5B1h%5D)" | python3 -m json.tool | grep -E "\"sink\"|\"kind\"|value"'

# Why the drain tripped: was Postgres healthy across the restart?
ssh root@136.243.90.96 'journalctl -u postgresql -u stellarindex-indexer --since "-3h" --no-pager | grep -iE "shutting down|ready to accept|connection refused|infrastructure fault" | tail -40'
```

Write down, per ERROR line: `ledger_from`/`ledger_to` (trades) or
`source` (events), and the wall-clock time of the restart.

## Triage tree

1. **`kind="trade"`, ERROR line carries a non-zero ledger range** →
   on-chain trades (SDEX and Soroban DEXes). Recoverable from the lake:
   Resolution A.
2. **`kind="trade"`, `ledger_from=0`** → the abandoned batch held only
   external CEX/FX trades (they carry no ledger). No lake copy exists;
   Resolution C.
3. **`kind="event"`** → a non-trade served-tier write (oracle update,
   supply observation, blend / cctp / rozo row). The line names the
   `source`; Resolution B.
4. **Alert silent, but the journal shows the lines** → the scrape
   missed the increment (known caveat). Proceed exactly as if it fired.
5. **Alert firing, no ERROR line in the journal** → both are emitted by
   the same function, so this is a log-shipping / journald problem, not
   a phantom loss. Fix the log path, then read the range from the
   `PersistEvents drain deadline exceeded` line once it appears.

## Resolution

### A — on-chain trades: re-derive the ledger range from the lake

ADR-0034 keeps every raw operation in ClickHouse, so the served-tier
rows are rebuilt from it. `-sdex-gaps` restricts the pass to ledgers
the served tier is short on, which is exactly the shape a shutdown loss
leaves. Dry-run first (no `-write`), then write; on r1 run the write
under `/usr/local/sbin/run-heavy-job.sh`.

```sh
stellarindex-ops ch-rebuild -config /etc/stellarindex.toml \
  -from <ledger_from> -to <ledger_to> -sdex -sdex-gaps
stellarindex-ops ch-rebuild -config /etc/stellarindex.toml \
  -from <ledger_from> -to <ledger_to> -sdex -sdex-gaps -write
```

Soroban DEX trades (soroswap / aquarius / phoenix / comet) land through
the same command's default event pass, so the range covers them too.
Then confirm the served tier is whole over the range:

```sh
stellarindex-ops compute-completeness -config /etc/stellarindex.toml -ch -skip-recognition -source sdex -from <ledger_from>
```

[sdex-gap-detected](sdex-gap-detected.md) has the longer form if the
range is wide or the writer is still unhealthy.

### B — non-trade events: re-derive the source's tail

`consumer.Event` carries no ledger, so the window comes from the
restart: take `ledger_from`/`ledger_to` from the
`drain deadline exceeded` line in the same journal burst, or the
`ingestion_cursors` value at the restart minus a generous margin. Then
re-derive by source:

- **Oracle / ContractCall sources (band, soroswap-router):**
  `stellarindex-ops ch-rebuild -config /etc/stellarindex.toml -from <from> -to <to> -contract-calls -sources <source>`
  (dry-run, then `-write`), as in
  [oracle-unknown-symbols](oracle-unknown-symbols.md).
- **Event-based sources:** the same `ch-rebuild` without
  `-contract-calls`, scoped with `-sources <source>`.
- **Anything else the line names:** follow that source's own recovery
  in [insert-errors](insert-errors.md); the per-source gap detector
  ([ingest-gap-detected](ingest-gap-detected.md)) and the completeness
  verdict are how you confirm the tail is whole afterwards.

### C — external CEX/FX trades: record the loss

External trades have no ledger and no lake copy; the source poller
does not replay history. Note the venue and window in the incident
record. Served prices are unaffected beyond that window (the next poll
refills the feed), so no further action.

### D — stop it recurring

The drain tripped because Postgres could not absorb a few hundred
buffered rows in 20 s. Find out why before the next deploy: a
`VACUUM`/autovacuum on `trades`, a lock convoy
([pg-lock-convoy](pg-lock-convoy.md)), or the deploy stopping Postgres
before the indexer. If the answer is "the deploy order", fix
the playbook; if it is "Postgres was genuinely down", the loss was the
designed outcome (bounded drain over unbounded hang, ADR-0041) and the
re-derive above is the whole remedy.

## Known false-positive patterns

None. The counter increments only where a row has nowhere left to go.
A steady-state flush interrupted by shutdown carries its rows into the
bounded drain and is counted only if THAT also fails; a projector-owned
event the sink was never going to write is skipped before counting
(`TestDrainFinalPass_SkipInSinkExcludedFromReport`).

## Related

- [ch-live-sink-drops](ch-live-sink-drops.md) — the ClickHouse half of
  the same class. Different remedy: those drops are healed by the
  `ch-live-catchup` timer; this loss is healed by nothing but the
  re-derive above.
- [trade-insert-backpressure](trade-insert-backpressure.md) — the
  early signal that Postgres is unreachable; a deploy while it is
  firing is how this alert fires.
- [insert-errors](insert-errors.md), [sdex-gap-detected](sdex-gap-detected.md),
  [ingest-gap-detected](ingest-gap-detected.md), [cursor-stuck](cursor-stuck.md).
- `docs/adr/0034-tiered-clickhouse-architecture.md` — why the range is recoverable;
  `docs/adr/0041-ingest-durability-semantics.md` — why the drain is
  bounded rather than blocking.
- `internal/pipeline/trade_sink.go` (`reportAbandonedTrades`) and
  `internal/pipeline/sink.go` (`reportAbandonedEvent`) — the only two
  increment sites; `pipeline.ShutdownDeadline` — where the budget comes
  from.

## Changelog

- 2026-09-18 — created with `stellarindex_sink_undrained_rows_total` and
  the alert; served-tier twin of the `ch_live_sink` rules.
