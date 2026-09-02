---
title: Volume-legitimacy + scam signals for the asset directory
last_verified: 2026-08-24
status: partially shipped — §3 landed (#423, #460); §4 policy still pending
date: 2026-08-25
---

# Wash-volume detection + scam-label surfacing

Origin: 2026-08-24 operator report — a scam AUD token (`AUD-GAIF52QZ…GAUD`,
issued by audrev-stellar.com) showed ~$205k/day directory volume. Forensics
proved it wash: 108/109 of its 14-day XLM/AUD trades are between one wallet
pair where the **taker is the token's own issuer**. Our usd_volume math is
correct (the real XLM leg moves); the volume is economically fabricated.

## 1. The taxonomy (network-wide census, 2 days, >80% single-account-pair)

| species | signature | example | treatment |
|---|---|---|---|
| **Operational corridor** | issuer-side, few large trades, wrap/redeem | USDC↔USDCAllow $32M/2d; AUDD↔AUDR $344k | label OPERATIONAL, exclude from market-volume rankings |
| **Volume-painting wash** | market-styled pair, issuer as sole counterparty | the scam AUD | flag CONCENTRATED, do not count as market activity |
| **Third-party ping-pong** | two non-issuer wallets round-tripping both directions | XAUa↔USDV $23k/2d | flag CONCENTRATED |
| **Dust-bot** | one account pair, thousands of micro trades | HELIX/XLM 6221 trades / $5.5k | flag CONCENTRATED (low-value) |

High issuer-side share alone is NOT wash — a stablecoin mint/redeem corridor
is 100% issuer-side by nature. The discriminator is **issuer-side AND
single-counterparty AND a market-styled pair** (native/X, USDC/X), versus a
wrap corridor (assetAllow/asset).

## 2. Signals to compute (per-pair account-structure rollup)

A ClickHouse rollup over `trades` (maker, taker, base/quote issuer suffixes),
refreshed on the existing aggregate cadence:
- `distinct_makers`, `distinct_takers`
- `top_account_pair_vol_share` — max (maker,taker) volume / pair volume
- `self_cross_share` — volume where maker==taker
- `issuer_side_share` — volume where maker or taker == base/quote issuer
- `is_market_styled` — quote ∈ {native, USDC, fiat} (a real price surface) vs
  a wrap pair (assetAllow/asset, or two SACs of the same asset)

Derived `volume_character` enum on the asset/pair payload:
`market` (default) · `operational` (issuer-side wrap corridor) ·
`concentrated` (>90% one account pair on a market-styled pair).

## 3. Scam-label surfacing (SHIP FIRST — no policy needed)

> **SHIPPED** (#423, #460). Scam-flagged issuers no longer publish a
> price claim: `suppressScamIssuerPricing` /
> `withholdPriceSeriesWhenUnpriced` in
> `internal/api/v1/asset_directory_tags.go`, with the matching
> withholding in `internal/api/v1/assets.go` and
> `internal/api/v1/chart.go`. The rest of this section is retained as
> the design record; §4 is still the open decision.


`account_directory` (migration 0136, stellar-expert public-directory) ALREADY
tags the scam issuer `{malicious, unsafe}` with domain audrev-stellar.com —
but the data is display-only and not joined onto asset pages. Surface it: an
asset whose issuer address carries a `malicious`/`unsafe`/`fraud` directory
tag renders a scam warning banner on its asset page + a badge in the
directory, with the third-party attribution the table's doc comment requires
("display-only, third-party attribution, never an input to verification").
This is a pure read-join + UI, no policy fork, and it's the highest-value
half — it labels the exact asset that triggered the report.

## 4. POLICY FORK — operator decision (recommendation: annotate + demote)

Two honest options for concentrated/wash volume in the DIRECTORY:
- **(A) Annotate only** — keep raw volume, badge it "concentrated: >90% one
  account pair incl. issuer". Chain-fact posture: we report what the ledger
  did, labeled. Nothing is hidden or editorialized.
- **(B) Annotate + demote** — additionally rank the directory by a
  concentration-adjusted volume (raw × (1 − top_account_pair_share) style, or
  simply exclude `concentrated`/`operational` from the default "by volume"
  sort), so wash assets don't sit atop the directory painting legitimacy.

**Recommendation: B.** A directory sorted by raw volume lets a $50/day wash
farm outrank real assets — the exact outcome the operator flagged. B keeps the
raw number visible (honest) but stops the ranking from rewarding fabricated
volume, and it makes the popularity tripwire (§5) robust. A is the fallback if
"never re-rank chain facts" is the stronger principle for you.

## 5. Tie into the priceless-popular tripwire (task #28)

The popularity floor that fires the "popular asset has no price" alert MUST use
concentration-adjusted volume, or a wash farm self-selects into the alert and
we chase phantoms. Wire §2's `volume_character` into #28's floor: only
`market`-character volume counts toward "popular".

## Build sequencing
1. §3 scam-label surfacing — ship immediately (read-join + UI, panel-verified).
2. §2 rollup + `volume_character` — next, with the census query as the test oracle.
3. §4 ranking — gated on the operator choosing A or B (default B on no answer,
   since the goal directive says work everything with my recommendations).
4. §5 — folds into #28 when that builds.
