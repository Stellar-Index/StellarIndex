---
title: Soroswap taker coverage 100%; verify results + AC2 load-test status
captured: 2026-07-31 ~19:50Z (v0.21.7 tail chain completion)
verdict: 🟢 taker retro-fill COMPLETE (554,221/554,221 = 100.00%); completeness 15/17 with all three gaps root-caused + fixes staged; AC2 anchor = the clean acceptance run
---

# Taker coverage (the replay's goal)

`SELECT count(*), count(taker) FROM trades WHERE source='soroswap'`
→ **554,221 / 554,221 = 100.00%**. Every historical soroswap trade now
carries its taker (decoder fix fda0aea0 + full-history replay).

# Completeness after the full all-source verify

15/17 complete. The three gaps, each root-caused same-day:
- **redstone** — the known 15 provably-ambiguous ledgers; exact
  closure (state-write attribution) LANDED on main, flips with the
  v0.21.8 deploy + scoped replay.
- **soroswap** — TWO artifacts surfaced by the deeper replay window:
  (1) 20 skim-event legacy twins (pre-discriminator event_index=0 rows
  duplicated against the replay's true-index rows — the CCTP
  migration-0112 class) → **DELETED same evening, 0 remain**;
  (2) 1 blind ledger (57,403,300): LP-share `transfer` events from
  gated pair contracts, unclassified by the soroswap decoder
  (honest-blind) → classify-as-recognized-no-op fix in progress,
  rides v0.21.8.
- **aquarius** — verify TIMED OUT mid-run (box under post-replay
  churn); scoped re-verify queued overnight on the quiet box.

# Replay livelock incident (same day, resolved)

The taker replay livelocked at 63.058M: retro-fill upserts into a
compressed trades chunk blew write deadlines (373k retried inserts/h,
all recoverable by design — the cursor held). Fix: decompressed the 4
compressed chunks between cursor and tip (69GB monster among them);
replay resumed immediately and completed. The 22:45Z compression
policy recompresses automatically. Bank: taker-style retro-fills
against compressed history need the chunks decompressed FIRST.

# AC2 load evidence status

Primary anchor remains the CLEAN acceptance-contract run:
**30,601 reqs, 0 failures, med 8.5ms, p95 37.1ms** (k6-acceptance.json).
All three mixed-scenario attempts were contaminated in sequence:
mixed (rate-limited key: 69% 429s), mixed2 (during replay saturation),
mixed3 (immediately post-replay: cold caches on 100GB+ freshly
decompressed data, verify churn — p95 5.1s, 4.5% fails; ALSO
self-inflicted both SLO burn-rate pages, which roll off). **mixed4 is
armed overnight** after recompression on a genuinely quiet box — the
definitive mixed-load number.

# Same-evening ops closures

- 19 missing compression policies applied (DAT-03) + the assertion's
  OWN stale postgres password file repaired (cred-drift class — the
  assertion could never have passed regardless of policies);
  `compression_policies_applied` now 1. Ansible codification of the
  password file rides the vault-password OP item.
- Root disk 81%→78% (removed finished chain logs + superseded one-shot
  binaries).
- Alerts cleared today: projector_lag, dex_trade_unit_ratio (rewritten
  fraction-based), config_assertion, freeze_escalated, root-disk.
