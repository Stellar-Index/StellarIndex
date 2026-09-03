---
title: Diagnose a live incident
last_verified: 2026-09-03
status: living doc
---

# Diagnose a live incident
r1 access: `ssh root@136.243.90.96` (hostname doesn't resolve; login
root). SQL: file+scp then `sudo -u postgres psql -d stellarindex -f`
— NEVER inline `$$` over ssh (expands to shell PID, silently corrupts
the query; caused two wrong conclusions once). ClickHouse: HTTP
`curl http://127.0.0.1:8123/` on r1, not the native port over ssh.

## Rule zero (before alarming anyone)

1. **Check your own session first** — long sessions often caused the
   anomaly earlier and forgot (backfills, restarts, seeds).
2. **Widen the lens** — bursty/sparse sources look identical to
   "broken decoder" in a 24h window; check 7d before escalating.
3. **Match measurement windows** — Stellar Expert/CoinGecko totals
   are lifetime; our counters are often windowed. Confirm bases match
   before claiming under/over-count.
4. Every alert has a runbook at
   `docs/operations/runbooks/<alert>.md`; `alerts-catalog.md` is the
   index. Start there; this skill is the cross-cutting triage.

## Frozen / stuck ingest cursor

```sh
# FIRST hypothesis every time: is the next ledger object even in the bucket?
ssh root@136.243.90.96 'mc stat local/galexie-live/... <cursor+1 object>'   # frozen-cursor diagnostic
ssh root@136.243.90.96 'sudo -u postgres psql -d stellarindex -c "SELECT name, last_ledger, updated_at FROM ingest_cursors ORDER BY updated_at DESC LIMIT 5"'
```

- Cursor advancing but trades frozen while CEX fresh → the 2026-06-01
  class: cursor RESET on restart (compare cursor vs
  `max(trades.ledger)`; fast-forward via SQL restored it).
- Inserts crawling (~6/s) → chunk bloat: check chunk counts;
  `merge_chunks()` swept 3445→619 once (memory:
  trades-chunk-perf). Check `max_locks_per_transaction` before
  chunk surgery.
- Galexie itself stalled → check the captive-core subprocess +
  MinIO cred drift (`SignatureDoesNotMatch` = env file diverged from
  the MinIO user secret).

## Prices stale / wrong / diverging

Runbook chain: `price-divergence.md` + the aggregator-layer runbooks.
Layer order (check top-down):
1. **Served value vs truth** — `stellarindex-ops verify-served-values
   -api https://api.stellarindex.io` (supply/mcap class) and
   `/v1/divergence` (price class).
2. **Freshness** — `/v1/price` is structurally 30–150s behind tip
   (closed-bucket, ADR-0015/0018) — that's design, not an incident;
   the freshness watchdog textfile
   (`data_freshness.prom`) is the per-domain signal.
3. **Aggregator policy chain** — a pair going quiet: check
   `filterForVWAP` class gating (unknown source fail-closes to zero
   votes), the min-USD-volume gate, outlier filter (<3 trades =
   no-op), freeze state (`/v1/anomalies`).
4. **Reference outages** — since CS-087/088/089: `divergence_checked`
   flag on /v1/price, `no_reference` outcome, chainlink staleness
   rejects. CoinGecko free tier has been 429'd since 2026-06-19
   (operator: buy Pro).

## Completeness verdict red (`complete=false` / coverage < 1)

Read the snapshot before touching anything:
```sh
sudo -u postgres psql -d stellarindex -c "SELECT source, complete, coverage_pct, substrate_ok, recognition_ok, projection_ok, detail FROM completeness_snapshots ORDER BY computed_at DESC LIMIT 20"
```
- `substrate_ok=false` → lake gap/chain break: run ch-live-catchup,
  then re-verify. Check the LiveSink drop alert history.
- `recognition_ok=false` → a NEW topic shape (contract upgraded or a
  foreign emitter): `detail` names it; this is the
  schema-evolution/gating path (ADR-0040), not a backfill problem.
- `projection_ok=false` → per-ledger served≠lake (strict since
  CS-084): `detail` names the first mismatched ledger; catch-up is
  `projector-replay -source <name> -from <ledger>` — NEVER a bespoke
  backfill. Known benign class: oracle sources are aggregate-compared
  (keying vintages, see reconciliation_catalogue.go).
- Verdict simply STALE → the timer: `systemctl status
  compute-completeness.timer`; watermark-lag gauge (CS-090).

## Alert triage discipline

- Standing reds that are known-by-design are listed in the runbook's
  post-mortem notes — read before declaring new.
- After any deploy, suspect the deploy first (`git log` + the
  deploy workflow run) before infrastructure.
- 0–2s CI job failures with no steps = GitHub billing cap, not code
  — stop opening PRs and surface it.
- SEV flow: `docs/operations/sev-playbook.md`; post-mortems are
  append-only in the runbook.
