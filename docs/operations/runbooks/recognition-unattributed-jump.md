# Runbook: recognition unattributed-shapes jump

**Alert:** `stellarindex_recognition_unattributed_jump` (ticket)

## What this guards

`stellarindex_recognition_unattributed_shapes` counts the distinct
(contract, topic) event shapes in the certified lake that **no enabled
source's decoder claims** — i.e. the rest of the Soroban ecosystem. For
a curated indexer this is EXPECTED to be large (2026-08 census: 22,919
shapes) and slowly growing (~30 shapes/day of new foreign protocols),
which is exactly why the system `recognition` snapshot is excluded from
`stellarindex_completeness_incomplete` (a permanently-red alert trains
operators to ignore the channel).

The alertable signal is a **step change**: a jump of >250 shapes in 2
days is far outside organic drift but well below a typical protocol
family's shape count. The founding failure class is the rozo/BACKLOG-89
blind spot — a protocol we DO index loses its ownership mapping
(`protocol_contracts` seed / `GatedRegistryOptions` warm), its shapes
stop being attributed to the source's own recognition axis (which then
reads **green over a real drop**), and they fall into this census
instead. The jump is the only externally visible symptom.

## When it fires

1. **Census the current shapes:**
   ```sh
   set -a; . /etc/default/stellarindex-ops; set +a
   stellarindex-ops ch-recognition -config /etc/stellarindex.toml -top 60
   ```
   Look at the top new entries (compare `ledgers=[first,last]` — a
   registry regression shows a large-event-count contract whose FIRST
   ledger is old but which is newly unattributed).
2. **Check whether the jumped contracts belong to an enabled source:**
   diff the top contracts against `protocol_contracts` (Postgres) and
   the factory lists logged at completeness startup ("gated registry
   warmed source=… factories=… children=N"). A known factory child
   appearing here = the ownership map lost it.
3. If it is a registry regression: re-seed (`seed-protocol-contracts`),
   confirm the next `compute-completeness` pass re-attributes (the
   count drops back), and verify the affected source's own
   `recognition_ok` — it may have been reading green over a real drop;
   re-derive the affected window if events were skipped.
4. If it is genuinely a new foreign protocol having a busy week: no
   action — note it and let the baseline absorb it. Consider a decoder
   if the protocol is in scope for the product.

## Related

- The per-source recognition axis (real silent-drop alarm for owned
  contracts) still fails that source's own row and alerts via
  `stellarindex_completeness_incomplete`.
- `verify-recognition` (ops) is the per-range deep audit.
