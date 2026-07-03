---
title: Pass B per-page review — findings (agents, 2026-07-03)
status: current
last_verified: 2026-07-03
---

# Pass B findings

Two family reviewers, 10-point checklist per page type. Fix wave =
severity-ordered; rows move to 'fixed' as commits land. The register
(REGISTER.md) holds the S-numbered waves; these carry AM-/ACC-/CON-/ISS-/X-
ids.

## Family: assets + markets (30 findings)

See the conversation record; P1s: AM-01 (pair-page quote-vol /1e8 not
1e7 + $-mislabel), AM-02 (trade amounts raw stroops), AM-03
(include=sparkline7d ignored server-side — dead chart column), AM-04
(build cache strips ATH/top-markets/sparklines on most asset pages),
AM-05 (/markets leads with CEX tape under an on-Stellar header), AM-06
($ on non-USD quotes). P2: AM-07..AM-16. P3: AM-17..AM-30.

## Family: accounts + issuers + contracts (22 findings)

P1s: ACC-1 (burn address = 'richest account' $11.3B), CON-1 (protocol
column empty on all 100 rows), CON-2 (SAC names resolvable today via
/v1/sac-wrappers but rendered as bare hashes; XLM SAC missing from the
operator map), ISS-1 (supply/mcap fetched then dropped), ACC-2
(sourced-vs-all mislabel). P2: ACC-3..CON-4, X-1 (error-as-empty
conflation). P3: ISS-5..CON-5.

Full tables live in the session transcript; fixes reference the ids.
