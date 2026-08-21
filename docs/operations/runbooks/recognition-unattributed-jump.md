---
title: Runbook — recognition unattributed-shapes jump
last_verified: 2026-08-22
status: ratified
severity: P3
---

# Runbook — `stellarindex_recognition_unattributed_jump`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_recognition_unattributed_jump` (P3, ticket) |
| Severity | P3 |
| Detected by | Prometheus rules in `deploy/monitoring/rules/data-freshness.yml` + `configs/prometheus/rules.r1/data-freshness.yml` |
| Metric source | `stellarindex_recognition_unattributed_shapes` — `data-freshness.sh` (15-min timer, textfile collector) parses the system `recognition` row of `completeness_snapshots`, refreshed by each `compute-completeness` pass |
| Steady-state | ~22,900 shapes (2026-08 census), organic growth ~30/day; alert = delta > 250 over 2d for 1h |
| Customer impact | None directly — but the founding failure class means a source's OWN recognition axis may be reading green over a real event drop |
| Companions | [completeness-incomplete](completeness-incomplete.md) |

## Why this exists

`stellarindex_recognition_unattributed_shapes` counts the distinct
(contract, topic) event shapes in the certified lake that **no enabled
source's decoder claims** — i.e. the rest of the Soroban ecosystem. For
a curated indexer this is EXPECTED to be large and slowly growing,
which is exactly why the system `recognition` snapshot is excluded from
`stellarindex_completeness_incomplete` (a permanently-red alert trains
operators to ignore the channel that catches real gaps).

The alertable signal is a **step change**. The founding failure class is
the rozo/BACKLOG-89 blind spot: a protocol we DO index loses its
ownership mapping (`protocol_contracts` seed / `GatedRegistryOptions`
warm), its shapes stop being attributed to the source's own recognition
axis (which then reads **green over a real drop**), and they fall into
this census instead. The jump is the only externally visible symptom.

## Quick diagnosis

1. **Census the current shapes:**

   ```sh
   set -a; . /etc/default/stellarindex-ops; set +a
   stellarindex-ops ch-recognition -config /etc/stellarindex.toml -top 60
   ```

   Look at the top new entries — a registry regression shows a
   large-event-count contract whose FIRST ledger (`ledgers=[first,last]`)
   is old but which is newly unattributed. A genuinely new foreign
   protocol shows a recent first ledger.

2. **Check whether the jumped contracts belong to an enabled source:**
   diff the top contracts against `protocol_contracts` (Postgres) and
   the factory lists logged at completeness startup ("gated registry
   warmed source=… factories=… children=N"). A known factory child
   appearing here = the ownership map lost it.

## Remediation

- **Registry regression:** re-seed (`seed-protocol-contracts`), confirm
  the next `compute-completeness` pass re-attributes (the count drops
  back), and audit the affected source's `recognition_ok` history — it
  may have been reading green over a real drop; re-derive the affected
  window if events were skipped.
- **Genuine foreign-protocol burst:** no action — note it and let the
  baseline absorb it. Consider a decoder if the protocol is in scope
  for the product.

## Related

- [completeness-incomplete](completeness-incomplete.md) — the
  per-source recognition axis (the real silent-drop alarm for owned
  contracts) still fails that source's own row and alerts there.
- `verify-recognition` (ops subcommand) is the per-range deep audit.
