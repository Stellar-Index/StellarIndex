---
title: Runbook — sep41-supply-rollup-no-cursor
last_verified: 2026-09-24
status: draft
severity: P3
---

# Runbook — `stellarindex_sep41_supply_rollup_no_cursor`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_sep41_supply_rollup_no_cursor` |
| Severity | ticket |
| Detected by | Prometheus rule in `deploy/monitoring/rules/supply-refresh.yml` and `configs/prometheus/rules.r1/supply-refresh.yml` |
| Typical MTTR | 15 min once the projector is committing `sep41_supply` cycles |
| Impact | Served SEP-41 supply stays exact, but every supply read for the contract re-sums its whole `sep41_supply_events` history: the Postgres load that caused the 2026-07-06 incident. |

## Why this exists

The aggregator's rollup worker folds only rows the projector has durably
committed, and it reads that bound from `ingestion_cursors` row
`(source='projector', sub_source='sep41_supply')`. When that row is absent it
fails closed: it folds nothing and leaves `sep41_supply_rollup.last_ledger`
where it is (often 0). Correctness is preserved, but the fast
checkpoint + delta read path is not. Those passes are labelled
`outcome="no_cursor"` on `stellarindex_sep41_supply_rollup_advances_total` so
they cannot be mistaken for a dormant token's `noop`.

## Symptoms

- `stellarindex_sep41_supply_rollup_advances_total{outcome="no_cursor"}` climbs
  for the alerting `contract_id`, with no `ok` increments.
- The aggregator logs one WARN per contract when it enters the state:
  `sep41 supply rollup pinned: projector cursor for sep41_supply is absent`.
- Postgres IO and `stellarindex_aggregator_supply_refresh_duration_seconds`
  p99 may climb while nothing else looks wrong.

## Quick diagnosis (≤ 5 min)

```sh
# 1. Is the cursor row really missing?
psql -U stellarindex -d stellarindex -c \
  "SELECT source, sub_source, last_ledger, last_updated FROM ingestion_cursors WHERE source = 'projector' ORDER BY sub_source;"

# 2. Where is the fold pinned?
psql -U stellarindex -d stellarindex -c \
  "SELECT contract_id, last_ledger, updated_at FROM sep41_supply_rollup ORDER BY contract_id;"

# 3. Is the projector running and projecting sep41_supply?
sudo journalctl -u stellarindex-indexer --since "30 min ago" | grep -i 'projector' | grep -i 'sep41_supply' | tail -20
```

If step 1 shows a `sep41_supply` row, the alert should clear on the next
rollup pass. If it does not, check that the aggregator and the projector
point at the same database.

## Mitigation (≤ 15 min)

- [ ] Get the projector committing `sep41_supply` cycles. The projector
      registers `sep41_supply` only when the indexer's config has a
      non-empty `[supply] watched_sep41_contracts`. An aggregator that
      watches contracts the indexer does not causes this alert.
- [ ] If the projector is running but not advancing, follow
      [projector-wedged](projector-wedged.md) or
      [projector-row-quarantined](projector-row-quarantined.md).
- [ ] If `ingestion_cursors` was lost in a restore, do NOT insert a cursor
      row by hand. A hand-written cursor claims settlement the projector
      never evidenced, and the fold would then permanently exclude any row
      still missing below it. Re-run the projection from a known ledger
      with `stellarindex-ops projector-replay -source sep41_supply`
      ([projector-replay](projector-replay.md)).
- [ ] Verification: `outcome="ok"` (or `noop`) increments resume for the
      contract on the next rollup pass, and the alert resolves within 15 min.

## Related

- [supply-refresh-error-dominant](supply-refresh-error-dominant.md): the
  per-asset refresher that reads this rollup.
- [sep41-mint-recovery](../sep41-mint-recovery.md): rollup reset and re-fold
  procedure.
- Code: `AdvanceSEP41SupplyRollup` in
  `internal/storage/timescale/sep41_supply_events.go`;
  `runSEP41SupplyRollup` in `cmd/stellarindex-aggregator/main.go`.
