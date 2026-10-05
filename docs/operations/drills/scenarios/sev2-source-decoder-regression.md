---
title: SEV-2 tabletop — Source decoder regression after protocol upgrade
last_verified: 2026-10-05
status: ratified
severity: P2
exercises_runbook: ../../runbooks/decode-errors.md
playbook_section: ../../sev-playbook.md#4-response-flow
---

# SEV-2 tabletop — Source decoder regression

~30 min, 3 people. Exercises [`decode-errors.md`](../../runbooks/decode-errors.md), [`source-stopped.md`](../../runbooks/source-stopped.md) and the SEV-2 path of
[SEV playbook §4](../../sev-playbook.md). Less catastrophic than the SEV-1 scripts: it tests detection latency and triage discipline.

## Setup and trigger

All sources healthy (`stellarindex_source_events_total` within ±20% of baseline). Mainnet upgraded to a hypothetical protocol 25 yesterday
23:00 UTC; no pre-test (testnet rollout was unstable). 09:15 UTC Thursday, oncall starting shift.

> 09:17 UTC the Ingestion → Decode errors panel climbs for **soroswap**: 0/s to 3/s over 5 min. `source_events_total{source="soroswap"}`
> still rises (events arrive, every one rejected). Aquarius, Phoenix and Comet are unaffected.

## Beats (T+ min:sec)

| T+ | Beat |
| --- | --- |
| 0:00 | `stellarindex_ingestion_decode_error{source="soroswap"}` fires (>1/s for 5m); P3 ticket, SEV-2 if it escalates |
| 5:00 | Indexer logs repeat `decode: SCVal: unknown discriminant 99 at field 'amount'` |
| 8:00 | `stellarindex_aggregator_class_drop_spike` fires: VWAP for affected pairs lost soroswap |
| 12:00 | Customer DM: why is XLM/USDC tracking only the SDEX side? |
| 18:00 | Other sources keep working: soroswap-only failure |
| 25:00 | stellar-core 25.0.0 release notes mention an SCVal type-tag enum extension |

## Expected response

- **30 min, acknowledge + diagnose** ([§2](../../sev-playbook.md#2-timelines-the-sla-promises)): acknowledge within 30 min; open `#incident-<date>-soroswap-decode`;
  apply the `decode-errors.md` quick-diagnosis flow (source from alert label, logs grouped by error pattern, root cause #2 "Stellar protocol version bump").
- **1 h, confirm:** check the network `protocolVersion` on the public stellar-rpc (`https://mainnet.sorobanrpc.com`, per the runbook's diagnostic command);
  same WASM hash but changed SCVal type-tag space; decoder-side not source-side because `source_events_total` still rises.
- **4 h, mitigate:** no runtime fix (failed-decode events are not retried). Forward-fix: handle the new type-tag in `internal/scval`, add a golden-file fixture,
  ship via the normal release process. Tell customers affected pairs may show elevated `flags.divergence_warning` while soroswap is out; price still served from the other sources.
- **After the fix, recover the gap:** soroswap is a projected source, so rewind the projector, never a bespoke backfill:
  `stellarindex-ops projector-replay -config /etc/stellarindex.toml -source soroswap -from <protocol-25-activation-ledger> -write`
  (see [decode-errors.md](../../runbooks/decode-errors.md)). Triangulated rates recompute on the next aggregator tick.

## Pass criteria

1. Acknowledged within 30 min.
2. Found `decode-errors.md` first try.
3. Linked the timing to the protocol upgrade before the leader's T+25:00 narration.
4. Confirmed decoder-side (events still rising), not source-stopped.
5. Did not panic or restart (SEV-2, nothing to restart).
6. Identified fix-forward: `internal/scval` update, golden fixture, ordinary deploy.
7. Proposed replaying the gap window after the fix.
8. Surfaced `flags.divergence_warning` as the customer-facing degradation signal.

## Gaps surfaced by prior runs

Slow drift below the 1/s threshold goes unalerted (action: per-source ratio alert, `decode_errors / events_total > 5%`);
protocol upgrades are discovered by operator knowledge, no release-notes watcher on `developers.stellar.org`.

## Variants

Multi-source (every Soroban source fails; protocol-wide vs source-specific); subtle (0.7/s, below threshold, found via customer report).
