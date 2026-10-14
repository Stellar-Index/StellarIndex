---
title: Open fixes inventory — 2026-08-08
last_verified: 2026-08-29
status: point-in-time audit
---

# Open fixes inventory — 2026-08-08

> **SUPERSEDED by [`v1-launch-plan.md`](v1-launch-plan.md) and the private inventory.
> Frozen: do not read a row below as open work.** The reconciliation against `origin/main`
> is in issue #321. Rows are kept because systemd units, SQL and Go comments cite them by
> number (1, 2, 4) and blocked inventory items cite rows 7, 12, 14, 16, 22, 29, 33–35.

## Where the still-open threads live

- 7 per-account deep trade history: W8 item 13 (CH account-keyed mirror vs bounded horizon).
- 12 residual, r1 `[supply].sac_wrappers` for USDC/AQUA: code shipped, r1 landing unconfirmed.
- 14 SEP-41 genesis rollup resets (12 of 13 remain): W5.4, gated on the `ops_batch` ClickHouse profile on r1.
- 15 LP reserve/trustline backfill: ACCEPTED, documented cutoff, no backfill.
- 22 residual, sub-$100M base-unresolvable prints: W6.5 accepted-risk register.
- 31, 34: `audit-remediation-operator-actions.md`. 33: CoinGecko licence is HELD (`v1-launch-plan.md`). 35: decision D10.

Legend: **[ENG]** engineering fix; **[OWNER]** operator decision first; **[VERIFY]** re-verify before working.

## Tier 1 — site loading / explorer serving

1. **[ENG] Post-P23 all-asset movement archive** — closed (`ch-cap67-movements`).
2. **[ENG] contracts census cost** — closed (`contracts_census_daily`); never a Summing MV over an RMT (the migration-0059 double-fire class).
3. **[ENG] `/contracts/{id}/code-history`** — ship the reader only after `ch-instance-backfill` completes.
4. **[DONE] `/assets/{id}/holders`**.
5. **[ENG] refresh-gate saturation UX** — closed (`TryAcquireClass`).
6. **[ENG] old-account `/transactions`, `/operations` 3–8 s cold** — re-measure first.
7. **[ENG] per-account deep trade history** — bounded to the uncompressed horizon with an honest note (ADR-0048).
8. **[DONE] `/contracts/{id}/wasm`**.

## Tier 2 — data correctness / money

Rows 9–30 (decoder drops, identity aliasing, pricing guards, SSE, dashboard auth, billing, CI residue) are tracked in the private inventory. Cited here by number:

14. **[OWNER]** SEP-41 genesis rollup resets on r1: 13 contracts double-counted, one psql each.
15. **ACCEPTED** LP reserves live-only from ledger 63.3M; no backfill.
16. **[ENG]** manage_data G-address injection into another account's operation history: render-side provenance guard.
19. **[ENG]** comet migration-0059 double-count, disarmed by migration 0137.
22. **[OWNER]** pricingguard downside protection off for 27.5% of pairs; sub-$100M residual accepted (W6.5).
29. **[ENG]** CI gate residue: lint-metric-refs accepts a comment as emission proof.

## Tier 3 — operator one-offs

31. Cloudflare zone cache rule (respect origin) + one-time purge.
32. `ansible-playbook --tags caddy` (two pending config changes).
33. CoinGecko Pro key (`COINGECKO_API_KEY` on r1).
34. GCP SA key rotation (`~/.config/stellarindex/`).
35. Privacy/GDPR review.
