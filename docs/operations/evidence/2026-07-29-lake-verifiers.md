---
title: Evidence — lake verification battery (contiguity + hash-chain + verify-lake)
last_verified: 2026-07-29
status: final — all PASSED
---

# Lake verification battery — 2026-07-29 08:00–08:0xZ

Sequential run on r1 (`/var/log/lake-verifiers.log`), fresh post-D3 /
post-dedup / post-trim state, range **[2, 63,699,907]** (genesis → tip):

| verifier | result |
|---|---|
| `verify-contiguity` | ✅ exit 0 — ledger substrate contiguous [2, tip]; `ledger_entry_changes` coverage clean above the ec-floor (63,050,000) |
| `verify-hashchain` | ✅ exit 0 — **0 in-window + 0 boundary broken links** across [2, tip] in 1M-ledger windows |
| `verify-lake` | ✅ exit 0 — `total_failures=0 (PASSED)` (contiguity + entrychanges + hashchain combined) |

This is the §1 "prove-it" substrate evidence: every ledger from genesis
to tip is present and cryptographically chained. Covers the lake AFTER
the July dedup, the galexie trim, and the D1–D3 rebuild campaign — i.e.
it certifies the CURRENT substrate, not a pre-campaign memory of it.

**What this does NOT prove:** served-tier correctness (separate
artifacts: reconcile-balances, supply reconcile), per-source projection
completeness (completeness_snapshots — 16/17), or decoder semantics.
