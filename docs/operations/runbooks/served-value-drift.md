---
title: Runbook — served value diverges from independent ground truth
last_verified: 2026-07-02
status: ratified
severity: P3 (ticket)
---

# Runbook — `stellarindex_served_value_drift`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_served_value_drift` / `stellarindex_served_value_check_stale` |
| Severity | ticket |
| Detected by | `stellarindex_served_value_ok == 0` for 26h (two daily runs) |
| Typical MTTR | investigation-bound (data derivation, not availability) |
| Impact | A customer-visible NUMBER is wrong while every availability signal is green — the CS-010 class (XLM market cap read +58% until hand-sampled). Credibility, not uptime. |

## Alert description

`stellarindex-ops verify-served-values` (daily timer) reconciles a
curated set of values we serve against INDEPENDENT sources — the SDF
lumen API for XLM supply, Stellar Expert for classic-asset supply —
and emits `stellarindex_served_value_{ok,rel_err,last_run_unix}`
textfile gauges. This alert means a served value sat outside its
tolerance for two consecutive runs. The companion `_check_stale`
alert means the harness itself is dark (timer dead / run crashing).

Deliberate scope notes: prices are NOT checked here (the divergence
worker cross-checks them continuously); lake↔served row counts are
NOT checked here (compute-completeness owns that). This harness is
specifically "is the VALUE right", not "is the pipe healthy".

## When this fires

- A supply-derivation basis is wrong or partially populated (the
  known standing cases at the time of writing:
  `xlm_circulating_supply` reads `xlm_total_only` until the operator
  sets `sdf_reserve_accounts` — CS-010's config half; and
  `usdc_total_supply` under-reads vs Stellar Expert — board #34).
- A backfill/observer gap left the supply hypertables incomplete.
- The GROUND TRUTH changed methodology (SE counts locked amounts,
  SDF changes basis) — verify before "fixing" our side
  (feedback: confirm windows/bases match before claiming undercount).

## How to investigate

```sh
# Reproduce with full detail (read-only, run anywhere):
stellarindex-ops verify-served-values -api https://api.stellarindex.io

# What do we serve, on what basis?
curl -s 'https://api.stellarindex.io/v1/assets/native' | jq '.data | {circulating_supply, total_supply, max_supply, supply_basis}'

# The independent sources:
curl -s https://dashboard.stellar.org/api/v3/lumens | jq .
curl -s 'https://api.stellar.expert/explorer/public/asset/USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN' | jq '{supply}'

# Supply pipeline state (r1):
sudo -u postgres psql -d stellarindex -c "SELECT asset, basis, max(time) FROM asset_supply_history GROUP BY 1,2 ORDER BY 3 DESC LIMIT 10"
```

Then read `docs/architecture/supply-pipeline.md` for which algorithm
(1 XLM / 2 classic / 3 SEP-41) derives the failing value.

## How to mitigate

There is no cache to flush — fix the derivation (or its config), or
document the differing basis honestly on the wire (`supply_basis`).
Never widen a tolerance to silence the alert without a written
methodology justification in the check's `note`.

## How to escalate

Standing drift on a flagship value (XLM/USDC) that resists a day of
investigation → raise with @ash; it may need an upstream
(SDF/SE/Circle) methodology confirmation.

## Post-mortem notes from prior firings

- 2026-07-02 (first run, pre-alert): caught the harness's own unit
  bug (served F2 supply is base-unit strings), the standing CS-010
  config gap (47% on XLM circulating), and the new USDC finding
  (board #34) — one run, three findings.

## Related

- `docs/adr/0041-ingest-durability-semantics.md` — durability vs
  data-truth split.
- `docs/operations/runbooks/price-divergence.md` — the PRICE
  counterpart (continuous, divergence worker).
- `docs/architecture/supply-pipeline.md` — the three supply
  algorithms behind most values checked here.
