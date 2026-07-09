---
title: Runbook — dex-nonstandard-decimals
last_verified: 2026-07-09
status: ratified
severity: P2
---

# Runbook — `stellarindex_dex_nonstandard_decimals_detected`

## Why this exists

The served price is `Σ(quote_amount) / Σ(base_amount)` computed on **raw
smallest-unit integers** — both in the `prices_*` continuous aggregates
(`migrations/0002_create_price_aggregates.up.sql`) and in `aggregate.VWAP`
(`internal/aggregate/vwap.go`). The per-asset decimals **cancel in that
ratio only when the base and quote assets share the same decimals scale.**

Every DEX-traded Stellar token to date is 7-decimal (SACs are always 7;
classic credits are uniformly 7; the pure-SEP-41 tokens observed all declare
`decimals = 7`), so the ratio equals the true price and the served value is
correct. The decoders are correct too — they store faithful native-decimal
amounts (ADR-0003); the gap is purely the *absence* of a decimals
normalization on the read side.

The latent risk: the first non-7-decimal SEP-41 token to gain DEX liquidity
(an 18-decimal bridged asset, a 6-dp token, …) makes **every served price
for a pair involving it silently skew by `10^(7−decimals)`**, with no other
alarm. `internal/decimalsguard` sweeps recently-DEX-traded Soroban tokens,
resolves each one's on-chain `decimals()` from the certified lake, and raises
`stellarindex_dex_trade_nonstandard_decimals_total{source,asset}` the moment
one is confirmed `!= 7`. This alert turns that silent landmine into a loud
signal so the mispricing is caught **before** a customer consumes it.

**This stopped being latent on 2026-07-08.** Token
`CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO` declares
`decimals()=9` and traded on aquarius against USDC; the served price was
exactly 100x wrong (`41.32` vs true `~4132`) for 35 trades since
2026-06-22 before the guard confirmed it. That confirmed incident is why
this runbook now has a real stop-serving lever (2026-07-09) instead of
only a detector — see "Mitigation" below.

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_dex_nonstandard_decimals_detected` |
| Severity | P2 |
| Detected by | Prometheus rule in `deploy/monitoring/rules/aggregator.yml` (and `configs/prometheus/rules.r1/aggregator.yml`) |
| Typical MTTR | Automatic within ~60s of confirmation (the API-side serving guard); the full normalization remains a scheduled code change + deploy |
| Impact | A **real, live pair** has a leg with confirmed non-7 `decimals()`. As of 2026-07-09, `/v1/price`, `/v1/vwap`, `/v1/history`, `/v1/ohlc` DECLINE (422) any pair involving `{asset}` instead of serving the skewed value — see "Mitigation". Before that date (and for any deployment that hasn't run migration 0093 / doesn't wire `NonstandardDecimals`), those endpoints served the wrong price with no warning. |

## Symptoms

- The alert names a `source` (DEX connector) and `asset` (Soroban C-strkey
  contract id).
- The aggregator log (component `decimals-guard`) has an ERROR line with the
  exact `decimals` value and `price_skew_decades` (`|7 − decimals|`).
- The counter latches: once a token is detected it stays firing for the
  process lifetime (dedup is per `source`+`asset`).

## Quick diagnosis (≤ 5 min)

```sh
# 1. What decimals does the offending token declare? (the guard already
#    logged it; confirm from the lake directly)
journalctl -u stellarindex-aggregator | grep decimals-guard | tail

# 2. Which pairs are affected — every served pair with {asset} on either leg:
psql "$PG" -c "SELECT DISTINCT base_asset, quote_asset, source
               FROM trades
               WHERE (base_asset = '<asset>' OR quote_asset = '<asset>')
                 AND ts >= now() - interval '24 hours';"

# 3. How wrong is the served price? For a pair with a 7-dp counter-leg:
#    served = true_price * 10^(7 - decimals) when {asset} is the BASE leg,
#    served = true_price * 10^(decimals - 7) when {asset} is the QUOTE leg.
#    e.g. an 18-dp base token → served is 10^-11 of the true price.
```

If the token genuinely declares `decimals != 7`, this is a **true positive** —
the served price for those pairs is wrong. Proceed to mitigation.

## Mitigation (≤ 15 min)

Immediate (stop serving the wrong number) — **now mostly automatic**:

- [ ] **Nothing to do by default — including for a token that is already
      DORMANT.** The moment `internal/decimalsguard` confirms an offender,
      it upserts `nonstandard_decimals_assets` (migration 0093) via
      `UpsertNonstandardDecimalsAsset`. This now happens two ways: the
      periodic freshness sweep (20-minute trailing window, catches a token
      that is STILL trading) and — since 2026-07-09 — a one-time startup
      `Guard.Backfill` pass that scans a much longer historical window
      (default 90 days, `[decimals_guard].backfill_window_days`) exactly
      once when the aggregator process starts, so a token that traded and
      then went quiet is confirmed and upserted without waiting for it to
      trade again. The API's `NonstandardDecimalsCache` (`internal/api/v1`)
      refreshes that table every `NonstandardDecimalsRefreshInterval` (60s)
      and, from then on, `/v1/price`, `/v1/vwap`, `/v1/history`, `/v1/ohlc`
      DECLINE — `422 problem+json`, `Cache-Control: no-store` — any pair
      with `{asset}` on either leg (`declineIfNonstandardDecimals` in
      `internal/api/v1/nonstandard_decimals_guard.go`). A declined price is
      honest; a wrong price is not.
- [ ] Verification: confirm the row exists —
      `psql "$PG" -c "SELECT * FROM nonstandard_decimals_assets WHERE asset = '<asset>';"`
      — and confirm the API is actually declining —
      `curl -s "$API_BASE_URL/v1/price?asset=<asset>&quote=fiat:USD" | jq .status`
      should show `422` within ~60s of the row appearing. If the row exists
      but the API still serves 200: check whether this deployment's
      `cmd/stellarindex-api` binary predates the guard, or whether
      `NonstandardDecimals` failed to wire (see "Known false-positive
      patterns" — a nil cache fails OPEN, i.e. serves normally). If the row
      is missing for a token you can see traded in `trades` history, check
      the aggregator log for `decimals-guard: startup backfill complete` —
      its `scanned_pairs`/`confirmed_offenders` counts confirm the pass
      actually ran (only runs once per process start, not on every restart
      loop iteration).
- [ ] The alert stays latched (expected — it is a "this happened" signal,
      distinct from the API's live decline counter
      `stellarindex_price_serve_declined_nonstandard_decimals_total{asset}`,
      which tracks ongoing customer impact and tapers to zero once
      normalization ships).
- [ ] **Manual hand-seed — now a fallback, not the primary path.** Only
      needed when: the token's last trade is OLDER than
      `backfill_window_days` (default 90d, so the startup pass never scans
      back far enough), the aggregator hasn't restarted since this fix
      shipped, or ClickHouse was unreachable at the aggregator's last
      startup (the whole guard — sweep AND backfill — is best-effort and
      silently disabled without a lake connection). In any of those cases:
      `INSERT INTO nonstandard_decimals_assets (asset, decimals, source,
      confirmed_at) VALUES ('<asset>', <decimals>, '<source>', now());` —
      the API cache picks it up on its next refresh, no restart needed.
      This is the same row shape the guard writes; the table doesn't
      distinguish operator-inserted rows from guard-confirmed ones. A
      faster alternative to a manual INSERT: restart the aggregator — the
      startup backfill pass runs unconditionally on every boot and will
      self-seed any historically-traded offender within its window.

Durable (the real fix — schedule, do not hot-patch during the incident):

- [ ] Apply the **decimals normalization**: multiply the finalized ratio by
      `10^(dec_base − dec_quote)` at the read layer. See "Root cause analysis"
      for why this was deferred and what a correct fix must cover. Once
      normalization ships for `{asset}`, remove its row from
      `nonstandard_decimals_assets` (or simply stop re-confirming it) — the
      serving guard is self-clearing: the decline disappears within one
      cache refresh interval of the row going away.

## Root cause analysis

The forward normalization was deliberately **deferred** when this guard
shipped, because a *consistent* fix is not low-risk:

- There are **two** served-price paths that must agree: the materialized
  `prices_*` continuous aggregates (`/v1/history`, `/v1/ohlc`, the closed-1m
  point lookup behind `/v1/price`) and the query-time `aggregate.VWAP` path
  (`/v1/vwap`, `/v1/twap`, the aggregator's Redis-published VWAP).
- Normalizing only the `aggregate.VWAP` path is cheap (it recomputes from raw
  trades each call) but would make it **disagree** with the CAGG path for a
  non-7-dp token — trading one silent error for an inconsistency.
- Normalizing the CAGG path requires **rewriting a decade of materialized
  history** (the `prices_*` aggregates span back to 2015), which is not
  warranted for a latent risk and carries its own operational risk.

A correct fix therefore normalizes **both** read paths together, guarded so a
7-dp/7-dp pair (`10^0 = 1`) is byte-identical to today's behaviour and only
the non-7-dp case is scaled. Track this as the follow-up; this alert is the
forcing function that makes it real work rather than latent.

For the postmortem, gather: the offending contract id, its declared
`decimals()`, the list of affected pairs + their 24h volume, and how long the
skew was live before suppression.

## Known false-positive patterns

- **None expected.** The guard alarms only on a **confirmed** non-7
  `decimals()` (a successful lake read returning a value != 7). A resolution
  error or a token whose metadata isn't yet in the lake is treated as "cannot
  confirm" and never fires. If it fires, a token really does declare non-7
  decimals.
- A token could legitimately declare non-7 decimals and still be low-value /
  low-volume. Check step 2's volume before deciding urgency — but the served
  price is still wrong, so declining is correct regardless.
- **The API-side serving guard fails OPEN, never closed.** A nil
  `NonstandardDecimalsCache` (not wired in `cmd/stellarindex-api/main.go`), a
  cold cache (never refreshed yet), or a refresh error (Postgres blip —
  tracked by `stellarindex_nonstandard_decimals_cache_refresh_failures_total`,
  which retains the last-good snapshot rather than clearing it) all mean
  requests serve NORMALLY rather than declining. This is deliberate —
  availability wins over the guard for infra errors — but it means a
  confirmed offender can still serve briefly during an API-process restart
  before the initial cache refresh completes, or indefinitely on a
  deployment that predates migration 0093 / hasn't wired `NonstandardDecimals`.

## Related

- Detection: `internal/decimalsguard/guard.go` (the periodic sweep,
  `Guard.Sweep`, AND the one-time startup self-seed pass, `Guard.Backfill`
  — both share the same classify+report path), `internal/storage/timescale/soroban_dex_assets.go`
  (the shared time-bounded enumerator both call with different windows),
  `internal/storage/clickhouse/token_decimals_reader.go` (the resolver).
  Backfill's lookback window is config-surfaced:
  `internal/config.DecimalsGuardConfig.BackfillWindowDays` (`[decimals_guard]`
  in `configs/example.toml`), default 90 days
  (`decimalsguard.DefaultBackfillWindow`).
- Enforcement (2026-07-09): `internal/api/v1/nonstandard_decimals_guard.go`
  (`declineIfNonstandardDecimals`, the four call sites in `price.go` /
  `vwap.go` / `history.go` / `ohlc.go`), `internal/api/v1/nonstandard_decimals_cache.go`
  (`NonstandardDecimalsCache`), `internal/storage/timescale/nonstandard_decimals_assets.go`
  (`UpsertNonstandardDecimalsAsset` / `LoadNonstandardDecimalsAssets`),
  `migrations/0093_create_nonstandard_decimals_assets.up.sql`.
- Metrics: `stellarindex_dex_trade_nonstandard_decimals_total` (detection),
  `stellarindex_price_serve_declined_nonstandard_decimals_total` (live
  enforcement impact), `stellarindex_nonstandard_decimals_cache_refresh_failures_total`
  (cache infra health) — `docs/reference/metrics/README.md`.
- The correctness invariant it protects: ADR-0003 (i128/decimals discipline)
  and the "external-source amount scaling is NOT uniform" note in `CLAUDE.md`.
- Companion serving-sanity guard: `internal/pricingguard` (guards the raw
  closed-bucket `prices_1m` serving path against a different failure mode —
  gross single-bucket manipulation, not a per-asset decimals mismatch).

## Changelog

- 2026-07-09 — closed the DORMANT-token self-seed gap: the guard's periodic
  sweep only ever enumerated a 20-minute trailing window, so `CC2RB…` (see
  the previous entry) traded from 2026-06-22 but was never automatically
  confirmed — it wasn't trading at the moment the guard shipped and never
  traded inside a 20-minute window again, so it stayed unseeded until an
  operator hand-inserted the row. Added `Guard.Backfill`: a one-time pass,
  run once at aggregator startup before the periodic loop begins, that
  scans a much longer historical window (default 90 days, config-surfaced
  via `[decimals_guard].backfill_window_days`) through the SAME
  classify+report path the periodic sweep uses, and logs a
  `decimals-guard: startup backfill complete` summary
  (`scanned_pairs`/`confirmed_offenders`). The manual `INSERT` above is now
  a fallback for the residual case (a token dormant longer than the
  backfill window, or a process that hasn't restarted since this shipped),
  not the primary remediation path.
- 2026-07-09 — added the READ-TIME enforcement half after a confirmed
  production incident (token `CC2RB…`, 100x-wrong price, 35 trades since
  2026-06-22): `/v1/price`, `/v1/vwap`, `/v1/history`, `/v1/ohlc` now
  DECLINE (422) rather than serve a price for a pair with a confirmed
  non-7-decimals leg. Migration 0093 (`nonstandard_decimals_assets`),
  `internal/decimalsguard` now upserts on confirmation,
  `internal/api/v1.NonstandardDecimalsCache` mirrors it API-side. Self-clearing
  once the durable normalization ships and the row is removed.
- 2026-07-07 — initial draft alongside the decimals-guard (decoder-correctness
  audit Finding 2).
