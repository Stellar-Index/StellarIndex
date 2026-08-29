---
title: Runbook — sdex-gap-detected
last_verified: 2026-08-29
status: current
severity: P1
---

# Runbook: SDEX data-coverage gap detected

## At a glance

- **Severity:** P1 — pages on-call (`severity: page`)
- **Trigger:** `max by (source) (stellarindex_ingest_gap_max_size_ledgers) > 1000` with `source="sdex"`, sustained 15 min (the generic `stellarindex_ingest_gap_detected` alert firing with the sdex label — there is no sdex-specific rule)
- **Detected by:** `configs/prometheus/rules.r1/ingestion.yml` (group `stellarindex.ingestion`, `severity: page`, `for: 15m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/ingestion.yml`
- **Time to act:** within 30 min
- **Owner:** stellarindex on-call
- **TL;DR fix:** confirm writer health → `stellarindex-ops backfill -config /etc/stellarindex.toml -from $GAP_START -to $GAP_END -source sdex -dry-run`, then re-run with `-resume` (on r1, under `/usr/local/sbin/run-heavy-job.sh`)

This is the SDEX-specific surface of [ingest-gap-detected](ingest-gap-detected.md). SDEX is classic-DEX and does NOT flow through `soroban_events`; its rows land in the unified `trades` hypertable filtered by `source = 'sdex'`. Symmetric to the Soroban path, an SDEX-side cascade (Postgres back-pressure halting the SDEX writer goroutine while the rest of ingest stays healthy) used to be invisible at the data layer. This alert closes that gap.

**Sibling target:** the gap detector registers a separate `sdex-offers` target over the `sdex_offer_events` hypertable (OfferCreated/OfferUpdated/OfferRemoved). An offer-events writer halt does NOT show in this trades gauge — it fires the same alert with `source="sdex-offers"`.

**Scan-window caveat (applies to both the gauge and this alert):** the detector scans only a trailing window below tip — `GapDetectorSafetyLookback` = 200,000 ledgers steady-state, `GapDetectorFirstScanCap` = 2,000,000 on a target's first-ever scan (`internal/storage/timescale/gap_detector.go`). A gap deeper in history than the scan window never appears in the gauge; deep-history assurance belongs to the ADR-0033 completeness verdict, and full-range diagnosis to `find-data-gaps -from/-to`.

## Triage (5 min)

1. **Confirm the signal.** Two quick checks:
   ```
   ssh root@136.243.90.96
   curl -s localhost:9465/metrics | grep 'stellarindex_ingest_gap.*sdex'
   stellarindex-ops find-data-gaps -config /etc/stellarindex.toml -source sdex
   ```
   Both should agree on the gap inventory WITHIN the detector's trailing scan window (see caveat above): the gauge covers only that window, while `find-data-gaps -from N -to M` is the full-range tool.

2. **Is the SDEX writer alive?**
   ```
   ssh root@136.243.90.96 'journalctl -u stellarindex-indexer --since "30 min ago" | grep -E "sdex|source=sdex" | tail -50'
   ```
   Healthy steady-state writes are SILENT — the sink logs only failures and recovery, so an absence of sdex lines is normal. Liveness comes from the metrics instead:
   ```
   curl -s localhost:9464/metrics | grep 'stellarindex_source_last_insert_unix{source="sdex"}'
   curl -s localhost:9464/metrics | grep 'stellarindex_source_events_total{source="sdex"'
   ```
   `last_insert_unix` climbing + `events_total` rising = writer healthy. Both frozen = goroutine wedged or the source is halted.

3. **Is the gap forming or static?**
   ```
   curl -s localhost:9465/metrics | grep 'stellarindex_ingest_gap_max_size_ledgers{source="sdex"}'
   sleep 60
   curl -s localhost:9465/metrics | grep 'stellarindex_ingest_gap_max_size_ledgers{source="sdex"}'
   ```
   Same value = static (incident already over; backfill needed). Growing = active outage (the writer goroutine is still down). Note the gauge only refreshes on the detector's scan cadence — 6h for sdex (see Remediation) — so "same value" over one minute is expected either way; use the `last_insert_unix` liveness signal from step 2 as the live discriminator.

## Common shapes

- **Active writer halt (cascade-style).** The classic SDEX writer is paused; `stellarindex_source_last_insert_unix{source="sdex"}` is stale. Investigate the cascade root cause first (Redis MISCONF, Postgres pool exhaustion, disk pressure). Restarting the indexer without addressing the root cause will deadlock again within minutes.
- **Historic quiet windows do NOT fire this alert.** The sdex target carries a per-target `MinGapSizeOverride` of **1,000,000 ledgers** (`internal/storage/timescale/per_source_gaps.go` — the largest natural SDEX gap measured was 574,674 ledgers, so the generic 1K threshold would page constantly on historical data). Sub-1M gaps never report on this target. Combined with the trailing scan window (200K steady-state / 2M first-scan), this alert only catches large, recent holes — deep-history completeness is the ADR-0033 verdict's job.
- **Network outage during an upgrade.** Mainnet halts (e.g. a chain upgrade gone wrong) leave a real ledger gap but it's chain-wide, not SDEX-specific. The Soroban target should show a similarly-shaped gap. If only SDEX is short, it's an ingest-side issue.

## Remediation

Targeted SDEX backfill (re-decodes the range via the dispatcher). There is no `-parallel` flag, and `-config` is required — dry-run first to confirm scope, then re-run with `-resume`:

```
# On r1, wrap heavy one-shots in the mandatory memory-capped scope:
/usr/local/sbin/run-heavy-job.sh sdex-backfill \
  stellarindex-ops backfill \
    -config /etc/stellarindex.toml \
    -from $GAP_START -to $GAP_END \
    -source sdex \
    -dry-run

# Then drop -dry-run and commit:
/usr/local/sbin/run-heavy-job.sh sdex-backfill \
  stellarindex-ops backfill \
    -config /etc/stellarindex.toml \
    -from $GAP_START -to $GAP_END \
    -source sdex \
    -resume
```

Idempotent via the `trades` PK (`(source, ledger, tx_hash, op_index, ts)`). Re-runs over already-covered range are no-ops.

Verify the gauge decays on the next detector cycle — the sdex target's `ScanCadence` is **6 hours** (`per_source_gaps.go` override; it scans the 62M-row `trades` hypertable, so the generic 30-min cadence would pile concurrent scans on Postgres). Allow up to 6h for the gauge to refresh; a skipped/failed cycle retains the last-known-good value rather than zeroing:

```
curl -s localhost:9465/metrics | grep 'stellarindex_ingest_gap_max_size_ledgers{source="sdex"}'
# expect: 0 within one 6h cycle of the backfill completing
```

## Why no `sdex-backfill` subcommand?

There is no per-source `*-backfill` subcommand for any source — the whole `*-backfill` family (`cctp-backfill`, `soroswap-skim-backfill`, …) was **deleted** in rc.97 / ADR-0032 Phase 5. Soroban-derived sources catch up by rewinding the projector cursor (`projector-replay -source <name> -from <ledger>`), which re-projects from the ClickHouse `contract_events` lake by default (ADR-0034; the Postgres `soroban_events` landing zone is the legacy fallback source, decommission-pending #39) — no MinIO re-walk. SDEX has no equivalent landing zone — the classic-DEX ingest path writes straight to `trades` — so its repair re-decodes the raw range via the generic `backfill -source sdex` subcommand, which is the existing tool.

## Related

- [ingest-gap-detected.md](ingest-gap-detected.md) — the parent alert (matches any `source=` label; the `sdex-offers` sibling target for `sdex_offer_events` fires through the same rule)
- [projector-replay.md](projector-replay.md) — Soroban equivalent for the per-source projection tables (ADR-0032 supersedes the former `cascade-window-drain` subcommand)
- ADR-0030 — per-source coverage invariant; SDEX target is the canonical example of a non-Soroban source registered in the same scheme
- ADR-0033 — the completeness verdict that owns deep-history assurance beyond the detector's trailing scan window

## Changelog

- 2026-08-29 — first re-verification against HEAD: frontmatter added; fictional `--parallel 8` backfill replaced with the real flag set (`-config` required, dry-run → `-resume`, run-heavy-job.sh on r1); "30-min detector cycle" corrected to the sdex target's 6h `ScanCadence` (skipped cycles retain last-known-good); the "raise min-gap-size" follow-up bullet replaced with the shipped facts (per-target `MinGapSizeOverride` exists and sdex's is 1M ledgers; detector scans only a trailing window — 200K steady / 2M first-scan); "batch-write log line every ~5s" corrected (healthy writes are silent — use `stellarindex_source_last_insert_unix{source="sdex"}`); `last_insert_at` renamed to the real metric; projector-replay note updated to the ClickHouse-lake default (ADR-0034); duplicate Trigger line dropped; `sdex-offers` sibling target cross-referenced; dual-tree Detected-by. Status → current.
