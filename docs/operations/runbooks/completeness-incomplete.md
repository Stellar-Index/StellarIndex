---
title: Runbook — completeness verdict incomplete
last_verified: 2026-08-29
status: current
severity: P3
---

# Runbook — `stellarindex_completeness_incomplete`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_completeness_incomplete` |
| Severity | **P3** (ticket) |
| Detected by | `stellarindex_completeness_incomplete{source} == 1` for > 1h |
| Emitted by | `data-freshness.sh` from the latest `completeness_snapshots` row per source |
| Typical MTTR | minutes to verify; a re-derive can be longer (chunked) |
| Impact | The served tier no longer reconciles to the certified ClickHouse lake for that source (ADR-0033 `complete=false`) — a real served<>lake gap, i.e. served data is incomplete vs the proven substrate. |

## Symptoms

`stellarindex_completeness_incomplete{source="X"} = 1`. The ADR-0033 verdict
(`compute-completeness`, daily) found served counts ≠ the lake re-derive for
source `X`. Since the verdict is trustworthy + self-maintaining as of rc.149
(it preseeds factory children), this is a **real gap**, not a checker artifact.

## Quick diagnosis (≤ 5 min)

```sh
# The exact Δ + window the verdict recorded:
sudo -u postgres psql -d stellarindex -c \
 "SELECT source, complete, watermark_ledger, detail FROM (SELECT DISTINCT ON (source) * \
  FROM completeness_snapshots ORDER BY source, computed_at DESC) s WHERE NOT complete;"
# Re-run that source to confirm it persists (off the serving DB, -ch):
stellarindex-ops compute-completeness -config /etc/stellarindex.toml -ch -source <X> -from <recent>
```

## Mitigation (≤ 15 min to start)

Re-derive the flagged source from the certified lake, then re-verify:

- **Soroban projected sources** (`trades`/protocol tables):

  ```sh
  stellarindex-ops projector-replay -config /etc/stellarindex.toml \
    -source <X> -from <F> -write
  ```

  `-config`, `-source` and `-from` are all REQUIRED, and `-write` is required
  to actually rewind (dry run is the default). The source name is the
  projector registry's name, not the hyphenated table name.
- **Non-projected** (`sdex`, soroban-events):

  ```sh
  stellarindex-ops backfill -config /etc/stellarindex.toml \
    -source <X> -from <F> -to <T>
  ```

  or the CH re-derive (`ch-rebuild`, also `-config`-required).
- Then re-run `compute-completeness -ch -source <X>`; the gauge clears when it
  returns `complete=true`. Run chunked + off-peak (the SDEX/heavy re-derives
  blow ClickHouse's per-query memory limit over large windows).

## Root cause analysis

A served<>lake divergence: dropped rows (a decoder bug fixed forward-only, e.g.
the SEP-41 CAP-67 loss), a missed projection window, or a retention/PK artifact.
The `detail` column names the per-target Δ and window.

## Known false-positive patterns

- Pre-rc.149: factory-gated sources (blend) false-fired because the verdict's
  childgate wasn't self-seeded — **fixed**; if a NEW factory-gated source
  false-fires, confirm its creation events are reachable in `soroban_events`.
- **The `recognition` row is excluded on purpose and is NOT a source.**
  `data-freshness.sh` filters `source <> 'recognition'` out of this gauge: that
  row counts event shapes on contracts no source owns (the rest of the Soroban
  ecosystem, ~23k shapes growing ~30/day), so `complete=false` there is the
  permanent expected state of a curated indexer, not a served↔lake gap. Folding
  it in kept this ticket alert firing continuously from 2026-08-17. The
  unattributed census that row carries is exported separately as
  `stellarindex_recognition_unattributed_shapes` — shapes on contracts NOBODY
  owns, i.e. foreign protocols. It is large and permanently growing, and is
  deliberately NOT alerted on. The signal that means a real defect is
  per-source: `stellarindex_recognition_ok == 0`, meaning a protocol we DO
  index emitted something its decoders could not claim
  ([source-recognition-failing](source-recognition-failing.md)). Until
  2026-09-01 the alert watched the census and inferred the defect from its
  growth rate; it now reads the per-source axis directly.
- **A green verdict with a lagging watermark.** `complete=true` only speaks for
  the range that was walked; `stellarindex_completeness_watermark_lag_ledgers`
  (CS-090) is the companion gauge for "verified, but only up to an old ledger".

## Related

- `stellarindex_data_source_stale` — a source not ingesting at all
  ([data-source-stale](data-source-stale.md)).
- [ADR-0033](../../adr/0033-completeness-verification-model.md) — the
  completeness-verification model (there is no
  `docs/architecture/completeness-verification.md`).
- `docs/operations/launch-todo.md` Phase C.

## Changelog

- 2026-08-29 — re-verified against HEAD (runbook Wave L, #319): the
  "SEP-41 is EXCLUDED from the verdict" bullet is obsolete — P1-7 is DONE and
  `sep41_transfers`/`sep41_supply_events` have been promoted into the
  reconciliation catalogue whenever `[supply] watched_sep41_contracts` is
  configured since 2026-07-11; `docs/architecture/completeness-verification.md`
  does not exist → ADR-0033; both catch-up commands omitted the REQUIRED
  `-config` (and `projector-replay` also needs `-write`, or it is a dry run);
  added the recognition-census exclusion and the CS-090 watermark-lag note.
- 2026-06-30: created with the data-freshness watchdog.
