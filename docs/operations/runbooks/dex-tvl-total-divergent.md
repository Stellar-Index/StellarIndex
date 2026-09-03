---
title: Runbook — dex-tvl-total-divergent
last_verified: 2026-09-03
status: draft
severity: P3
---

# Runbook — `stellarindex_dex_tvl_total_divergent`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_dex_tvl_total_divergent` (ticket) |
| Detected by | Prometheus rules in `deploy/monitoring/rules/api.yml` + `configs/prometheus/rules.r1/api.yml` |
| Typical MTTR | 5–20 min (usually the same backend reachability the sibling refresh alert covers) |
| Impact | The headline `tvl_total` on `/v1/protocols` — and the "Total value locked" figure on the explorer's protocol directory — UNDERSTATES: it excludes at least one protocol whose per-protocol figure is still published beside it. If nothing at all was admitted, `tvl_total` is omitted entirely and the explorer shows no headline. No 5xx, no wrong number: the response names every refusal in `excluded[]`. |

## Why this exists

`tvl_total` is defined as the exact sum of the per-protocol `tvl_usd`
strings published on the same response, so a reader can add the rows on
the page and land on the headline byte-for-byte
(`internal/api/v1/dex_tvl_total.go`). To keep that true, the
reconciliation ADMITS a protocol's figure only when that figure's own
claims hold — it is not carried forward from an earlier cycle, its
`as_of` is this snapshot's, its pool counts balance, and its decimal
parses — and DROPS one that does not, naming it in `excluded[]`.

That degradation is honest, and therefore invisible. There is no error
flag, no stale marker, and the sibling
[`dex-tvl-refresh-failing`](dex-tvl-refresh-failing.md) alert cannot
fire while the OTHER protocols keep refreshing successfully. Without
this alert a single protocol's reader could break permanently and the
public money figure would sit a whole protocol low, indefinitely, with
nobody informed.

## Symptoms

- `stellarindex_dex_tvl_reconcile_total{outcome="divergent"}` increasing
  with `outcome="ok"` flat for 1h+ (both children are zero-seeded at
  process start, so "flat at zero" is a real reading, not no-data).
- `curl -s localhost:3000/v1/protocols | jq '.data.tvl_total'` shows a
  `protocols[]` array SHORTER than the set of rows carrying a `tvl`
  object — or `tvl_total` is `null`/absent altogether.
- The explorer's `/protocols` page shows per-protocol TVL bars but the
  "Total value locked" headline is missing, or the headline is visibly
  smaller than the bars beneath it sum to.

## Quick diagnosis (≤ 5 min)

The response says which protocol and why — start there, not in the logs.

```sh
# Which protocol was refused, and under which rule?
curl -s localhost:3000/v1/protocols \
  | jq '.data.tvl_total | {protocols, lower_bound, as_of, excluded}'

# Compare against which rows DO carry a per-protocol figure: the
# difference is the refused set.
curl -s localhost:3000/v1/protocols \
  | jq -r '.data.protocols[] | select(.tvl) | .name'

# A carried-forward figure means that protocol's reserve read failed:
journalctl -u stellarindex-api --since -60min | grep "dex tvl" | tail -5
```

`excluded[]` always contains the STANDING scope entries (classic CAP-38
pools, the SDEX order book, lending supplied-value, vault AUM) — those
are not refusals. A refusal reads as a protocol NAME with a reason like
"carried forward from an earlier refresh".

## Mitigation (≤ 15 min)

1. Fix the failing backend. The reconciliation is stateless and
   self-healing: the next 10-min refresh readmits the protocol and the
   headline widens on its own. No restart needed.
2. If ClickHouse or the served tier is unhealthy, expect
   [`dex-tvl-refresh-failing`](dex-tvl-refresh-failing.md) or a louder
   backend alert alongside this one — treat that as the primary and this
   as its public-surface consequence.
3. Verification: `stellarindex_dex_tvl_reconcile_total{outcome="ok"}`
   resumes incrementing within one refresh interval (10 min), and
   `tvl_total.protocols` on `/v1/protocols` lists every protocol whose
   row carries a `tvl` object.
4. Do NOT "fix" this by loosening admission. A total that absorbs a
   carried-forward figure is a wrong number published under a fresh
   `as_of`, which is strictly worse than a narrow one that says so.

## Root cause analysis

Every refusal so far reduces to a protocol's reserve read failing for
that cycle (`DEXTVLCache.Refresh` keeps the previous entry and names the
protocol as carried), so this alert normally tracks
`stellarindex_dex_tvl_refresh_total{outcome="error"}`. Two other
admission rules exist and would indicate a code-level defect rather than
a backend one:

- **pool counts do not balance** (`pools_priced + unpriced_pools !=
  pools_total`) — a per-protocol accumulator has drifted; the protocol
  has no provable coverage and its figure is unusable.
- **the decimal will not parse** — the per-protocol `tvl_usd` is not a
  decimal string, which should be unreachable (`*big.Rat.FloatString`)
  and means the cache's rendering path changed.

Either of those points at `internal/api/v1/dex_tvl_cache.go` /
`dex_tvl_total.go`, not at an operational failure.

## Known false-positive patterns

- A ClickHouse merge window or a restart of a dependency can refuse one
  or two consecutive cycles; the 30-min rate window plus the 1-hour
  `for:` absorbs that (the `..._does not ticket` case in
  `deploy/monitoring/rule-tests/api_test.yml` pins it).
- Immediately after an API restart the snapshot is empty and no
  reconciliation has run, so both children read zero and the alert
  cannot fire. That is correct: `tvl_total` is absent, not wrong.

## Related

- Sibling alert on the same worker:
  [`dex-tvl-refresh-failing`](dex-tvl-refresh-failing.md) — fires when
  EVERY protocol's refresh errors; this one fires when the total is
  narrowed while the page still looks healthy.
- Metric docs: `docs/reference/metrics/README.md` —
  `stellarindex_dex_tvl_reconcile_total`.
- Published methodology (what the total includes, excludes and why):
  `docs/methodology/dex-tvl.md`.
- Admission rules in code: `internal/api/v1/dex_tvl_total.go`
  (`dexTVLAdmit`).

## Changelog

- 2026-09-03: created with the #338 headline-total alert.
