---
title: Runbook — anomaly alerts
last_verified: 2026-10-05
status: draft
---
# Anomaly alerts

Runbooks for the aggregator anomaly and freeze alerts (policy: [ADR-0019](../../adr/0019-anomaly-response-and-confidence-scoring.md)). Rules: `configs/prometheus/rules.r1/anomaly.yml` (r1 overlay, the file r1 loads) and `deploy/monitoring/rules/anomaly.yml`.

## At a glance

- [`stellarindex_amm_self_pair_swap_burst`](#stellarindex_amm_self_pair_swap_burst)
- [`stellarindex_anomaly_freeze_engaged`](#stellarindex_anomaly_freeze_engaged)
- [`stellarindex_anomaly_freeze_recovery_stalled`](#stellarindex_anomaly_freeze_recovery_stalled)
- [`stellarindex_anomaly_warn_rate`](#stellarindex_anomaly_warn_rate)

## stellarindex_amm_self_pair_swap_burst

**Severity** P3 (ticket). **Trigger** `sum by (source) (increase(stellarindex_amm_self_pair_swap_total[15m])) > 10`, `for: 2m`. The counter is normally flat at zero; any climb is the signal (comet emitted zero before the exploit, which is why the threshold is low; escalate to page once the detector proves out). A self-pair swap (`token_in == token_out`) has no honest purpose; it is the primitive of the 2026-08-25 Blend/Comet exploit (~390 swaps walking a pool's spot price).

**Impact** None direct: the decoder drops self-pair rows (`(nil, nil)`) before serving, so they never reach `trades`. The freeze and divergence guards do not see them, so a sustained burst means someone is hammering a pool's internal price. Counter is incremented at the drop point in `internal/sources/comet/dispatcher_adapter.go` (`internal/obs/metrics.go`: `AMMSelfPairSwapTotal`).

**Diagnose** (detection alert, not an outage; goal is malicious vs benign):

```sh
# 1. Which source + how many, right now:
curl -s http://127.0.0.1:9090/api/v1/query \
  --data-urlencode 'query=increase(stellarindex_amm_self_pair_swap_total[15m])'

# 2. Find the offending tx(s) + signer in the raw landing zone
#    (self-pair swaps ARE persisted here even though they never become trades):
#    POOL/swap events on the curated pool contract within the burst window.
psql "$STELLARINDEX_POSTGRES_DSN" -c \
  "SELECT tx_hash, op_index, ledger, ledger_close_time
     FROM soroban_events
    WHERE contract_id = '<pool-contract>'
      AND ledger_close_time > now() - interval '30 min'
    ORDER BY ledger DESC LIMIT 50;"

# 3. Did the pool's spot price move over the window?
curl -s 'https://api.stellarindex.io/v1/price?base=<pool-base>&quote=<pool-quote>'
```

**Fix / decide**

- Confirm from the tx envelope that `token_in`/`token_out` really are the same (rule out a decode quirk).
- Malicious (coordinated burst walking the pool price): open a SEV, capture the tx set + signer, check whether any published price for the pool's assets moved (divergence board, freeze markers; see [stellarindex_anomaly_freeze_engaged](#stellarindex_anomaly_freeze_engaged)). The served price is protected by the substance + scam gates, but treat live manipulation as an incident.
- Benign (a pool operation not yet modelled that legitimately emits `token_in == token_out`): widen or silence the alert and file a decoder-modelling follow-up so the primitive is classified, not just counted. None are known today.
- Verification: `increase(...[15m])` falls back below 10 within ~15 min of the burst ending.

**Postmortem data** Full tx set from `soroban_events`, signer/source account, pool contract + constituent assets, spot-price series, whether freeze/divergence guards engaged.

**False positives** Backfill or completeness re-derive of the historical exploit window (ledger ~64112340) does NOT trip this: the counter increments only for LIVE self-pair swaps (`selfPairLiveWindow` in `dispatcher_adapter.go`). Re-ingesting a RECENT window (< 1h old) can double-count; rare and operator-initiated. If a benign pool event is found, model it in the decoder and raise/scope the threshold.

## stellarindex_anomaly_freeze_engaged

**Severity** P3 (ticket). **Trigger** `sum by (class) (rate(stellarindex_anomaly_freeze_engaged_total[5m])) > 0`, `for: 1m`. Escalation to operator-only pages separately as `stellarindex_anomaly_freeze_escalated` (freeze-lifecycle.yml; see [anomaly-freeze-sustained](anomaly-freeze-sustained.md)). Related alerts: [stellarindex_anomaly_warn_rate](#stellarindex_anomaly_warn_rate) (precursor), [stellarindex_anomaly_freeze_recovery_stalled](#stellarindex_anomaly_freeze_recovery_stalled) (recovery side). Typical MTTR 5-30 min for a confirm/override decision; cold-baseline false-fires resolve with a config tune.

**Impact** The affected pair's `/v1/price` serves the last-known-good (LKG) VWAP with `flags.frozen: true`. `/v1/price/tip` and `/v1/observations` keep serving live data (per-surface policy, [ADR-0018](../../adr/0018-api-consistency-surfaces.md): closed-bucket honours the freeze, tip/observations do not). Nothing is deleted.

**What freeze means.** Two layers in the aggregator (`internal/aggregate/orchestrator`); either can emit `ActionFreeze`:

1. Phase 1, per-class thresholds (`[anomaly.thresholds.*]` TOML): bucket-to-bucket deviation above the class `freeze_pct` on a single-source bucket. Log: `anomaly freeze engaged` (WARN).
2. Phase 2, 3-signal AND (per-asset MAD baseline + multi-factor confidence): fires only when ALL of `confidence < 0.10`, `z_score > 5.0`, `source_count <= 1` (knobs `[anomaly.phase2]` `confidence_max_freeze` / `z_score_min_freeze` / `source_count_max_freeze`). Log: `phase2 freeze engaged` (INFO) with per-pair confidence, z-score, source count.

Phase 2 is the extreme corner (anomalous move on a single source with no consensus): it catches USTRY-shape oracle manipulation and should not fire on real market events (multi-source, `source_count > 1`). Cross-oracle agreement enters only through the confidence score (`cross_oracle` factor from the divergence worker); there is no separate cross-oracle freeze condition.

For allow-listed structurally single-venue targets (`[aggregate.composite_reference] targets`, default XLM/GBP + XLM/EUR) the reason string carries the current-bucket composite-reference verdict, e.g. `… sources=1 corroboration_basis=venue composite_refuted divergence_pct=48.9 composite_leg_sources={crypto:XLM/fiat:USD:3,fiat:USD/fiat:GBP:1}`.

- `composite_refuted`: the deep XLM/USD x USD/GBP composite did NOT move with the venue; lean venue-specific (manipulation or artifact).
- `composite_unavailable: <cause>`: reference could not be built (thin XLM/USD leg, leg venues disagreeing `leg_dispersion=…bps`, FX snap stale / not FX-class, leg not refreshed); the freeze fired on the venue's own print. Check `stellarindex_aggregator_composite_corroboration`, `..._composite_reference_leg_sources`, `..._composite_reference_leg_dispersion_bps`.
- Mid-hold, a resolved reference releases only within `release_band_pct` (2%) of the composite; a venue parked at +4% stays frozen and walks the ladder.
- A bucket the composite CORROBORATED is not frozen: it increments `stellarindex_aggregator_composite_freeze_suppressed_total` and logs `phase2 freeze suppressed`. `sources=` is always the real venue count.

On freeze the orchestrator: skips the pair's VWAP cache write (prior value keeps serving, TTL refreshed for the freeze lifetime); writes a `freeze:<asset>:<quote>` Redis marker carrying the ladder; INSERTs an open `freeze_events` row (durable mirror for the explorer `/anomalies` timeline, also holds the ladder); increments `stellarindex_anomaly_freeze_engaged_total{class}`. Marker TTL = remaining hold + `cachekeys.FreezeTTL` (5 min). The 5 min is only how long a freeze survives aggregator silence; the duration is the ladder. Every bucket refreshes it, including empty buckets and buckets under `min_usd_volume`.

**Lifecycle (ADR-0019).** Initial hold 30 min (10 min when no corroborating lens was consulted). Auto-release only once the hold is served AND two consecutive scored buckets read healthy with a lens agreeing with the new price. On release the orchestrator deletes the marker and retires the ladder; the recovery worker (60 s poll) stamps `recovered_at`. Each hold expiry without release extends 30 min, up to 4 times; an expiry on an unscorable bucket slides the hold without using an extension. After the fourth extension the freeze escalates (P1); an escalated freeze never auto-releases and is held until `stellarindex-ops freeze-unfreeze` (see Force-unfreeze below).

**Symptoms**

- `rate(stellarindex_anomaly_freeze_engaged_total[5m]) > 0` for some `class` (counter, not gauge; a sustained anomaly is a steady increment stream). Counter is class-labelled only; find the pair via Redis markers or the journal.
- `/v1/price` carries `flags.frozen: true`; `price` + `observed_at` stop advancing.
- `redis-cli --scan --pattern 'freeze:*'` lists the pair(s); `SELECT * FROM freeze_events WHERE recovered_at IS NULL` has open rows.

**Diagnose** (≤ 5 min). Key question: real market event or manipulation? Real events move every venue; manipulation typically hits one thin venue.

```sh
# 1) Which pairs are frozen, and on which phase/signals?
ssh root@136.243.90.96
redis-cli --scan --pattern 'freeze:*'
journalctl -u stellarindex-aggregator --since -1h | grep -E 'freeze engaged'
# Phase 2 lines carry confidence= z= sources= per pair.

# 2) What does the confidence decomposition say? (served on /v1/price)
curl -s "https://api.stellarindex.io/v1/price?asset=<base>&quote=<quote>" \
  | jq '.data | {price, confidence, confidence_factors, flags: .flags}'
# cross_oracle_checked=true + cross_oracle_agreement>=2 → external
# references corroborate OUR price → lean "real market event".
# cross_oracle_checked=false → we could NOT verify externally —
# never read that as agreement.

# 3) What do the cross-oracle references say right now?
curl -s "https://api.stellarindex.io/v1/divergence?limit=50" \
  | jq '.data.pairs[] | select(.asset_id=="<base>")'
# References: reflector-dex/cex/fx, redstone, band (on-chain) +
# coingecko, chainlink. status=firing references disagree with us.

# 4) Raw per-source observations — which venue moved?
curl -s "https://api.stellarindex.io/v1/observations?asset=<base>&quote=<quote>" | jq
```

| Cross-oracle references | Our raw observations | Probable cause | Action |
|---|---|---|---|
| >= 2 references corroborate the NEW price (`cross_oracle_agreement >= 2`, small `/v1/divergence` deltas) | Move visible across sources | Real market event | Override: widen/tune the class threshold (Phase 1) or `[anomaly.phase2]` knobs, restart aggregator. Annotate the incident. |
| References stick to the OLD price (divergence firing against us) | Move on one venue only, thin book | Manipulation | Confirm freeze (do nothing). LKG keeps serving; usually arbitrages out within minutes. |
| No reference covers the asset (`cross_oracle_checked=false`, no `/v1/divergence` rows) | Unclear | Cannot tell | Leave frozen; escalate to on-call lead. |
| Conflicting | n/a | Investigation needed | Leave frozen; dig into per-reference rows in `divergence_observations`. |

**Fix** (≤ 15 min)

1. Identify pair + phase + signals (above); capture journal lines and the `confidence_factors` JSON.
2. Decide.
   - Confirm (manipulation suspected): no action. The freeze holds, then auto-releases after two healthy buckets; if the ladder is exhausted it escalates and the P1 `_sustained` alert pages by design.
   - Override (legitimate event / mis-tune): the freeze is config-driven, fix the config, not the marker:
     ```sh
     # Phase 1 mis-tune: widen the class threshold or fix the asset's
     # class in [anomaly.classifications]; Phase 2 false-fire: raise
     # z_score_min_freeze or lower confidence_max_freeze in
     # [anomaly.phase2]. Codify in configs/ansible/ (vault for
     # secrets), then:
     sudo systemctl restart stellarindex-aggregator
     ```
     To clear the flag immediately rather than wait for the hold to lapse, use the operator command, NOT a raw `redis-cli DEL`:
     ```sh
     stellarindex-ops freeze-unfreeze -config /etc/stellarindex.toml \
       -asset '<asset>' -quote '<quote>' \
       -reason "phase-2 threshold retuned; false fire" \
       -write   # fail-closed: without -write this only previews the unfreeze
     ```
     Any r1 config change lands in `configs/ansible/` in the same PR; a hand-edited TOML will page Monday morning.
3. Cold-baseline false-fire (most common): if MANY pairs freeze at once with `writer_wired=false` or near-genesis baselines (backfill mid-run), the per-asset baseline is unstable, not the market. Wait for backfill/baseline maturity or raise `z_score_min_freeze`. No customer impact when no marker is written (`redis-cli --scan --pattern 'freeze:*'` empty while the counter climbs).

**Verification** `rate(stellarindex_anomaly_freeze_engaged_total[5m])` returns to 0; `stellarindex_anomaly_freeze_released_total` increments with `mode="auto"` (or `operator` after force-unfreeze); the `freeze:<asset>:<quote>` marker is deleted and `flags.frozen` disappears from `/v1/price`; the recovery worker closes the `freeze_events` row within ~60 s. `mode="lapsed"` is NOT a resolution: the aggregator stopped refreshing the freeze for its hold plus 5 min; investigate the aggregator.

**Postmortem data** Bucket-by-bucket `confidence`, `z_score`, `source_count` from the aggregator journal for ±1h; raw trades in the anomalous bucket(s) (`/v1/observations` + `trades` hypertable: price, volume, tx-hash); per-reference rows for the window (`SELECT * FROM divergence_observations WHERE asset_id=... AND observed_at BETWEEN ...`, `agreement_count` on the cached result); verdict (market event / manipulation / mis-tune) and deciding evidence; whether auto-clear worked or config change was needed; whether `[anomaly]` thresholds need retuning. If manipulation, update [oracle-manipulation-defense.md](../../architecture/oracle-manipulation-defense.md) "Known incidents".

**Known false-positive patterns**

- Cold baseline (Phase 2): sparse history (mid-backfill, young deployment) destabilises the MAD baseline; many pairs fire at once, often `writer_wired=false`. Tune or wait.
- Thin baseline (bootstrap cap): confidence is capped at 0.5 while the pair's 30-day baseline density is under 28.5 days-equivalent of 1-minute buckets (`confidence_factors.bootstrap_capped` true, `confidence_factors.baseline_age_days` < 28.5 on `/v1/price`, `stellarindex_aggregator_bootstrap_capped{pair}` = 1). Density, not calendar age. With single-source coverage it can cross the freeze corner on modest moves. Expected; leave frozen or reclassify.
- Source feed restarting: a connector restart can briefly drop a multi-source pair to single-source; a normal move can fire. The freeze still serves its initial hold (10 min with no corroborating lens) before two healthy buckets can release it.
- Asset class mis-classification (Phase 1): a stablecoin classified `crypto` gets 20/50% thresholds instead of 1/3% (or a governance token classed `stablecoin` freezes constantly). Audit `[anomaly.classifications]`.

**Force-unfreeze (the operator override).** Always available; for an escalated freeze it is the only thing that ends it.

```sh
# See what is actually open, with its ladder state:
stellarindex-ops freeze-unfreeze -config /etc/stellarindex.toml -list

# Rehearse (default is a fail-closed dry run; -dry-run is an explicit alias):
stellarindex-ops freeze-unfreeze -config /etc/stellarindex.toml \
  -asset native -quote fiat:USD -reason "..." -dry-run

# Do it (-reason is REQUIRED for a mutation; -write actually applies it;
# -actor defaults to your OS user):
stellarindex-ops freeze-unfreeze -config /etc/stellarindex.toml \
  -asset native -quote fiat:USD \
  -reason "oracle recovered, verified by hand against Reflector + Kraken" \
  -write
```

Order of effects: appends a `freeze.unfreeze` row to `audit_log` (actor + reason in `metadata`; if that fails nothing changes); writes a `freeze:override:<asset>:<quote>` tombstone (5 min TTL) so the release counts as `mode="operator"` not `lapsed`; clears the Redis marker (serving authority, price republishes immediately); stamps `recovered_at` on the open `freeze_events` row (what `/v1/anomalies` reads). Idempotent; reports which half failed. The `freeze_events` row looks the same as an automatic release; `audit_log` records who and why.

If the pair is still anomalous it re-freezes on its next bucket. Within 2 hours of the override the re-freeze resumes the ladder the override ended, so an escalated pair returns escalated and pages again (`stellarindex_anomaly_freeze_refired_after_override_total` increments); an override does not buy a fresh first hold.

`-list` STATE column: `live` (Redis marker present), `rehydrated` (marker gone, durable ladder still holding; aggregator will re-write the marker), `GONE` (neither holds it; row awaits the recovery worker), `err` (could not read marker or ladder; reason on stderr).

**Do NOT `redis-cli DEL` the marker.** Since the durable ladder (migration 0119) it no longer works: deleting the key leaves `freeze_events.recovered_at` NULL, so the aggregator reads the missing marker as "Redis lost the marker", rehydrates the ladder from the open row and re-writes the marker next tick. On an escalated freeze a bare DEL is permanently inert (looks done for one tick, then undoes itself); on a healthy mid-hold pair the freeze re-engages regardless of whether the condition still fires, because the hold, not the condition, keeps it alive.

**Code** `internal/aggregate/orchestrator/phase2_freeze.go` (3-signal AND), `internal/aggregate/anomaly/` (Phase 1), `internal/aggregate/confidence/`, `internal/aggregate/freeze/` (marker lookup + recovery worker), `internal/divergence/`. Related runbooks: [price-divergence.md](price-divergence.md) (often co-fires), [divergence-no-reference.md](divergence-no-reference.md) (cross-oracle checker dark; an unverifiable freeze decision is weaker evidence either way), [aggregator.md#stellarindex_aggregator_outlier_storm](aggregator.md#stellarindex_aggregator_outlier_storm) (one source diverging inside a multi-source pair), [anomaly-freeze-sustained](anomaly-freeze-sustained.md). Postmortems tagged `anomaly-freeze`: `docs/operations/postmortems/`.

## stellarindex_anomaly_warn_rate

**Severity** P3 (ticket). **Trigger** `sum by (class) (rate(stellarindex_anomaly_warn_total[15m])) > 0`, `for: 15m`. The Phase 1 checker emitted `ActionWarn` (deviation past `warn_pct`; at or past the class `freeze_pct` it also warns when more than one source corroborates) for asset class `{{ class }}`; the rule needs at least one warn in every trailing 15m window for 15 min, not literally continuous warns.

**Impact** None customer-facing: `ActionWarn` does not freeze or flag `/v1/price`. Meaning: either a real, building market move (may precede [stellarindex_anomaly_freeze_engaged](#stellarindex_anomaly_freeze_engaged)) or `warn_pct` is tuned too tight for the class's normal volatility.

**Diagnose** The counter is class-labelled only. Use the shared steps in [stellarindex_anomaly_freeze_engaged](#stellarindex_anomaly_freeze_engaged): journal for the pair, `/v1/price` confidence decomposition, `/v1/divergence`, `/v1/observations`, and the same real-event vs manipulation decision tree.

```sh
curl -s http://127.0.0.1:9090/api/v1/query \
  --data-urlencode 'query=sum by (class) (rate(stellarindex_anomaly_warn_total[15m]))'
ssh root@136.243.90.96 "journalctl -u stellarindex-aggregator --since -1h | grep -i anomaly"
```

**Fix**

- Real, building move: no action; watch for a freeze alert. If it freezes, follow the freeze_engaged section.
- Chronic noise on one class: `warn_pct` too tight; retune `[anomaly.thresholds.*]` for the class and check the asset's class in `[anomaly.classifications]` (a mis-classified asset warns constantly). Codify in `configs/ansible/`, then `sudo systemctl restart stellarindex-aggregator`.
- Many pairs at once during backfill or a young deployment: cold baseline; wait for maturity (see freeze_engaged false-positive patterns).

**Verification** `rate(stellarindex_anomaly_warn_total[15m])` returns to 0.

## stellarindex_anomaly_freeze_recovery_stalled

**Severity** P3 (`severity: ticket`). **Trigger** `stellarindex_anomaly_freeze_active > 0 and on() (sum(rate(stellarindex_anomaly_freeze_recovery_sweeps_total{outcome!="ok"}[15m])) > 0)`, `for: 2h`. Typical MTTR 5-15 min once the cause is found.

**Impact** Resolved freezes still appear "firing" in `freeze_events`; the explorer `/anomalies` timeline shows resolved incidents as ongoing. The API is unaffected: `flags.frozen` is driven by the Redis marker, not the durable mirror.

**What "stalled" means.** Two sides of the freeze pipeline (`internal/aggregate/freeze/lifecycle.go`):

1. Writer (`freeze.Writer`): on every `TransitionFired` writes a Redis marker and INSERTs a `freeze_events` row with `recovered_at = NULL`. Hold is 30 min (10 min with no corroborating lens at fire time); each unearned expiry extends 30 min, up to 4 extensions, then escalates to operator review. Marker TTL = remaining hold + `cachekeys.FreezeTTL` (5 min silence grace, not the freeze duration). The ladder is mirrored to `freeze_events` (`hold_until`, `extensions_used`, `escalated`, `corroborated`), so it survives losing Redis.
2. Recovery (`freeze.Recovery`, `internal/aggregate/freeze/recovery.go`): every 60 s (plus one sweep at startup) lists open `freeze_events` rows and checks whether the pair's Redis marker exists. A marker-gone row is closed (`recovered_at = now()`) only if the durable ladder no longer holds (`ladderStillHolds`, same `LadderStillLive` predicate as the writer); a row whose `hold_until` + grace is still in the future stays OPEN for the orchestrator to rehydrate (logged `held_by_ladder`). After a Redis flush, open rows legitimately outnumber markers; that is not a stall.

A genuine stall: `stellarindex_anomaly_freeze_engaged_total` keeps incrementing, `stellarindex_anomaly_freeze_recovered_total` flatlines, sweeps report non-`ok` outcomes, and open `freeze_events` rows pile up.

**Symptoms**

- The trigger expression above for >= 2 h (use the `rate()` form; a `max_over_time` counter read latches non-zero forever after one historical error).
- `SELECT count(*) FROM freeze_events WHERE recovered_at IS NULL` on r1 postgres is well above the plausibly live count (steady state: Redis markers + rows whose durable hold is still running).
- Aggregator logs `component=freeze-recovery` WARN lines: `list open freezes`, `MarkRecovered failed`, `Redis Get failed during recovery sweep`.

**Diagnose** (≤ 5 min)

```sh
# 1) Is the recovery goroutine running? The sweep counter increments
# every 60 s on EVERY outcome (including a no-op sweep with zero open
# rows). Sample twice, 2 min apart — increasing on ANY outcome label
# means the goroutine is alive.
ssh root@136.243.90.96 "curl -s http://localhost:9465/metrics | grep stellarindex_anomaly_freeze_recovery_sweeps_total"
# (Do NOT grep the logs for Debug "recovery sweep complete" lines: that
# line is Debug-level — invisible at the default info log level — and a
# sweep with zero open rows logs nothing at all. Absence of the line
# does not mean the goroutine is dead; do not restart a healthy
# aggregator on that evidence.)

# 2) Is it failing on the lister side (postgres) or the cache side
# (Redis)? Read the outcome labels from the same output:
# outcome="error"   → the ListOpen postgres query failed (whole sweep aborted)
# outcome="partial" → per-row failures: Redis GETs erroring, or the
#                     MarkRecovered postgres UPDATE failing for some rows

# 3) Confirm the open-row backlog directly, with the durable-ladder columns:
ssh root@136.243.90.96 "runuser -u postgres -- psql -d stellarindex -c \
  \"SELECT count(*), min(frozen_at),
          count(*) FILTER (WHERE hold_until > now()) AS holds_still_live,
          count(*) FILTER (WHERE escalated) AS escalated
     FROM freeze_events WHERE recovered_at IS NULL;\""
```

| `outcome="error"` rising | `outcome="partial"` rising | Probable cause | Action |
|---|---|---|---|
| Yes | No | Postgres lister query failing OR Redis transport broken | Check postgres logs + Redis health |
| No | Yes | Per-row `MarkRecovered` UPDATE or Redis GET failing | Check postgres logs for `freeze_events` UPDATE errors; check WARN lines in the aggregator log |
| No | No, backlog still growing | (a) Markers refreshed because the anomalies genuinely persist (verify: `ssh root@136.243.90.96 "redis-cli --scan --pattern 'freeze:*'"`), or (b) markers GONE but durable holds still live (`held_by_ladder`, post-Redis-flush; look for INFO "freeze marker missing but the durable hold is still live"). Neither is a stall. | (a) underlying-anomaly runbooks ([stellarindex_anomaly_freeze_engaged](#stellarindex_anomaly_freeze_engaged), [anomaly-freeze-sustained](anomaly-freeze-sustained.md)); (b) nothing, rows close when the freeze ends |
| No | No, backlog flat near 0 | False positive; worker healthy | Tune the alert threshold |

**Fix** (≤ 15 min)

1. Confirm whether the underlying freezes are real: if open rows match live Redis markers or live durable holds, the worker is right; go to [stellarindex_anomaly_freeze_engaged](#stellarindex_anomaly_freeze_engaged) and [anomaly-freeze-sustained](anomaly-freeze-sustained.md).
2. Postgres-side error: `journalctl -u postgresql@15-main --since "30 min ago" --no-pager`. Common causes: connection-pool exhaustion (the worker uses the aggregator's shared `*sql.DB`) or a long ANALYZE/VACUUM blocking the UPDATE. Restart the aggregator if the pool is wedged.
3. Redis-side error: `redis-cli ping` and `systemctl status redis-server`. If Redis is up but GETs still fail, suspect ACL changes (worker shares the freeze writer's client).
4. Manual close-out when specific rows must close and the worker stays broken. Prefer the typed operator path (clears the marker AND stamps the row, requires a recorded reason):

   ```sh
   stellarindex-ops freeze-unfreeze -config /etc/stellarindex.toml -list
   stellarindex-ops freeze-unfreeze -config /etc/stellarindex.toml \
     -asset <asset_id> -quote <quote_id> -reason "recovery worker down; verified marker gone by hand"
   ```

   A bulk SQL sweep is a last resort and MUST be gated on the durable hold. Closing a ladder-held row is destructive: `recovered_at IS NULL` is the exact predicate the rehydrate reads, so blanket-closing deletes the ladder's Redis-flush protection (an escalated freeze loses its "stays active until manual unfreeze" state) and records a normal recovery on `/v1/anomalies` for a freeze that never recovered. `freeze_events` is a hypertable on `frozen_at` (migrations/0018; compression is enabled but no `add_compression_policy` is scheduled) and `recovered_at`/`hold_until` are not the partition column, so bound `frozen_at` too (7 days is generous: a stalled incident is hours old, the ladder escalates after 4x30 min):

   ```sql
   -- Close only rows whose durable hold has demonstrably lapsed
   -- (10 min is comfortably past the 5-min marker/ladder grace), and
   -- only within the current incident's window (chunk exclusion on the
   -- frozen_at partition column; widen if this stall has
   -- genuinely run longer than 7 days).
   -- recovered_at_ledger stays NULL: the ledger is unknown at manual
   -- close time, and 0 would violate the CHECK constraint
   -- (migrations/0018:54 — recovered_at_ledger >= frozen_at_ledger).
   UPDATE freeze_events
      SET recovered_at = now(),
          recovered_at_ledger = NULL
    WHERE recovered_at IS NULL
      AND frozen_at > now() - interval '7 days'
      AND (hold_until IS NULL OR hold_until < now() - interval '10 minutes');
   ```

   (`hold_until IS NULL` rows predate migration 0119 and carry no durable ladder.)

**Verification** `stellarindex_anomaly_freeze_recovered_total` resumes climbing on the next sweep tick (within 60 s); the open-row count trends toward live Redis markers + live durable holds.

**Postmortem data** Stall duration and max open-row backlog; transport at fault (postgres, Redis, or goroutine not running); if not running, whether an aggregator restart missed wiring it (`freezeRecovery` block in `cmd/stellarindex-aggregator/main.go`); whether the explorer `/anomalies` timeline diverged from reality (customer impact). Code: `internal/aggregate/freeze/lifecycle.go` (ADR-0019 ladder), `recovery.go`, `migrations/0119_freeze_events_lifecycle.up.sql`.

## Related

- [freeze-lifecycle rules](../../../configs/prometheus/rules.r1/freeze-lifecycle.yml) link to the `stellarindex_anomaly_freeze_engaged` section.
