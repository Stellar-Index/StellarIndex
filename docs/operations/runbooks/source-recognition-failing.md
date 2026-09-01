---
title: Runbook — source recognition failing
last_verified: 2026-09-01
status: ratified
severity: P3
---

# Runbook — `stellarindex_source_recognition_failing`

A source **we index** emitted a `(contract_id, topic_0_sym)` shape that
none of its decoders claimed. We are silently dropping events on a
protocol we promised to cover, and that source's ADR-0033 coverage
verdict is capped until it is resolved.

> **Do not confuse this with
> `stellarindex_recognition_unattributed_shapes`.** The ADR-0033
> recognition scan produces two buckets and they mean opposite things:
>
> | bucket | meaning | action |
> | --- | --- | --- |
> | **owned + unrecognised** — *this alert* | a protocol we index emitted something we cannot decode | **investigate** |
> | **unowned + unrecognised** — the unattributed census | someone else's contract, a protocol we don't index | none; it grows forever |
>
> The unattributed number was 23,866 on 2026-09-01 and rises with
> Stellar's own growth. It is a coverage-ambition figure for deciding
> which decoder to build next, not an error. Only the owned bucket is a
> defect.

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_source_recognition_failing` — `configs/prometheus/rules.r1/data-freshness.yml` and `deploy/monitoring/rules/data-freshness.yml` (group `stellarindex.data_freshness`, `severity: ticket`, `for: 1h`) |
| Severity | **P3** — a ticket, not a page. Events are still captured in the ClickHouse lake; what is lost is their projection into a served per-source table, and a replay recovers it once the decoder is fixed. |
| Detected by | `stellarindex_recognition_ok{source="…"} == 0`, emitted by `data-freshness.sh` from the `recognition_ok` column of `completeness_snapshots` (written by the daily `compute-completeness` job). |
| Typical MTTR | Hours to days — it usually needs a decoder arm written, reviewed and released, then a `projector-replay` to recover the missed range. |
| Impact | That source's coverage verdict is capped (`complete=false`), so `/v1/coverage` reports it publicly as incomplete. Served data for the source is missing whatever the unrecognised shape carried. |

## Symptoms

- `/v1/coverage` shows the named source with `recognition_ok: false` and
  `complete: false`.
- The source's `detail` field names the unrecognised shape count.
- Served rows for that protocol stop advancing, or advance with a
  category of event missing, while ingest looks otherwise healthy.

## Quick diagnosis (≤ 5 min)

```sh
# 1. Which source, and what does the verdict say?
curl -s https://api.stellarindex.io/v1/coverage \
  | python3 -c "import sys,json;[print(r['source'], r['recognition_ok'], r['detail'][:200]) for r in json.load(sys.stdin)['data']['sources'] if not r['recognition_ok']]"

# 2. Which shapes are unclaimed? Read-only; run under the wrapper.
/usr/local/sbin/run-heavy-job.sh ch-recognition \
  stellarindex-ops ch-recognition -config /etc/stellarindex.toml -top 60
```

Cross-reference the reported `contract_id`s against the source's
registry (`protocol_contracts`, or its in-code curated set). A shape
whose contract belongs to the alerting source is the one to fix.

## Mitigation (≤ 15 min)

There is no quick mitigation — this needs a decoder change. What you
*can* do immediately is establish blast radius: how many events, over
what ledger range, on which contract. That determines whether the
replay after the fix is minutes or hours.

Do **not** silence the alert to clear the board. The coverage verdict is
capped for a real reason, and `/v1/coverage` is public.

## Root cause analysis

Two causes account for nearly all occurrences:

1. **The contract upgraded in place.** Soroban contracts can
   `update_contract` without changing address, and event body schemas
   and topic shapes can change across that upgrade. Live ingest only
   ever sees the current WASM. See
   [`../../architecture/contract-schema-evolution.md`](../../architecture/contract-schema-evolution.md).
2. **A new event kind shipped** that the decoder has no arm for — the
   protocol added a feature.

Both are fixed the same way: add the decoder arm, gate the backfill
behind a per-WASM-hash audit if the range predates the current WASM
(see [`../wasm-audits/README.md`](../wasm-audits/README.md)), then
`projector-replay` the affected range so the missed events land in the
served tier.

## Known false-positive patterns

- **A newly-admitted contract mid-scan.** If an operator seeds a
  contract into a source's registry between the recognition scan and the
  decoder deploy that understands it, the shape is briefly owned and
  unclaimed. Clears on the next daily run.
- **Not** foreign-protocol growth. That lands in the unattributed census
  and cannot trigger this alert — the shape has to be on a contract the
  source owns.

## Related

- [`completeness-incomplete.md`](completeness-incomplete.md) — the
  broader ADR-0033 verdict this axis feeds.
- [`../../architecture/contract-schema-evolution.md`](../../architecture/contract-schema-evolution.md)
  — why in-place upgrades are the usual cause.
- [`../adr-0033-data-recovery.md`](../adr-0033-data-recovery.md) —
  replaying a range after the decoder is fixed. Note the gated-source
  trap: `backfill` writes nothing and exits 0; use `projector-replay`.
- [`../alerts-catalog.md`](../alerts-catalog.md) — the full inventory.

## Changelog

- 2026-09-01 — replaces `stellarindex_recognition_unattributed_jump`.
  That alert watched the *unowned* bucket and tried to infer a registry
  regression from its growth rate against a ~30/day organic baseline.
  The inference was unreliable in both directions: it fired when Stellar
  was merely busy (361 new shapes in 2 days on 2026-09-01, every one on
  an unowned contract, no source degraded), and a genuine regression
  would have looked identical from the alert alone. The signal it was
  trying to infer — a source's own recognition axis — was already a
  column on `completeness_snapshots` and simply was not exported. So the
  inert bucket had a metric and an alert while the meaningful one had
  neither. This alert reads the meaningful one directly and infers
  nothing.
