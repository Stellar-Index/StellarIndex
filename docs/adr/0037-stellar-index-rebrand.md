---
adr: 0037
title: Rebrand to Stellar Index (Stellar Atlas name was taken)
status: Accepted
date: 2026-06-12
supersedes: [0036]
superseded_by: null
---

# ADR-0037: Rebrand to Stellar Index

## Context

ADR-0036 (same day) rebranded the product from Rates Engine to **Stellar
Atlas** and executed the full migration including the live r1 cutover.
Immediately after, we found the name is **already in use** — exactly the
collision class a pre-flight would have caught. The replacement was chosen
from a candidate list that WAS pre-flighted this time: domain
registration (RDAP), GitHub name availability, and a same-category
product-conflict search (which eliminated, e.g., "StellarScope" —
stellarscope.net is an existing Stellar asset explorer).

## Decision

1. **Name**: the product is **Stellar Index**, at **stellarindex.io**
   (primary; `.xyz` held defensively). "Index" fits both halves of the
   identity: a comprehensive catalog of every protocol / contract / event
   (the explorer), and a price index (the pricing API).
2. **Code identity**: module path `github.com/StellarIndex/stellar-index`;
   binaries `stellarindex-*`; env prefix `STELLARINDEX_*`; metric
   namespace `stellarindex_*`.
3. **Positioning is unchanged** from ADR-0036: a protocol explorer for
   the Stellar network, pricing API as flagship product, evolving toward
   a comprehensive blockchain explorer (classic + Soroban).
4. **Process rule going forward**: any name/brand choice gets a
   pre-flight (domain RDAP + GitHub availability + same-category conflict
   search + SDF brand-policy consent check) BEFORE any migration work.
   SDF's brand policy requires written consent for "Stellar" in product
   or domain names; as an SDF RFP awardee, request that consent before
   public launch.

## Consequences

- Full re-run of the ADR-0036 migration playbook
  (`docs/operations/stellar-index-migration.md`), including the r1
  cutover. The second pass is cheap — the first migration parameterized
  every surface.
- "Stellar Atlas" never shipped anywhere durable: no DNS existed, no
  release was tagged, no org was created. The only public exposure was
  the auto-deployed explorer at ratesengine.net for a few hours.
- Historical records keep their historical names: ADRs 0001–0036 (0036
  documents the Atlas decision and is superseded, not rewritten),
  discovery, audits, CHANGELOG dated history.

## Related

- ADR-0036 (superseded) — the Atlas rebrand + repositioning rationale;
  the repositioning part carries forward unchanged.
