---
title: Runbook — stellarindex_oracle_stream_rows_unparsed
last_verified: 2026-08-31
status: ratified
severity: P3
---

# Runbook — `stellarindex_oracle_stream_rows_unparsed`

## At a glance

| | |
| --- | --- |
| **Severity** | ticket (P3) |
| **Fires when** | any `oracle_updates` row is dropped from the served stream because its stored `asset`/`quote` text will not parse |
| **Effect** | those rows are **absent** from `/v1/oracle/streams` and the explorer `/oracles` page |
| **Metric** | `stellarindex_oracle_stream_rows_unparsed_total{source,field}` |

**Why this exists.** The read has always dropped an unparseable row —
there is nothing sane to serve for an asset we cannot name — but it used
to do so with **no log, metric or error** (wave-D SI-OC-04). The row
simply vanished.

That is worst at the moment it is most likely. The documented
remediation for a mislabelled oracle row is an **operator-run raw SQL
`UPDATE`** against this very column, and the column has **no `CHECK`
constraint**. A typo therefore deleted the row from the served surface
rather than erroring — and the operator would watch it disappear and
reasonably conclude the relabel had worked.

## Quick diagnosis (≤ 5 min)

`{{ $labels.field }}` tells you which column; `{{ $labels.source }}`
which oracle.

```sql
SELECT DISTINCT source, asset, quote
  FROM oracle_updates
 WHERE source = '<source>'
 ORDER BY 1,2,3;
```

Compare against the forms `internal/canonical` accepts:

| Form | Example |
| --- | --- |
| native | `native` |
| classic | `USDC-GA5ZSEJ…` (code, dash, 56-char G-strkey) |
| Soroban | `CA…` (56-char C-strkey) |
| fiat | `fiat:USD` |
| crypto | `crypto:BTC` |
| unmapped oracle symbol | `raw:<symbol>` |

Most likely mistakes, in order: a truncated or lower-cased strkey; a
missing prefix (`USD` instead of `fiat:USD`); a stray space or quote
from a shell-built `UPDATE`; `rwa:`/`raw:` confusion — those differ by
one letter.

## Mitigation

1. Correct the row with a **parameterised** update, not string
   concatenation.
2. Re-read `/v1/oracle/streams` and confirm the row is back — the alert
   clearing only means no *new* drops in the window, not that the
   existing ones were repaired.
3. If the text is correct and the parser rejects it, that is the more
   serious case: a canonical form changed under the running binary.
   Check the deployed version against `internal/canonical` before
   editing any data.

## When NOT to act

- Immediately after a deliberate schema/namespace migration, a burst is
  expected while rows are rewritten. It should stop; if it does not, the
  rewrite missed rows.

## Related

- [`oracle-unknown-symbols.md`](oracle-unknown-symbols.md) — the *other* direction: a symbol that parses fine but maps to no canonical asset.
- [`price-divergence.md`](price-divergence.md) — where missing oracle rows eventually show up.
