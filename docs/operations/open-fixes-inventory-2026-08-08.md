---
title: Open fixes inventory — 2026-08-08
last_verified: 2026-08-29
status: point-in-time audit
---

# Open fixes inventory — 2026-08-08

> **⛔ SUPERSEDED by [`v1-launch-plan.md`](v1-launch-plan.md) and the
> private inventory. Frozen — do not read a row below as open work.** The
> row-by-row reconciliation against `origin/main` (24 done, 3 superseded,
> 5 open, 3 partial of 35) is in issue #321; it is not restated here.
> Rows kept because systemd units, SQL and Go comments cite them by number
> (1, 2, 4) and blocked inventory items cite rows 7, 12, 14, 16, 22, 29, 33–35.

## Where the still-open threads live

| Row | Now tracked as |
|---|---|
| 7 — per-account deep trade history | W8 item 13; inventory INV-0902 (CH account-keyed mirror vs bounded horizon) |
| 12 residual — r1 `[supply].sac_wrappers` for USDC/AQUA (code shipped, r1 landing unconfirmed) | W2 + r1 config confirm (INV-0902 evidence) |
| 14 — SEP-41 genesis rollup resets (12 of 13 remain) | W5.4, gated on the `ops_batch` ClickHouse profile on r1 |
| 15 — LP reserve/trustline backfill | ACCEPTED: documented cutoff, no backfill (W8-12) |
| 18 residual — `MinBatchLimit` wedge | W8 item 9 (INV-0880, done) |
| 22 residual — sub-$100M base-unresolvable prints stay unchallenged | W6.5 accepted-risk register (INV-0839) |
| 31 — Cloudflare zone cache rule + purge | `audit-remediation-operator-actions.md` |
| 33 — CoinGecko Pro key | `v1-launch-plan.md` §3 `[OP]` item 2 |
| 34 — GCP SA key rotation | `audit-remediation-operator-actions.md` CS-001 |
| 35 — privacy/GDPR review | decision D10 (documentation sign-off) |

Sub-claims 20b/20c (`changesummary` silent failures, `d30` NULL) and 24a/24b
(VWAP volume-unit window dependence) were re-measured or dropped (INV-0866,
INV-0867). 29b (actions-pinning no-op on push-to-main) survives only in
INV-0902's evidence.

Legend: **[ENG]** engineering fix; **[OWNER]** operator decision first;
**[VERIFY]** may already be fixed — re-verify before working.

## Tier 1 — site loading / explorer serving

1. **[ENG] Post-P23 all-asset movement archive** — closed `d1cd18ac`
   (`ch-cap67-movements`). `sep41_transfers` projected only watched
   contracts, so classic XLM payments after P23 were missing from
   `/v1/accounts/{g}/movements`. Design: derive movement rows lake-side
   from `stellar.contract_events` transfer/mint/burn (reuse
   `sep41_transfers.decodeTransfer` + `clickhouse.FanOutAccountMovement` +
   `InsertAccountMovements`), backfill P23→tip under run-heavy-job, then
   decode-at-ingest. RMT on `account_movements` makes the backfill
   replay-safe.
2. **[ENG] contracts census cost** — closed `f75ab4b2` (day-keyed
   `contracts_census_daily`). Ranking from `contract_events_daily` is not
   the fix (HLL-state merge costs more than it saves); derive counts from
   the RMT-collapsed active-ledgers index in a periodic job, never a
   Summing MV over an RMT (the migration-0059 double-fire class).
3. **[ENG] `/contracts/{id}/code-history`** — keyed
   `contract_instance_changes` index + MV; run `ch-instance-backfill` to
   genesis, then ship the reader only after the backfill completes
   (present + non-empty = trusted).
4. **[DONE] `/assets/{id}/holders`** — 30-min holders rollup + keyed reads.
5. **[ENG] refresh-gate saturation UX** — problem-type split shipped;
   per-key-class gates closed `ef278218` (`TryAcquireClass`).
6. **[ENG] account family latency for old accounts** — `/transactions`,
   `/operations` 3–8 s cold; re-measure, then chase the real arm.
7. **[ENG] per-account deep trade history** — compressed trades chunks
   have no per-account index; current fix bounds to the uncompressed
   horizon with an honest note. Options: CH account-keyed mirror
   (ADR-0048) or taker/maker in compression segmentby (recompress-all,
   hurts the pair workload).
8. **[DONE] `/contracts/{id}/wasm`** — both hops read keyed
   `ledger_entries_current`.

## Tier 2 — data correctness / money

9. **[ENG]** Phoenix pre-upgrade decode drops (5,161 swaps, 616 bond/unbond,
   Map-schema provide/withdraw_liquidity) — decoder fix + projector-replay.
10. **[ENG]** DeFindex harvest/dfees discarded (1,018 + 8,018 events) — decode + replay.
11. **[ENG]** Aquarius fee token dropped though present in topic[1] — decode + replay + re-derive.
12. **[OWNER→ENG]** SAC-wrapped assets as a second un-aliased identity — alias-union sweep across money handlers.
13. **[VERIFY]** XLM total_supply 2.11× across two routes; volume scale flips 10×; `markets.last_price` stale:false.
14. **[OWNER]** SEP-41 genesis rollup resets on r1 — 13 contracts double-counted, one psql each.
15. **ACCEPTED** — LP reserves live-only from ledger 63.3M; no backfill.
16. **[ENG]** manage_data G-address injection into another account's operation history — render-side provenance guard.
17. **[ENG]** `recognition_ok` structurally always-true for match-by-topic sources (ADR-0033 gap).
18. **[OWNER]** `derive_generation` makes projector-replay corrections lose to equal-generation rows.
19. **[ENG]** comet migration-0059 double-count — disarmed by migration 0137.
20. **[ENG]** MEV sandwich detector names accounts on impossible evidence; `mev_events` 6700× growth.
21. **[VERIFY]** explorer serves build-frozen prices as live outside asset pages.
22. **[OWNER]** pricingguard downside protection off for 27.5% of pairs — policy call on the one-side-zero storage rule; sub-$100M residual accepted (W6.5).
23. **[VERIFY]** confidence capped at 0.5 on all served pairs; MinMAD floors 24/27; native/fiat:USD unscoreable.
24. **[ENG]** TWAP CAGGs cover 5 months vs 8 years for siblings (1y chart silently 5 mo).
25. **[ENG]** SSE: 3 VWAP windows interleaved on one topic; `?asset=native` matches nothing; payload names diverge from OpenAPI.
26. **[ENG]** tip stream 6 DB queries/s per connection (pool saturates ~2300 streams); `/v1/readyz` half closed (single-flight + 1 s cache).
27. **[ENG]** dashboard auth: login code derivable from stored token hash; signup email squatting; self-service key re-widening.
28. **[OWNER]** billing bridge inert — nothing writes `accounts.stripe_customer_id`.
29. **[ENG]** CI gate residue — lint-metric-refs accepts a comment as emission proof; actions-pinning no-op on push-to-main.
30. **[OWNER]** archive one-offs — chmod o+rx on placed archive dirs; ADR-0017 contract 4 never runs on r1.

## Tier 3 — operator one-offs

31. Cloudflare zone cache rule (respect origin) + one-time purge.
32. `ansible-playbook --tags caddy` (two pending config changes) — INV-0865 done.
33. CoinGecko Pro key (`COINGECKO_API_KEY` on r1).
34. GCP SA key rotation (`~/.config/stellarindex/`).
35. Privacy/GDPR review.
