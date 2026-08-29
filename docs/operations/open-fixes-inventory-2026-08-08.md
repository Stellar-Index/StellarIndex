# Open fixes inventory — 2026-08-08

> **⛔ SUPERSEDED 2026-08-29 by
> [`v1-launch-plan.md`](v1-launch-plan.md)** (issue #321). **Frozen — do not
> read a numbered row below as open work.** The file's own protocol ("an item
> leaves the list only when its fix is deployed and verified on r1") was
> abandoned the week it was compiled: the last edit was 2026-08-09 and rows
> kept closing without ever being struck. Verified first-hand at
> `0f13aa14`:
>
> | Row | Closed by | Still reads as open here |
> |---|---|---|
> | 1 — post-P23 all-asset movement archive ("the single biggest user-visible gap") | `d1cd18ac`, 2026-08-08 (`ch-cap67-movements`) | yes |
> | 2 — contracts census cost | `f75ab4b2`, 2026-08-10 (day-keyed `contracts_census_daily`) | yes |
> | 5 — refresh-gate per-key-class fairness (the "Remaining" half) | `ef278218`, 2026-08-10 (`TryAcquireClass`) | yes |
> | 19 — comet migration-0059 double-count armed | `a1c5c2e5`, 2026-08-08 (migration 0137) | yes |
> | 26 — `/v1/readyz` unlimited pool exhaustion (half of the row) | `a1c5c2e5`, 2026-08-08 (single-flight + 1 s cache) | yes |
>
> Rows 1 and 19 were closed on **the same day this file was compiled**. The
> full row-by-row reconciliation against `origin/main` — 24 done, 3
> superseded, 5 open, 3 partial of 35 — is in **issue #321**, which is where
> the per-row evidence lives; it is deliberately not restated here, because a
> second hand-maintained copy of it is the defect this banner closes.
>
> **The threads that are genuinely still open, and where they now live:**
>
> | Was | Now tracked as |
> |---|---|
> | 7 — per-account deep trade history (CH account-keyed mirror vs accept the bounded horizon) | **W8 item 13** (`accounts/{g}/trades` windowing) — needs the decision, not just the code |
> | 12 residual — r1 `[supply].sac_wrappers` for USDC/AQUA (the code shipped; the r1 config has no landing evidence, so the "53.5% of USDC volume invisible" headline is not closed) | **W2** (asset identity) + an r1 config confirm |
> | 14 — SEP-41 genesis rollup resets (12 of 13 remain) | **W5.4** — gated on the `ops_batch` ClickHouse profile on r1 |
> | 15 — LP reserve/trustline backfill path | **W8 item 12** — design decision outstanding |
> | 18 residual — `MinBatchLimit` wedge (replay-block itself is fixed; the wedge fix is undesigned) | **W8 item 9** |
> | 22 residual — sub-$100M base-unresolvable prints stay unchallenged (accepted residual per the 2026-08-25 W8 reconciliation) | **W6.5** — sign it onto the accepted-risk list |
> | 31 — Cloudflare zone cache rule / orange-cloud + purge | `audit-remediation-operator-actions.md` (unchecked) |
> | 32 — `ansible-playbook --tags caddy` for the two pending config changes | presumed applied (SSE works; ansible-drift would page) but **no apply record was codified** — needs one confirm, then strike |
> | 33 — CoinGecko Pro purchase | `v1-launch-plan.md` §3 `[OP]` item 2 |
> | 34 — GCP SA key rotation | `audit-remediation-operator-actions.md` CS-001 (checkbox still unchecked) |
> | 35 — privacy/GDPR review | decision **D10** — reduced to a documentation sign-off |
>
> **Sub-claims that fell out of tracking entirely** and are in no plan: 20b/20c
> (`changesummary` 25% silent failures, `d30` NULL), 24a/24b (VWAP volume-unit
> window dependence, 24 h VWAP blending 6.43 h/21.39 h legs), 29b
> (actions-pinning no-op on push-to-main). Each needs a **re-measure or an
> explicit drop** — they are currently neither fixed nor tracked.

Compiled on operator request ("we need to fix all of them"): every known
sidelined item from the 2026-08-03/04 cold-audit sweep, the 2026-08-07/08
site audits, and session findings — deduplicated, with what already
shipped removed. Sources of detail: the per-area audit memory files
(`~/.claude/projects/.../memory/project_*_audit_*.md`, one per area) and
`docs/operations/v1-launch-plan.md` (still the launch source of truth;
this is the defect-side companion).

Legend: **[ENG]** = engineering fix, no decision needed — queue and fix.
**[ASH]** = needs an operator decision/action first. **[VERIFY]** = audit
finding that may have been fixed since; re-verify before working.

## Tier 1 — site loading / explorer serving (the active thread)

Shipped this week: busy-contract 100× (v0.26.1); quiet-contract index +
backfill (v0.27.0); capacity-errors-as-500 fix; trades OR-scan → UNION +
compression-horizon bound; movements-tail OR-scan → UNION; honest
coverage notes on trades/activity/movements (v0.27.1); holders rollup +
saturation/timeout problem-type split + readyz single-flight (v0.28.1);
accounts analytics (v0.29.0); RT program + SSE correctness trio +
observations alias fan-in (v0.30.0); /ledgers dead-Suspense fix
(explorer 2026-08-09).

1. **[ENG] Post-P23 all-asset movement archive.** The single biggest
   user-visible gap: `sep41_transfers` projects only watched token
   contracts, so classic XLM payment history after 2025-09-03 appears
   NOWHERE in `/v1/accounts/{g}/movements` (the GATL report — the page
   "stops" at P23). The lake has every CAP-67 event (native SAC: 44.76M
   active ledgers). Design: derive movement rows lake-side from
   `stellar.contract_events` transfer/mint/burn (reuse
   `sep41_transfers.decodeTransfer` + `clickhouse.FanOutAccountMovement`
   + `InsertAccountMovements`), backfill P23→tip (~1.3B events, hours,
   run-heavy-job), then live at ingest via the extract's
   decode-at-ingest pattern (like SupplyFlows). Retires the PG tail
   merge. RMT on account_movements makes the backfill replay-safe.
2. **[ENG] contracts census cost** — `RecentContracts`-shape query: 40s /
   790M rows, 160 runs per 3h (4 window rungs × 5-min prewarm).
   MEASURED 2026-08-08: ranking from `contract_events_daily` is NOT the
   fix (24.5s — merging ~15M uniqCombined HLL states costs more than it
   saves; the table's t0/t1 dimensions explode its row count), and
   `contract_active_ledgers` is contract-first so a window scan doesn't
   prune. Needs a dedicated day-keyed plain-counts rollup with a
   dup-safety story (RMT re-insert double-fire through a Summing MV is
   the migration-0059 class — derive counts from the RMT-collapsed
   active-ledgers index in a periodic job instead of an MV). Own unit.
3. **[ENG→SHIPPING] /contracts/{id}/code-history** — keyed
   `contract_instance_changes` index + MV landed 2026-08-09 (bb77be89);
   DDL applied on r1 (live coverage from ledger 63863502). REMAINING:
   run `ch-instance-backfill` to genesis (queued behind the cap67 job —
   one heavy job at a time), then deploy the reader in the next release
   ONLY after the backfill completes (operator contract: present +
   non-empty = trusted).
4. **[DONE v0.28.1] /assets/{id}/holders** — 30-min holders rollup +
   keyed reads, 0.11–0.14s live-validated 2026-08-08.
5. **[ENG, half-done] refresh-gate saturation UX** — the problem-type
   split (saturated vs timed out) SHIPPED in v0.28.1. Remaining:
   per-key-class gates so one crawler burst can't starve every cold
   page class.
6. **[ENG] account family latency for old accounts** — /transactions,
   /operations 3–8s on quiet-box cold reads in the 2026-08-07 audit;
   re-measure after this week's fixes, then chase the real arm.
7. **[ENG] per-account deep trade history** — compressed trades chunks
   have no per-account index; current fix bounds to the uncompressed
   horizon with an honest note. Durable options: mirror trades to a
   CH account-keyed table (preferred, ADR-0048 logic) or add
   taker/maker to compression segmentby (recompress-everything cost,
   hurts the pair workload — probably no).
8. **[DONE, verified 2026-08-09] /contracts/{id}/wasm reads** — both
   hops (instance lookup + code fetch) read the keyed
   `ledger_entries_current` (509d1d83); no changes-log scan remains on
   the resolvable path.

## Tier 2 — data correctness / money (audit NEEDS-ASH backlog)

9. **[ENG] Phoenix pre-upgrade decode drops** — ALL 5,161 pre-upgrade
   swaps + 616 bond/616 unbond + Map-schema provide/withdraw_liquidity
   unrecognised. Decoder fix + projector-replay.
10. **[ENG] DeFindex harvest/dfees discarded** (1,018 + 8,018 events on
    a false premise) — decode + replay.
11. **[ENG] Aquarius fee token dropped** though present in topic[1]
    (migration 0129 baked it in) — decode + replay + re-derive.
12. **[ASH→ENG] SAC-wrapped assets as second un-aliased identity** —
    53.5% of USDC volume invisible to alias-blind readers; ten money
    handlers alias-blind (~$16.8M XLM volume hidden). Needs the
    alias-union sweep across handlers (mechanical but wide).
13. **[VERIFY] XLM total_supply 2.11× across two routes; volume scale
    flips 10× between requests; markets.last_price stale:false lies.**
14. **[ASH] SEP-41 genesis rollup resets on r1** — 13 contracts
    double-counted; EURC reset 2026-08-05, 12 remain (one psql each).
15. **[ENG] LP reserves live-only from ledger 63.3M** — no
    trustline/LP-reserve backfill path exists; design one.
16. **[ENG] manage_data G-address injection** — any account can inject
    an arbitrary G-address into another account's operation history for
    ~0.0001 XLM (proven live). Needs render-side provenance guard.
17. **[ENG] recognition_ok structurally always-true** for match-by-topic
    sources (ADR-0033 gap) — the completeness verdict can't see
    recognition failures for most sources.
18. **[ASH] derive_generation blocks ALL projector-replay corrections**
    — replay writes lose to equal-generation rows; corrections inert.
19. **[ENG] comet migration-0059 double-count armed** — fires on the
    first comet replay; disarm before any replay.
20. **[ENG] MEV sandwich detector names accounts on impossible
    evidence** (196/200 same-direction, median $0.10) + mev_events
    6700× growth + changesummary 25% silent failures + d30 NULL.
21. **[VERIFY] explorer serves build-frozen prices as live** (8.9%
    wrong at audit) — partially addressed by LiveAssetPrice on asset
    pages; sweep the other price surfaces.
22. **[ASH] pricingguard downside protection OFF for 27.5% of pairs**
    (attacker-inducible) — needs a policy call on the one-side-zero
    storage rule.
23. **[VERIFY] confidence capped at 0.5 on all served pairs; baselines:
    MinMAD floors 24/27; native/fiat:USD unscoreable.**
24. **[ENG] VWAP/TWAP unit honesty** — vwap volume unit
    window-dependent; 24h VWAP blends 6.43h/21.39h legs; TWAP CAGGs
    cover 5 months vs 8 years for siblings (1y chart silently 5mo).
25. **[ENG] SSE stream correctness** — 3 VWAP windows interleaved on
    one topic; `?asset=native` matches nothing; payload field names
    diverge from OpenAPI schema.
26. **[ENG] tip stream 6 DB queries/s per connection** (pool saturates
    at ~2300 streams) + `/v1/readyz` unlimited+unauthenticated pool
    exhaustion.
27. **[ENG] dashboard auth hardening** — 6-digit login code derivable
    from the stored token hash; signup email squatting; self-service
    key re-widening.
28. **[ASH] billing bridge inert** — nothing writes
    accounts.stripe_customer_id; whole billing path (incl.
    cancellation) dead. Product decision + wiring.
29. **[ENG] CI gate residue** — lint-metric-refs accepts a comment as
    emission proof; actions-pinning no-op on push-to-main.
30. **[ASH] archive one-offs** — chmod o+rx on placed archive dirs;
    ADR-0017 contract 4 never runs on r1.

## Tier 3 — operator one-offs (blocked on Ash or approval)

31. Cloudflare zone cache rule (respect origin) + one-time purge.
32. `ansible-playbook --tags caddy` (two pending config changes).
33. CoinGecko Pro purchase (COINGECKO_API_KEY on r1).
34. GCP SA key rotation (~/.config/stellarindex/).
35. Privacy/GDPR review (audit №3 leftover).

## Working order

_(Frozen 2026-08-09. The protocol below was never honoured — see the
supersession banner at the top.)_ Tier 1 items 1–5 next (site loading is the
active complaint), then Tier 2 money-correctness in listed order. Each fix =
one unit, committed + released per the session cadence; [VERIFY] items get
re-measured before work. This file is updated as items close — an item leaves
the list only when its fix is deployed and verified on r1, not when merged.
