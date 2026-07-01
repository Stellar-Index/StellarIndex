---
title: D4 — Documentation for agents / discoverability — findings + CLAUDE.md redesign
---

# D4 — Agent docs / discoverability

**Headline:** the rebuild-what-exists failure is **structurally caused**. The flagship
artifact ([CAPABILITY-INVENTORY.md](CAPABILITY-INVENTORY.md)) is now drafted; findings below.

## M0 — directly causes rebuild-what-exists
- **M0-1 — NO intent→capability index exists anywhere.** CLAUDE.md is keyed by *package name*
  and organized around *decisions/why*, not *"what am I trying to do."* An agent wanting to
  "SSRF-guard an outbound fetch" has no path to the code. Root cause of the pain.
- **M0-2 — SSRF guarding is ALREADY DUPLICATED, both copies unexported** — `metadata/sep1.go`
  (`ssrfDialer`,`isBlocked`) AND `customerwebhook/ssrf.go` (`ssrfGuardedDialContext`,`isInternalIP`),
  both hand-rolling the 169.254.169.254 block. **The thesis caught in the act** — and the doc
  surface hints neither exists, guaranteeing a third copy at the next outbound fetch.
- **M0-3 — the repo map undercounts ~3× (33 listed vs 90 leaf pkgs) and OMITS the exact reusable
  utilities agents rebuild** — `xdrjson` (0 mentions), `usage`, `redisclient`, most of `scval`.
  An agent trusting the map concludes a capability is absent and rebuilds it.

## M1
Repo map is package-keyed + prose-shaped (agents skim ASCII trees; the load-bearing "which symbol
/ don't rebuild" isn't where an intent-reader looks); **doc.go coverage 42% (38/90) and inverted
against reusability** — the packages with NEITHER doc.go nor README are exactly the reusable libs
(`scval`,`xdrjson`,`completeness`,`customerwebhook`,`dispatcher`,`ledgerstream`,`pipeline`,`usage`,
`clickhouse`,`redisclient`); known factual drift (Comet "one open case" false — 4 decoders ungated;
`storage/` "MinIO adapters" false — no top-level files) erodes trust so agents rebuild instead of
trusting a pointer.

## M2
doc.go quality varies + no see-also cross-links (ratelimit↔cachekeys.RateLimitKey↔usage); CLAUDE.md
front-loads philosophy over lookup.

## CLAUDE.md redesign spec (the D4 deliverable)
1. **Add a top-of-file "Before you build anything — capability index"** + check in
   `CAPABILITY-INVENTORY.md` at repo root, linked from CLAUDE.md, with the top ~20 most-rebuilt
   primitives inlined so an agent that never opens the file still gets caught.
2. **Fix the repo map** — state the real count ("90 leaf pkgs; see the inventory for the full
   surface"), add the missing lines (xdrjson/usage/redisclient/…), change each gloss from
   "what it is" to "when you'd reach for it."
3. **Mandate a per-package `doc.go` convention + enforce in CI** (extend docs-lint to fail when a
   non-source leaf package lacks doc.go — mirrors the "every alert has a runbook" gate) — this keeps
   the inventory from rotting. Backfill the 12 reusable-lib blind spots first.
4. **Correct the drifted invariants/traps** (Comet, storage/MinIO — ties CS-127).
5. **Extract the two realized dups** (SSRF → `internal/safehttp.GuardedTransport`; export the HMAC
   signer) and list them in the inventory.
6. **Add a Definition-of-Done line:** "new utility code requires a one-line justification that
   CAPABILITY-INVENTORY.md was checked and the capability is genuinely absent" — makes discovery a
   review-enforced habit, not a hope.
