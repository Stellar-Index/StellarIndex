---
title: Runbook — source-stopped
last_verified: 2026-08-29
status: draft
severity: P2
---

# Runbook — the `stellarindex_ingestion_source_stopped*` family

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts (three route here) | `stellarindex_ingestion_source_stopped` — 30 m rate / `for: 15m`, **high-volume allowlist only**<br>`stellarindex_ingestion_source_stopped_low_volume_dex` — 24 h rate / `for: 30m`<br>`stellarindex_ingestion_source_stopped_daily_publisher` — 30 h rate / `for: 1h` |
| Severity | P2 (`severity: ticket`) for all three |
| Detected by | `configs/prometheus/rules.r1/ingestion.yml` (the overlay r1 actually loads); multi-host template: `deploy/monitoring/rules/ingestion.yml`. Both trees carry the same exprs. |
| Typical MTTR | 15–60 min |
| Impact | One configured source has stopped producing events for longer than its own cadence budget. API clients querying that pair see price staleness creep up. If multiple sources stop, escalate to `all-ingestion-down.md` (P1). |

## Symptoms

**Read the alert NAME first — there is no single 30-minute window.**
F-1208 split the original universal-30m rule into three per-cadence
rules that share this runbook, because a rule tuned for Binance
false-positived every quiet afternoon on Phoenix and every day on
ECB. Each rule carries an explicit source allowlist; a source that
appears in none of the three is covered only by the fleet-level
`stellarindex_ingestion_all_sources_stopped`.

| Alert | Sources in its allowlist | Fires when | Sustain |
| ----- | ------------------------ | ---------- | ------- |
| `..._source_stopped` | `binance`, `bitstamp`, `coinbase`, `kraken`, `sdex`, `aquarius`, `reflector-dex`, `reflector-cex`, `reflector-fx`, `redstone`, `coingecko` | `rate(...[30m]) == 0` | 15 m |
| `..._source_stopped_low_volume_dex` | `comet`, `phoenix`, `soroswap`, `blend` | `rate(...[24h]) == 0` | 30 m |
| `..._source_stopped_daily_publisher` | `ecb`, `band` | `rate(...[30h]) == 0` | 1 h |

All three additionally require `stellarindex_source_enabled == 1`
joined `on (source)`, so a deliberately-disabled source stays quiet.

- Dashboard: *Ingestion → Events per source* panel shows a flat line for the offending source while other sources are still producing.

Total-outage coverage is the separate
`stellarindex_ingestion_all_sources_stopped` (5-minute rate,
`for: 3m`, P1/page) — that one stays tight because if no source at
all is emitting, something is broken across the whole
indexer/upstream surface.

## Quick diagnosis (≤ 5 min)

```sh
# Confirm which source: the alert label tells you, but dashboards
# sometimes drop the label on flat-line queries. :9464 is the
# indexer's metrics port and it is loopback-bound — query from r1.
ssh root@136.243.90.96 'curl -s http://localhost:9464/metrics | \
  grep -E "stellarindex_source_(events_total|enabled|last_event_unix)"'

# Health snapshot for every source's connection state:
stellarindex-ops list-cursors -config /etc/stellarindex.toml

# Is upstream the issue? r1 doesn't run its own stellar-rpc (removed
# 2026-04-23, see docs/operations/r1-deployment-state.md); point the
# probe at a public endpoint to confirm the network is closing
# ledgers and the source contract is still emitting events.
stellarindex-ops rpc-probe https://mainnet.sorobanrpc.com
```

Key signals:
- **Shared upstream failure**: on-chain and external sources both flatten at once. Jump to `all-ingestion-down.md`.
- **On-chain-only flattening**: inspect ledgerstream/indexer logs and current cursor movement for a dispatcher-path issue.
- **Per-source-only issue (others fine)**: the source's filter is rejecting everything, the source is legitimately idle, or a protocol change broke its decoder. Check `decode-errors` alert for correlation.

## Per-source cadence reference

Use this matrix BEFORE assuming a source is genuinely stopped —
several sources have natural cadences far longer than the
high-volume rule's 30-minute rate window, which is exactly why they
sit in the 24 h / 30 h tiers instead. The "expected idle cap" column
is the upper bound on a *normal* silent stretch (an operator
judgement, not the alert threshold); sustained idleness beyond it
warrants investigation, and the alert that actually fires is the one
whose allowlist names the source.

| Source | Expected cadence (active hours) | Expected idle cap (normal silence) | First-look-when-stopped |
| ------ | ------------------------------- | ---------------------------------- | ----------------------- |
| `sdex` (classic) | Continuous during US/EU trading; sparse off-hours | 30 min off-hours | Hubble cross-check via `stellarindex-ops hubble-check`; if Hubble shows trades we missed, decoder regression. |
| `soroswap` | Continuous during US/EU trading hours | 30 min off-hours | Soroban-RPC `getEvents` for the contract; if events flowing on-chain but not into us, decoder regression or cursor stuck. |
| `phoenix` | Low cadence; off-peak windows are common | 45 min off-peak | Same as soroswap. Phoenix's 8-event-per-swap shape (CLAUDE.md surprise) means partial decode-error storms can mimic source-stopped. |
| `comet` | Pool-activity-driven; sparse — one curated pool | 45 min normal; alerted by the 24 h low-volume-DEX rule | Since 2026-07-08 comet is contract-identity **gated** to a curated allowlist (`comet.MainnetGatedSet()`, today exactly the Blend BLND/USDC backstop; ADR-0035/0040, CS-026), so unrelated Balancer-v1 deploys no longer leak in — and silence now genuinely means that one pool is quiet. If a *legitimate* new pool has appeared, admit it with `stellarindex-ops seed-protocol-contracts -config /etc/stellarindex.toml -source comet`. |
| `aquarius` | Tied to AMM pool activity; sparse | 30 min | Soroban-RPC `getEvents`. |
| `blend` | Auction-driven; very sparse outside active markets | 90 min | Auctions don't run continuously — verify there's an active auction window before treating silence as a stop. |
| `band` | Relayer-push driven — **roughly daily**, not minutes (its rule's own annotation: "Band publishes on relayer push (also roughly daily)") | ~24 h normal; alerted by the 30 h daily-publisher rule | Band emits **zero events** (CLAUDE.md surprise) — observed via `InvokeContract` op args through the dispatcher's `ContractCallDecoder`. Verify the `ContractCallDecoder` is wired and the contract is still being relayed-to upstream. |
| `redstone` | Batch pushes every ~1 min during active periods | 10 min active / 30 min off-peak | Redstone's adapter event topic is `"REDSTONE"`; the body has no `feed_id` (lives in OpArgs). Verify the OpArgs plumbing is intact. |
| `reflector` (×3 contracts: DEX/CEX/FX) | Continuous on the active feed | 15 min DEX/CEX, 60 min FX (FX feed is much slower) | Reflector is **three separate contracts** — confirm WHICH one is silent. The DEX/CEX contracts are the most-watched; the FX contract's slower cadence makes it falsely-page-prone. **Upstream-relayer-stuck check**: if the contract is emitting fresh events on-chain (check the ClickHouse `contract_events` lake — the projector's default read source per ADR-0034 — for recent topic_0=REFLECTOR rows; the Postgres `soroban_events` landing zone is the legacy fallback, decommission-pending) BUT every row in `oracle_updates` has the same stale `ts` value, the issue is Reflector's relayer pushing the same `last_update_timestamp` payload — our decoder is correct, the data is genuinely stale upstream. Confirmed pattern on 2026-05-29 (24+ hours stuck at one ts). Cannot mitigate from our side; raise upstream via Reflector ops + flag the staleness publicly. |
| `binance`, `kraken`, `bitstamp`, `coinbase` (CEX WS streamers) | Continuous (sub-second) when open | 60 s gap = anomalous; 5 min = certainly broken | These are WebSocket streamers, not pollers — silence usually means the WS connection dropped silently. Check streamer-error metrics + reconnect logs. |
| `coingecko` (poller) | Default 60s interval | 5 min (one missed cycle plus cooldown headroom) | CG-specific cooldown semantics — see [`external-poller-error-rate-high.md` § Vendor-specific 429 patterns](external-poller-error-rate-high.md#vendor-specific-429-patterns). |
| `ecb` (FX dailies) | Once per business day ~16:00 CET | 24 hours weekdays / 72 hours weekends-and-holidays | ECB doesn't publish on EU bank holidays; cross-reference the silence date against the published TARGET2 closing days before treating as a stop. |

When the silent source is one of the off-peak-prone ones (Phoenix,
Comet, Aquarius, Blend, ECB) AND the silence is within the expected
idle cap, this is almost always a false positive — and the fix is
**not** to widen a window by hand. The three rules ARE the cadence
tiers: move the source between the allowlists in
`configs/prometheus/rules.r1/ingestion.yml` **and** the matching
`deploy/monitoring/rules/ingestion.yml` (the pair lint requires both
copies to stay in sync), then re-run `make monitoring-check`.
Aquarius is the one currently-awkward case — it sits in the
high-volume 30 m allowlist while its natural cadence is
pool-activity-driven; if it false-positives repeatedly, moving it to
the low-volume-DEX tier is the intended remedy, not a bespoke
fourth window.

## Mitigation

- [ ] Step 1 — restart the indexer if this is isolated to one or a few sources and the broader host/process is healthy. The indexer runs as `stellarindex-indexer.service`, templated by the `archival-node` ansible role (`templates/systemd/stellarindex-indexer.service.j2`); on r1 today there is exactly ONE such host. (The multi-host `indexer-0X` shape is ADR-0008's HA topology, which is not deployed.)
  ```sh
  ssh root@136.243.90.96 "systemctl restart stellarindex-indexer && \
    systemctl status stellarindex-indexer --no-pager | head -10"
  ```
- [ ] Step 2 — if events flow for 1-2 min post-restart then stop again: the source is probably legitimately idle, misconfigured, or affected by upstream schema drift. Compare its recent on-chain/off-chain activity to expectations before treating it as a dead connector.
- [ ] Step 3 — if decode-errors is also firing: the contract's event shape changed. Follow `decode-errors.md` Step 3 (update decoder + backfill).
- [ ] Verification: `rate(stellarindex_source_events_total{source=...}[5m]) > 0` within 2 min of mitigation.

## Known false-positive patterns

- **Low-volume sources during quiet windows**. Phoenix, Blend, Comet, Band, and ECB are precisely why the rule was split: they are NOT covered by the 30-minute rule at all. Phoenix's measured 30-day maximum swap gap is 8 h 28 m and a lake-verified 12 h lull is on record, which is why the low-volume-DEX tier sits at 24 h. A firing from `..._low_volume_dex` or `..._daily_publisher` therefore already means "well past this venue's normal quiet stretch" — treat it as real until the cadence matrix says otherwise, and if the venue's cadence has genuinely changed, move it between allowlists (see above) rather than stretching one.
- **Immediately post-deploy**. A restart briefly shows zero events while the source boots. Every tier's sustain (15 m / 30 m / 1 h) gives ample headroom for normal restarts including stellar-core catchup.

## Related

- `all-ingestion-down.md` — P1 escalation when multiple sources stop.
- `rpc-lag.md` — upstream root cause.
- `decode-errors.md` — adjacent failure mode that can masquerade as source-stopped if every event is being rejected.
- `cursor-stuck.md` — persistence-layer sibling (events flowing but cursor not advancing).

## Changelog

- 2026-04-23 — initial draft.
- 2026-04-30 — rpc-probe URL points at a public stellar-rpc; r1
  doesn't run its own (removed 2026-04-23).
- 2026-05-12 — alert window widened to 30m rate × 15m sustain to
  suppress false positives on low-volume sources (blend, band,
  ecb, comet, phoenix). F-1212b.
- 2026-08-29 — re-verified against HEAD (runbook re-verification
  wave K). The runbook still described ONE 30 m × 15 m alert; the
  shipped shape is the F-1208 three-way split (high-volume allowlist
  30 m/15 m, low-volume DEX 24 h/30 m, daily publisher 30 h/1 h),
  all three sharing this `runbook_url`. At-a-glance, Symptoms and the
  false-positive guidance rewritten around the family, and "extend
  the window" replaced with "move the source between allowlists in
  BOTH rule trees". Band's cadence was documented as "every 5-15 min"
  (it is roughly daily); comet's row predated the ADR-0035/0040
  contract-identity gate; the reflector row pointed at Postgres
  `soroban_events` rather than the ADR-0034 ClickHouse
  `contract_events` lake. Config path → `/etc/stellarindex.toml`,
  host/port shapes → r1's IP + `:9464`.
