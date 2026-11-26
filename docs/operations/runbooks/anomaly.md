---
title: Runbook — anomaly alerts
last_verified: 2026-11-20
status: draft
---

# Anomaly alerts

## At a glance

Anomaly freeze and confidence alerts; each section below is one alert.

Aggregator anomaly and freeze alerts (policy: [ADR-0019](../../adr/0019-anomaly-response-and-confidence-scoring.md)). Rules: `configs/prometheus/rules.r1/anomaly.yml` (r1 overlay) and `deploy/monitoring/rules/anomaly.yml`. r1 is `ssh root@136.243.90.96`; aggregator metrics on `:9465`.

## stellarindex_amm_self_pair_swap_burst

P3 (ticket). Expr: `sum by (source) (increase(stellarindex_amm_self_pair_swap_total[15m])) > 10`, `for: 2m`. The counter is normally flat at zero. The decoder drops self-pair rows before serving, so nothing reaches `trades`; a sustained burst means someone is hammering a pool's internal price. Goal: malicious or benign.

### Check

```sh
curl -s http://127.0.0.1:9090/api/v1/query \
  --data-urlencode 'query=increase(stellarindex_amm_self_pair_swap_total[15m])'
PGDSN="$STELLARINDEX_POSTGRES_DSN" psql-dsn -c \
  "SELECT tx_hash, op_index, ledger, ledger_close_time
     FROM soroban_events
    WHERE contract_id = '<pool-contract>'
      AND ledger_close_time > now() - interval '30 min'
    ORDER BY ledger DESC LIMIT 50;"
curl -s 'https://api.stellarindex.io/v1/price?base=<pool-base>&quote=<pool-quote>'
```

Self-pair swaps are persisted in `soroban_events` even though they never become trades.

### Likely causes

- Coordinated burst walking a pool price (malicious).
- A pool operation not yet modelled that legitimately emits `token_in == token_out` (none known).
- Re-ingesting a recent window (under 1 h old) can double-count; backfill of the historical exploit window does not trip it.

### Fix

1. Confirm from the tx envelope that `token_in` and `token_out` really are equal (rule out a decode quirk).
2. Malicious: open a SEV, capture the tx set and signer, check whether any published price for the pool's assets moved (divergence board, freeze markers; [freeze_engaged](#stellarindex_anomaly_freeze_engaged)).
3. Benign: widen or silence the alert and file a decoder-modelling follow-up.
4. Verify `increase(...[15m])` falls below 10 within ~15 min of the burst ending.

Escalate: any moved published price.

## stellarindex_anomaly_freeze_engaged

P3 (ticket). Expr: `sum by (class) (rate(stellarindex_anomaly_freeze_engaged_total[5m])) > 0`, `for: 1m`. Escalation pages separately as `stellarindex_anomaly_freeze_escalated` ([freeze-lifecycle](freeze-lifecycle.md)). The pair's `/v1/price` serves the last-known-good VWAP with `flags.frozen: true`; `/v1/price/tip` and `/v1/observations` stay live ([ADR-0018](../../adr/0018-api-consistency-surfaces.md)). The counter is class-labelled only; find the pair via Redis markers or the journal.

Two layers can freeze (`internal/aggregate/orchestrator`): Phase 1, per-class bucket deviation above `freeze_pct` on a single-source bucket (log `anomaly freeze engaged`); Phase 2, `confidence < 0.10` AND `z_score > 5.0` AND `source_count <= 1` (`[anomaly.phase2]`; log `phase2 freeze engaged`). Initial hold 30 min (10 min with no corroborating lens); release needs the hold served plus two healthy buckets. For allow-listed single-venue targets (`[aggregate.composite_reference] targets`) the reason carries a composite verdict: `composite_refuted` leans venue-specific (manipulation or artifact); `composite_unavailable: <cause>` means the freeze fired on the venue's own print. A composite-corroborated bucket is not frozen (`stellarindex_aggregator_composite_freeze_suppressed_total`).

### Check

Key question: real market event or manipulation? Real events move every venue; manipulation hits one thin venue.

```sh
redis-cli --scan --pattern 'freeze:*'
journalctl -u stellarindex-aggregator --since -1h | grep -E 'freeze engaged'
curl -s "https://api.stellarindex.io/v1/price?asset=<base>&quote=<quote>" \
  | jq '.data | {price, confidence, confidence_factors, flags: .flags}'
curl -s "https://api.stellarindex.io/v1/divergence?limit=50" \
  | jq '.data.pairs[] | select(.asset_id=="<base>")'
curl -s "https://api.stellarindex.io/v1/observations?asset=<base>&quote=<quote>" | jq
```

Phase 2 lines carry `confidence= z= sources=`. `cross_oracle_checked=true` with `cross_oracle_agreement>=2` means external references corroborate our price (real event); `cross_oracle_checked=false` means we could not verify, never agreement. `status=firing` divergence rows disagree with us.

| References | Raw observations | Cause | Action |
|---|---|---|---|
| >= 2 corroborate the NEW price | Move across sources | Real market event | Widen the class threshold or `[anomaly.phase2]` knobs, restart aggregator |
| Stick to the OLD price | One thin venue | Manipulation | Do nothing; LKG keeps serving |
| No reference covers the asset | Unclear | Cannot tell | Leave frozen; escalate to on-call lead |
| Conflicting | n/a | Investigate | Leave frozen; read `divergence_observations` |

### Likely causes

- Real market event (multi-source) or single-venue manipulation (above).
- Cold baseline: many pairs fire at once mid-backfill or on a young deployment (`writer_wired=false`, near-genesis baselines).
- Thin baseline: confidence capped at 0.5 while the 30-day baseline has under 28.5 days of 1-minute buckets (`confidence_factors.bootstrap_capped`).
- Asset class mis-classified in `[anomaly.classifications]` (a stablecoin classed `crypto` gets 20/50% thresholds instead of 1/3%); or a connector restart briefly dropping a pair to one source.

### Fix

1. Identify pair, phase and signals; capture the journal lines and `confidence_factors`.
2. Manipulation suspected: no action. The freeze auto-releases after two healthy buckets; an exhausted ladder escalates and the P1 `_sustained` alert pages by design.
3. Legitimate event or mis-tune: fix the config, not the marker. Phase 1: widen the class threshold or fix the asset class; Phase 2: raise `z_score_min_freeze` or lower `confidence_max_freeze`. Codify in `configs/ansible/` (a hand-edited TOML will page), then:

```sh
sudo systemctl restart stellarindex-aggregator
```

4. To clear the flag now, use the operator command, never a raw `redis-cli DEL` (it leaves `freeze_events.recovered_at` NULL, so the aggregator rehydrates the ladder from the open row and re-writes the marker):

```sh
stellarindex-ops freeze-unfreeze -config /etc/stellarindex.toml -list
stellarindex-ops freeze-unfreeze -config /etc/stellarindex.toml \
  -asset native -quote fiat:USD -reason "..." -dry-run
stellarindex-ops freeze-unfreeze -config /etc/stellarindex.toml \
  -asset native -quote fiat:USD \
  -reason "oracle recovered, verified by hand against Reflector + Kraken" \
  -write
```

   Without `-write` it is a fail-closed dry run; `-reason` is required for a mutation. It audits a `freeze.unfreeze` row, writes a `freeze:override:<asset>:<quote>` tombstone (5 min TTL), and clears the marker. `-list` STATE: `live`, `rehydrated`, `GONE`, `err`. A still-anomalous pair re-freezes next bucket; within 2 h the re-freeze resumes the ladder (`stellarindex_anomaly_freeze_refired_after_override_total`).
5. Verify `rate(stellarindex_anomaly_freeze_engaged_total[5m])` returns to 0, `stellarindex_anomaly_freeze_released_total` increments, the `freeze:<asset>:<quote>` marker is gone and `flags.frozen` clears.

Escalate: capture per-bucket `confidence`, `z_score`, `source_count` for +/-1 h and raw trades from `/v1/observations`.

## stellarindex_anomaly_warn_rate

P3 (ticket). Expr: `sum by (class) (rate(stellarindex_anomaly_warn_total[15m])) > 0`, `for: 15m`. Phase 1 emitted `ActionWarn` (deviation past `warn_pct`) for the class. No customer impact: `/v1/price` is neither frozen nor flagged. Either a building move (may precede [freeze_engaged](#stellarindex_anomaly_freeze_engaged)) or `warn_pct` is too tight.

### Check

Use the freeze_engaged checks for the pair, plus:

```sh
curl -s http://127.0.0.1:9090/api/v1/query \
  --data-urlencode 'query=sum by (class) (rate(stellarindex_anomaly_warn_total[15m]))'
ssh root@136.243.90.96 "journalctl -u stellarindex-aggregator --since -1h | grep -i anomaly"
```

### Likely causes

- Real building move.
- Chronic noise: `warn_pct` too tight, or an asset in the wrong class.
- Cold baseline across many pairs during backfill.

### Fix

1. Real move: watch for a freeze.
2. Noise: retune `[anomaly.thresholds.*]` and check `[anomaly.classifications]`; codify in `configs/ansible/`, then `sudo systemctl restart stellarindex-aggregator`.
3. Cold baseline: wait for maturity. Verify `rate(stellarindex_anomaly_warn_total[15m])` returns to 0.

## stellarindex_anomaly_freeze_recovery_stalled

P3 (ticket). Expr: `stellarindex_anomaly_freeze_active > 0 and on() (sum(rate(stellarindex_anomaly_freeze_recovery_sweeps_total{outcome!="ok"}[15m])) > 0)`, `for: 2h`. Resolved freezes still show "firing" in `freeze_events` and the explorer `/anomalies` timeline; the API is unaffected (`flags.frozen` follows the Redis marker).

The recovery worker (`internal/aggregate/freeze/recovery.go`) sweeps every 60 s, lists open `freeze_events` rows and closes a row only when its Redis marker is gone and the durable ladder no longer holds. A real stall: `stellarindex_anomaly_freeze_engaged_total` climbs, `stellarindex_anomaly_freeze_recovered_total` is flat, sweeps report non-`ok`, open rows pile up. Aggregator WARN lines (`component=freeze-recovery`): `list open freezes`, `MarkRecovered failed`, `Redis Get failed during recovery sweep`.

### Check

The sweep counter increments every 60 s on EVERY outcome: sample twice 2 min apart; rising on any label means the goroutine is alive. Do not judge by Debug "recovery sweep complete" lines (invisible at the default level) and do not restart a healthy aggregator on that evidence.

```sh
ssh root@136.243.90.96 "curl -s http://localhost:9465/metrics | grep stellarindex_anomaly_freeze_recovery_sweeps_total"
ssh root@136.243.90.96 "runuser -u postgres -- psql -d stellarindex -c \
  \"SELECT count(*), min(frozen_at),
          count(*) FILTER (WHERE hold_until > now()) AS holds_still_live,
          count(*) FILTER (WHERE escalated) AS escalated
     FROM freeze_events WHERE recovered_at IS NULL;\""
```

`outcome="error"` rising: the ListOpen Postgres query or Redis transport failed (whole sweep aborted). `outcome="partial"` rising: per-row Redis GET or `MarkRecovered` UPDATE failures.

### Likely causes

- Postgres lister or UPDATE failing (pool exhaustion, long ANALYZE/VACUUM).
- Redis transport or ACL problem.
- Not a stall: markers refreshed because anomalies persist (`redis-cli --scan --pattern 'freeze:*'`), or markers gone but durable holds still live (INFO "freeze marker missing but the durable hold is still live").
- Backlog flat near 0: false positive.

### Fix

1. If open rows match live markers or live durable holds the worker is right: [freeze_engaged](#stellarindex_anomaly_freeze_engaged) and [freeze-lifecycle](freeze-lifecycle.md).
2. Postgres: `journalctl -u postgresql@15-main --since "30 min ago" --no-pager`; restart the aggregator if the shared pool is wedged.
3. Redis: `redis-cli ping` and `systemctl status redis-server`; if up but GETs fail, suspect ACL changes.
4. Manual close-out, preferring the typed operator path (clears the marker and stamps the row):

```sh
stellarindex-ops freeze-unfreeze -config /etc/stellarindex.toml -list
stellarindex-ops freeze-unfreeze -config /etc/stellarindex.toml \
  -asset <asset_id> -quote <quote_id> -reason "recovery worker down; verified marker gone by hand" -write
```

5. Bulk SQL is a last resort and MUST be gated on the durable hold: blanket-closing drops the ladder's Redis-flush protection. `recovered_at_ledger` stays NULL (0 violates the CHECK). `hold_until IS NULL` rows predate migration 0119:

```sql
BEGIN;
-- Preview: the rows the UPDATE will close. Stop if the count or the assets are not what you expect.
SELECT count(*) FROM freeze_events
 WHERE recovered_at IS NULL
   AND frozen_at > now() - interval '7 days'
   AND (hold_until IS NULL OR hold_until < now() - interval '10 minutes');

UPDATE freeze_events
   SET recovered_at = now(),
       recovered_at_ledger = NULL
 WHERE recovered_at IS NULL
   AND frozen_at > now() - interval '7 days'
   AND (hold_until IS NULL OR hold_until < now() - interval '10 minutes')
RETURNING asset_id, quote_id, frozen_at;
-- UPDATE n must equal the preview count. If it does not, ROLLBACK; otherwise COMMIT.
COMMIT;
```

Verify `stellarindex_anomaly_freeze_recovered_total` resumes climbing within 60 s.

Escalate: record stall duration, max backlog, and which transport failed.

## stellarindex_aggregator_composite_corroboration_unavailable

P3 (ticket). Expr: `stellarindex_aggregator_composite_corroboration{verdict="unavailable"} == 1`, `for: 6h`. No served price is wrong: the composite reference could not judge, so a phase-2 fire on `pair` freezes with no corroboration path (the direct print and the freeze serve as if the reference did not exist).

### Check

The cause is the `unavailable=<reason>` field of the aggregator INFO line `composite reference unavailable` (logged once per change of cause; also in `freeze_events.detail->>'reason'` after `composite_unavailable:`):

```sh
ssh root@136.243.90.96 "journalctl -u stellarindex-aggregator --since '6 hours ago' --no-pager | grep 'composite reference unavailable' | tail -5"
ssh root@136.243.90.96 "curl -s http://localhost:9465/metrics | grep -E 'stellarindex_aggregator_(quote_surface_bps|composite_reference_leg_(sources|dispersion_bps))'"
```

| `unavailable=` | Meaning |
|---|---|
| `leg_sources=N` | A priced leg had fewer than `min_leg_sources` venues. |
| `leg_dispersion=Xbps` | The leg's venues disagree by more than `leg_dispersion_bps`. |
| `leg_quote_surface=Xbps` | The leg's stablecoin-quoted prints (taken at par) and its real-USD prints disagree by more than `leg_dispersion_bps`: two different USDs. |
| `leg_quote_surface=uncomputable` | A surface VWAP could not be computed; the leg is refused, not skipped. |
| `leg_not_refreshed`, `fx_*`, `no_chain` | Leg not published this tick, FX snap missing/stale, or no triangulation configured. |

How the quote-surface check judges a leg: the window is cut into window/24 slots (minimum 1 minute: 5m→1m, 1h→2m, 24h→1h). A slot is eligible when it holds stablecoin prints and real-USD prints from at least two USD venues whose own VWAPs agree within `leg_dispersion_bps`, its USD base volume is at least 1/4 of its stablecoin base volume, and each side carries at least 10,000 quote-currency units per hour of slot length (a partial first or last slot must still clear the full-length floor; the floor is in quote units, so it is weaker on a weak-currency leg such as MXN). The aligned gap is the mean of every stablecoin-bearing slot's gap `(stablecoin VWAP − USD VWAP) / USD VWAP`, weighted by that slot's stablecoin base volume and divided by the window's total stablecoin base volume: the average disagreement of all the prints the par rewrite vouches for. An eligible slot is compared with its own USD VWAP; an ineligible one with the whole-window USD VWAP, so no print inside an ineligible slot can move its reading. The aligned gap `r` decides alone only when eligible slots hold a share `c` of at least 95% of the window's stablecoin volume AND it passes the guard band `|r| + (1 − c) × R̂ × (1 + |r|) ≤ leg_dispersion_bps`. `R̂ = max / min − 1` (a ratio; ×10,000 for bps) over the VWAP of every slot in the window on either surface: each slot's stablecoin VWAP and each slot's USD VWAP (all venues), eligible or not, so the ineligible slots a trend could hide in are inside it. Otherwise the leg is judged by the larger of `r` and the whole-window VWAP gap (the whole-window gap alone with no eligible slot), so thin evidence is never judged more leniently than the whole-window compare.

Why the band bounds a hidden de-peg: `r` counts every stablecoin slot in full; an ineligible slot `s` is compared with the whole-window USD VWAP `O`, so it reads `g_s = p_s(1 + d_s)/O − 1` where `p_s` is the true price and `d_s` the true de-peg there, and `d_s − g_s = (1 + d_s)(1 − p_s/O)`. `O` and every observed slot VWAP lie in `[min, max]`; if `p_s` does too, `|1 − p_s/O| ≤ R̂`, so the true proxy-weighted de-peg `D` satisfies `|D| ≤ |r| + (1 − c)(1 + d)R̂`, `d` the largest slot de-peg. The band then gives `|D| ≤ leg_dispersion_bps + ρ`, with residual `ρ = (1 − c)[R̂ × max(0, d − |r|) + (1 + d)(1 + R̂)E]`, where `E` is how far a USD-silent slot's true price lies outside the observed envelope (a slot with USD prints is inside it by construction). Worst case: a monotone trend with USD-silent slots at the leading edge, so `E` is the move across those slots: 15% a day with one silent hour gives `E ≈ 0.6%` and `ρ ≈ 3 bps` at `c = 95%`; with de-pegs up to 300 bps the `d` term adds at most `(1 − c)R̂ × 3% ≤ 2.3 bps`. Generated continuous trends (1–15%, 1–3 ineligible slots at either edge or mid-window, de-pegs to 300 bps) stayed within the bound, largest residual 2.8 bps. Eligible slots are assumed to price their stablecoin prints' instants (price motion inside one slot adds to `ρ`). Not covered: a true price spike inside a USD-silent slot, which no same-time evidence can separate from a de-peg.

The price of that conservatism: under a pure trend, ineligible slots read their distance from the window's USD VWAP, so past the band the whole-window gap joins the verdict (with the old trend artefact when volume shifts during the day), and the leg refuses outright when ineligible share × that distance exceeds `leg_dispersion_bps` (one ineligible end hour of 24 at uniform volume: a move of ~46% over the window). Over 697 rolling 24h windows of 30 days of hourly production trades the band never bound: false alarms 34 for the whole-window compare, 1 with or without the band.

The log field `leg_quote_surface` reads `<leg>:<signed bps>/aligned=<n>,cover=<p>%` (decided alone on `n` eligible slots holding `p`% of stablecoin volume) or `/whole_window(overlap=<n>,cover=<p>%)` (coverage under 95% or the reading outside the guard band, so the larger of the whole-window and aligned gaps judged; a refusing reading is always outside the band, so a refusal reads `whole_window` even at full coverage). Coverage is a share of the stablecoin volume that was fetched: when the window's trade read was capped, it says nothing about prints beyond the cap. `stellarindex_aggregator_quote_surface_bps{pair,window,leg}` carries the same signed value: positive means stablecoin prints sit above real-USD prints. A refusal means that value exceeded `leg_dispersion_bps`: the stablecoin prints and same-time real-USD prints disagree, or the evidence was too thin to tell and the whole-window compare refused.

### Likely causes

- A real stablecoin de-peg: the gap persists in every window (5m, 1h, 24h) with the same sign, and the stablecoin's own market (`USDC/fiat:USD`, `USDT/fiat:USD` prices on `/v1/price`, or the issuer's redemption status) shows the same move.
- A trend artefact of the whole-window fallback: `whole_window(...)` on the 24h window during a strong move, while the 1h window's `quote_surface_bps` is inside the bound. More than 5% of stablecoin volume fell in ineligible slots (one USD venue only, USD venues disagreeing, USD volume under 1/4 of stablecoin volume, or either side under the floor), so the check could not rely on alignment alone. `cover=` says how much was eligible.
- Thin legs or dispersed venues: `leg_sources` / `leg_dispersion` causes; see the composite-reference config in `[aggregate.composite_reference]`.

### Fix

1. De-peg: no action on the aggregator; refusing the composite is the intended outcome. Track the stablecoin's peg and record it in the incident log.
2. Trend artefact on `whole_window`: no action; it clears as the window rolls past the thin hours, once eligible slots hold at least 95% of stablecoin volume. If it recurs on `aligned=` readings with a quiet 1h window, capture both windows' `quote_surface_bps` and the log lines and escalate as a defect in the surface check.
3. Do NOT raise `leg_dispersion_bps` or `tolerance_bps` to clear this alert: both also gate de-peg detection.

## Related

- [Alerts catalogue](../alerts-catalog.md)
