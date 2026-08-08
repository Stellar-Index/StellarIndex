# Page insight program — 2026-08-08

Operator directive: review every explorer page with the /accounts
mindset — lead with insight, visualize shape, stay sub-second — and make
each genuinely useful. This is the plan of record; items leave it when
deployed + validated (same discipline as the open-fixes inventory).

Design rules proven by the /accounts upgrade:
1. **Headline stats users would quote** ("top 100 accounts hold 87.75%")
   beat decorative counters. Every page leads with 3–6 of them.
2. **Distribution over listing** — the shape of a dataset (histogram,
   concentration, share) is the insight; the table is the appendix.
3. **Rollup-first**: every panel reads precomputed keyed tables
   (30-min cycle or incremental follower job) — sub-second by
   construction, honest `computed_at`, distinct warming state.
4. On-system components, with DELIBERATE chart variety (operator
   directive): DonutChart for composition (venue share, concentration),
   HBarList for magnitude comparisons, LineChart/Sparkline for time,
   stacked area for share-over-time (new primitive on LineChart's
   base), a freshness/intensity HEATMAP (promote the anomalies page's
   ReasonHeatmap into a shared primitive), and a TREEMAP primitive for
   share-of-whole across many entities (volume by asset, wealth by
   band). New primitives stay pure HTML/CSS/SVG per the design system.

## Infrastructure pillars (unlock most pages)

**P-A `network_daily` incremental rollup** — per-day: new accounts
(create_account count), txs, ops, ops-by-class (classic vs soroban),
contract events, fees paid, distinct source accounts ("active
accounts"). Incremental follower job (cap67 pattern): backfill once from
`stellar.operations`/`transactions`/`ledgers` (ledger-keyed → windowed
day scans are bounded), then extend only new days each cycle. Powers:
/network pulse, /ledgers strip, /operations trends, /accounts
new-accounts chart, home pulse.

**P-B per-asset balance sums** — one extra aggregation in the existing
ch-holders-rollup cycle: `asset → (sum_balance, holders)` (trustlines) —
enables per-asset concentration (top-10 holders share of supply held)
and the /assets hub strip. Near-zero marginal cost: the cycle already
scans the trustline set.

## Per-page designs (priority order)

1. **/contracts/[id] — activity timeline** *(cheap now)*: events-per-day
   sparkline + first-seen/last-seen + active-days count, straight off
   the contract-keyed `contract_active_ledgers` index (µs reads). The
   contract page finally answers "how alive is this contract?" at a
   glance. Also: /contracts hub strip (contracts with activity 24h/7d/
   all-time — needs a tiny daily-actives rollup from the same index).
2. **/assets/[slug] — holder concentration** *(needs P-B)*: holders
   count, top-10/top-100 share of held supply, holder-distribution
   bands. The token due-diligence panel: "3 wallets hold 92%" is the
   single most useful thing a token page can say.
3. **/assets hub strip** *(needs P-B)*: assets tracked / verified count /
   combined 24h volume / volume concentration (top-10 assets' share) /
   new assets this week.
4. **/network — the pulse page** *(needs P-A)*: TPS + ops/day trend,
   classic-vs-soroban share over time, new accounts/day, active
   accounts/day, fees/day. Becomes the "state of the network" flagship.
5. **/issuers — trust strip** *(cheap: PG counts)*: issuers total,
   SEP-1-resolved share, verified-org share, auth-flag adoption
   (auth_required / clawback / immutable percentages — a real trust
   signal), plus flags-adoption bar. One PG aggregate over `issuers`.
6. **/oracles — freshness + deviation** *(have data)*: per-feed
   freshness heatmap (last update age vs cadence), deviation-vs-
   aggregate sparkline per feed. Turns the list into a trust dashboard.
7. **/markets/[pair] — venue structure** *(bounded PG)*: 24h volume by
   venue donut, trade-size histogram, trade-count by hour strip —
   windowed reads on the pair's own trades (pair-keyed, bounded).
8. **/dexes/[source]** — volume trend (have per-source daily volumes via
   prices CAGGs), pair count, share-of-DEX-volume vs siblings.
9. **/ledgers + /operations** — fold in P-A trends (ops-by-class area,
   close-time stability strip) above the existing tables.
10. **/lending /amm /liquidity-pools** — TVL distribution bar +
    utilization histogram (lending) + new-pools-this-week.
11. **/bridges** — in/out volume trend + top corridors (rozo/cctp
    events, small tables).
12. **/mev** — BLOCKED on inventory #20 (detector correctness) — do not
    decorate data we know is wrong.
13. **Home** — after P-A: compact network-pulse strip above the fold.

## Sequencing

Unit 1 (contract activity) + P-B + unit 2/3 (asset concentration + hub)
ship first — highest insight-per-effort. Then P-A (its own follower
job + backfill), then units 4/9/13 which consume it. 5–8 interleave as
independent quick units. Everything validates against the sub-second
harness before being struck.
