---
title: Runbook — stellarindex_amm_self_pair_swap_burst
last_verified: 2026-08-26
status: draft
severity: P3
---

# Runbook — `stellarindex_amm_self_pair_swap_burst`

## Why this exists

The 2026-08-25 Blend/Comet exploit ran ~390 **self-pair swaps**
(`token_in == token_out`) through a curated Comet/Balancer pool to walk
its spot price, and both existing guards were blind to it: the freeze
guard treated the two-venue move as "corroborated," and the divergence
board never referenced the SAC pair. Worse, the self-pair rows never
reach the served `trades` table — the decoder maps them to zero rows
(`(nil, nil)`), so nothing downstream could count them. This alert is the
tripwire wired at that exact drop point (`internal/sources/comet/
dispatcher_adapter.go`), so a repeat of the primitive is visible even
though it moves no served trade.

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_amm_self_pair_swap_burst` |
| Severity | P3 (ticket — a new detector; escalate to page once it proves out) |
| Detected by | Prometheus rule in `deploy/monitoring/rules/anomaly.yml` (+ `configs/prometheus/rules.r1/anomaly.yml`) |
| Metric | `stellarindex_amm_self_pair_swap_total{source}` |
| Typical MTTR | 15 min to triage; incident-dependent to resolve |
| Impact | None direct — self-pair rows are dropped before serving. A sustained burst means someone is hammering a pool's internal price, which the freeze/divergence guards do not see. |

## Symptoms

- `increase(stellarindex_amm_self_pair_swap_total{source="…"}[15m]) > 10`.
- Historically this counter is **flat at zero** — any climb is the signal.
- The affected pool's constituent assets may show a spot-price walk over
  the same window (check `/v1/price` history for the pool's pair).

## Quick diagnosis (≤ 5 min)

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

## Mitigation (≤ 15 min)

This is a **detection** alert, not an outage. There is no service to
restore; the goal is to decide malicious-vs-benign and escalate.

- [ ] Confirm the swaps are genuinely self-pair (same `token_in`/`token_out`)
      from step 2's tx envelope — rule out a decode quirk.
- [ ] If malicious (a coordinated burst walking the pool price): open a
      SEV, capture the tx set + signer, and check whether any published
      price for the pool's assets moved (cross-reference the divergence
      board and freeze markers). The served price is protected by the
      substance + scam gates, but treat a live manipulation as an incident.
- [ ] If benign (a pool operation we do not yet model that legitimately
      emits `token_in == token_out`): widen or silence the alert and file
      a decoder-modelling follow-up so the primitive is classified, not
      just counted.
- [ ] Verification: `increase(...[15m])` falls back below 10 within ~15
      min of the burst ending (the window rolls off).

## Root cause analysis

Gather for the postmortem: the full tx set from `soroban_events`, the
signer/source account, the pool contract + its constituent assets, the
spot-price series over the window, and whether the freeze/divergence
guards engaged. Compare against the 2026-08-25 Blend/Comet incident
forensics.

## Known false-positive patterns

- A curated AMM pool that legitimately emits a `token_in == token_out`
  event for a non-swap operation we have not yet modelled. None are known
  today (comet emitted zero before the exploit), which is why the
  threshold sits low; if one is discovered, model it in the decoder and
  raise/scope the threshold rather than leaving the count ambiguous.

## Related

- Implementation: `internal/sources/comet/dispatcher_adapter.go` (the drop
  point + counter), `internal/obs/metrics.go` (`AMMSelfPairSwapTotal`).
- Companion guards this detector backstops: [anomaly-freeze-engaged](anomaly-freeze-engaged.md)
  (the freeze guard the exploit defeated) and the divergence board.
- Upstream: the 2026-08-25 Blend/Comet exploit dossier + ADR-0033
  (completeness), which motivated dropping self-pair swaps to `(nil, nil)`.

## Changelog

- 2026-08-26 — initial draft; wired with the `AMMSelfPairSwapTotal` metric
  (4.4c exploit-shaped detector, post-2026-08-25 Blend/Comet).
