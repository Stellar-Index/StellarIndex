---
title: Evidence — 50-account balance reconciliation vs Horizon (post-D2/D3)
last_verified: 2026-07-29
status: final — E1 gate evidence
---

# reconcile-balances, 50-account sample — 2026-07-29 ~02:45Z

**Command:** `stellarindex-ops reconcile-balances -sample 50` on r1
(defaults: SDF's public Horizon API as the independent reference —
ADR-0001 scopes the Horizon ban to production ingest, not one-off
verifiers, per the tool's own doc comment; tolerance 0
stroops, serial with 250 ms delay; account identities sampled from the
deduped current-state projection — provably the same population as the
change-log sample, per the 28405f0a analysis). Run uncontended, after
the D3 cutover, WITHOUT piping (exit code intact).

**Result:**

```
reconcile-balances: 50 checked, 37 matched, 0 mismatch, 0 no_data,
13 merged_or_absent (0 stale-held), 0 truth_unavailable, 0 error
```

**Verdict: 0 mismatches at 0-stroop tolerance.** The pre-ordinal
baseline was **19/50 mismatched (38%)** — the C2-4c intra-ledger tie
ambiguity. After the D2 ordinal re-derive of the change log (which this
verifier reads directly) the mismatch rate is **zero** across every
account that still exists on-chain. `merged_or_absent` (13) means the
account no longer exists on Horizon (merged away) — a category the
verifier reports separately by design, not a failure.

**What this does NOT prove:** trustline/contract balances (this checks
native XLM account balances), the SERVED current-state table's values
(that is D3's own divergence sample — 66,539 keys, 0 latest-ledger
disagreement, filed in the plan's 2026-07-29 ~01:50Z log entry), or
anything about supply aggregates (separate reconcile).
