---
title: Runbook — hashdb-drift-detected
last_verified: 2026-10-05
status: living
severity: P3
---

# Runbook — `stellarindex_hashdb_drift_detected`

This runbook also covers the companion alerts
`stellarindex_hashdb_verify_failing` and `stellarindex_hashdb_append_failing`
(same rule file, same underlying worker) — see
[Companion alert: hashdb_verify_failing](#companion-alert-hashdb_verify_failing)
and [Companion alert: hashdb_append_failing](#companion-alert-hashdb_append_failing)
below.

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_hashdb_drift_detected` |
| Severity | P3 (ticket) — see [Why not P1/P2](#why-not-p1p2) |
| Detected by | `deploy/monitoring/rules/hashdb.yml` (and `configs/prometheus/rules.r1/hashdb.yml`) |
| Typical MTTR | Investigation-bound — not a "fix and clear" alert. The counter is monotonic (never auto-resolves); closing the ticket is a human judgment call once the drifted ledger(s) are understood. |
| Impact | Data-integrity concern, not an outage. Served prices/rates are unaffected UNLESS the drifted ledger fed into pricing that hasn't yet been reconciled — see Investigate. Only fires on regions that opted in (`[hashdb].enabled = true`, off by default). |

## Why this exists

ADR-0016's trust model: regions reading galexie data from a
non-local bucket (R2 from the AWS public bucket, R3 from Vultr
Object Storage) are exposed to a failure mode R1's full-mirror
shape isn't — **upstream can retroactively rewrite a previously-
fetched ledger's bytes**. The rewritten bytes can still be
internally consistent (the chain-link hash still holds) and can
still match SDF's signed history (Tier A + Tier D checks pass), yet
differ from what the region first observed. Only a fingerprint of
what we *originally saw* catches that.

`internal/hashdb` is that fingerprint: the indexer's live LCM read
loop appends `sha256(LCM)` per ledger as it ingests; a periodic sweep
(also in the indexer, see `startHashDBVerifier` in
`cmd/stellarindex-indexer/main.go`) re-reads a trailing window of
ledgers from the SAME bucket and compares. **The founding case**:
ledger 63332650 — an upstream-corrupt ledger object, discovered
2026-07-08, that motivated wiring this detector into production (it
had existed as a library with zero callers before then). A re-ingest
replaying that ledger's history is exactly the scenario hashdb is
built to catch.

## Symptoms

- `stellarindex_hashdb_drift_total > 0` (any nonzero value — the
  counter only ever increases within a process lifetime, so once
  tripped it stays tripped until the indexer restarts).
- Indexer log line at ERROR level: `hashdb DRIFT DETECTED — upstream
  history rewritten or lake object corrupted`, with `verified`,
  `drifted`, `missing`, `out_of_range`, and `drifted_ledgers` fields
  — the last one names the exact sequences.

## Quick diagnosis (≤ 5 min)

```sh
# 1. Confirm the alert + get the drifted-ledger count.
curl -fs http://localhost:<obs.metrics_listen-port>/metrics \
  | grep '^stellarindex_hashdb_'

# 2. Find the exact drifted ledger sequence(s) — the metric alone
#    doesn't carry them; the log line does.
journalctl -u stellarindex-indexer --since "-2h" \
  | grep "hashdb DRIFT DETECTED"

```

## Adjudicate a drifted ledger

hashdb records `sha256(lcm.MarshalBinary())`: a hash of the re-marshaled
canonical XDR of the decoded `LedgerCloseMeta`. SDF's history archive
holds ledger headers, transaction sets and results, never an LCM, so
**SDF cannot confirm or refute that hash directly**. Adjudicate at
ledger-header level instead, where SDF can answer. The hash definition
stays as is: Galexie objects are zstd-compressed, and hashing the raw
compressed bytes would false-alarm whenever a file is re-compressed.

```sh
# 1. Re-check the drift through the same marshal path (one ledger).
#    Non-zero exit plus a "hashdb DRIFT DETECTED" log line = still drifted.
stellarindex-indexer -config /etc/stellarindex.toml \
  -verify-hashdb-from <N> -verify-hashdb-to <N>

# 2. Header hash in ClickHouse = what we first ingested.
curl -s 'http://127.0.0.1:8123/' --data-binary \
  "SELECT ledger_hash FROM stellar.ledgers FINAL WHERE ledger_seq = <N>"

# 3. Header hash in SDF's checkpoint file. The file for ledger N is the
#    one for its checkpoint C = N | 63 (it holds 64 LedgerHeaderHistoryEntry
#    records, C-63..C); take the entry whose ledger seq is N.
#    Path: <root>/ledger/XX/YY/ZZ/ledger-<hex8(C)>.xdr.gz, hex8 = %08x of C,
#    XX/YY/ZZ = its first three bytes
#    (internal/archivecompleteness/cross_anchor.go: checkpointPath).
#    Parsing reference: readArchivedLedgerHash in
#    internal/ops/archive/verify_archive.go.
```

The bucket object's CURRENT header hash (the `LedgerHeaderHistoryEntry`
inside the LCM) has **no single-ledger command today**: `verify-archive`
(`-tier` A/B/D/E/sdf-sample) and `verify-decoders` check ranges and
checkpoints, they do not print one ledger's header hash. Decode the
object with a throwaway Go snippet (`lcm.LedgerHash()`, as
`internal/storage/clickhouse/extract.go` does), or run `verify-archive`
over a range that contains the ledger and read its mismatch output.
Do not invent a command here.

## Decision tree

Compare three header hashes for the ledger: **current** (bucket object),
**SDF** (checkpoint file), **original** (ClickHouse `stellar.ledgers.ledger_hash`,
what we first saw). First re-run the verify command above.

| What you find | Likely cause | Next step |
| -------------- | ------------ | --------- |
| Re-running `-verify-hashdb-from/-to` now reports no drift | Transient read glitch (partial read, bit flip in transit) | No action beyond noting it; if it recurs on the SAME ledger, escalate |
| Current = SDF = original, but the LCM hash still drifts | The change is in the metadata only (not the header), which SDF cannot adjudicate | Compare the object against the B2 lake copy or the ClickHouse raw tables for that ledger; escalate if they differ |
| Current != SDF | **Local corruption** — our copy is bad, upstream's isn't | Re-fetch that ledger's object from a known-good source (SDF / peer) and re-place it in the local bucket; see the archival-node-bringup runbook's disaster-recovery triage tree for the general "corrupt-in-place" procedure |
| Original != SDF, current = SDF | **We ingested a bad object once** and it has since self-healed (Galexie re-upload, cold-tier refresh) | Confirms the detector worked on our OWN historical bad data, not an upstream rewrite. Note in the postmortem; no ongoing risk |
| Original = SDF, current != both | **Upstream rewrote history** — the exact ADR-0016 failure mode this detector exists for | This is the serious case. Escalate: the region may be silently serving/have served data derived from bytes SDF never signed. Check whether any pricing/trade data derived from the drifted ledger has already been served, and whether a re-derive is warranted (`docs/operations/wasm-audits/`-style evidence trail; coordinate before any bulk re-derive — see docs/operations/maintainer-workflow.md, "Heavy one-shot jobs") |

## Mitigation

- [ ] **Identify every drifted ledger** from the log line(s) (do NOT
      rely on the counter value alone — it's a running total, not a
      list).
- [ ] **Triage each one** against the decision tree above — this
      determines whether it's local corruption, a stale historical
      recording, or a genuine upstream rewrite.
- [ ] **For local corruption**: re-fetch the correct bytes from a
      known-good source and replace the local copy. Re-run the
      indexer's verify window (restart, or wait for the next tick)
      to confirm the drift clears.
- [ ] **For a genuine upstream rewrite**: this is a data-provenance
      incident, not a quick fix. Document which downstream data
      (trades, prices, completeness snapshots) derived from the
      affected ledger(s) and decide with the team whether a targeted
      re-derive is warranted. Do NOT run a wide unwindowed re-derive
      without the `run-heavy-job.sh` wrapper (AGENTS.md).
- [ ] There is no automatic "resolve" for this alert — `[hashdb]`'s
      drift counter is a lifetime total for the process. Acknowledge
      the ticket once triage is complete; the alert clears on the
      next indexer restart (fresh counter) or can be silenced
      manually in AlertManager once the finding is documented.

## Companion alert: `hashdb_verify_failing`

Same rule file, `stellarindex_hashdb_verify_runs_total{outcome="error"}`
dominating `{outcome=~"ok|drift"}` over 6h, sustained 30 min. This
means the periodic verify sweep ITSELF can't complete — the detector
has gone blind, as opposed to having found something. Common causes:

- The live bucket doesn't hold the ledgers the sweep's window
  references yet (the indexer is mid-catch-up from a large historical
  backfill — see the "Known limitation" note in
  `startHashDBVerifier`'s doc comment in
  `cmd/stellarindex-indexer/main.go`). Self-heals once the indexer
  reaches the live tip.
- `[hashdb].path` points at a filesystem that went read-only / ran out
  of space.
- The bucket itself is unreachable (same underlying cause as a
  `galexie-archive-tip-lag` / MinIO connectivity incident — check
  those alerts too).
- **An in-window object was rewritten to bytes that no longer
  XDR-decode, or was deleted outright.** This is NOT blindness — it
  is potentially the tamper/corruption class this detector exists
  for, surfacing under the "couldn't check" alert because a garbage
  or absent object can't be hashed at all. The sweep streams the
  window strictly (missing objects are errors, never tolerated) and
  aborts at the first bad object, so ledgers after it in the window
  went unverified this pass. Read the WARN's error text BEFORE
  assuming connectivity: a decode failure or "object … is missing"
  naming a ledger the indexer already ingested (i.e. NOT mid-catch-up)
  warrants the same three-way comparison as a drift hit. Note the
  sweep reports drift with priority: if any drift was tallied before
  the stream error, the run records `outcome="drift"` (with the
  stream error attached to the log line), not `error`.

Diagnosis: `journalctl -u stellarindex-indexer | grep -E "hashdb verify
sweep (failed|incomplete)"` — the WARN line includes the underlying
error (`incomplete` means the stream ended early without erroring:
same triage as a missing object).
Mitigation: fix the underlying connectivity/disk issue; the sweep
retries every `[hashdb].verify_interval_minutes` (default 60) with no
operator action needed once the cause clears. If the error text
points at a specific in-window object instead, escalate per the
drift checklist above.

## Companion alert: `hashdb_append_failing`

Same rule file, `stellarindex_hashdb_append_total{outcome="error"}`
dominating `{outcome="ok"}` over 15m, sustained 10 min. `hashdb.Append`
runs once per ledger on the indexer's live LCM read loop and is
deliberately failure-tolerant (an error logs + increments this counter
and never stalls or fails ingest — see `recordHashdb`'s docstring in
`cmd/stellarindex-indexer/main.go`), so this counter is the ONLY
operator-visible signal that the write side has gone silent. Unlike
`hashdb_verify_failing` (the periodic sweep can't check), this means
the sweep has nothing recorded to check AGAINST: it will report
Missing, not Drifted, for any ledger appended while this alert is
firing, even if real drift occurred.

Common causes: disk full or permission error on `[hashdb].path`, or an
out-of-range ledger sequence (a hashdb file created for a different
region/window). Diagnosis: `journalctl -u stellarindex-indexer | grep
"hashdb.Append"` for the underlying error. Mitigation: fix the
underlying disk/permission issue — Append retries on the next ledger
with no operator action needed once the cause clears.

## Why not P1/P2?

This is a brand-new, first-production-exposure detector (signed off
2026-07-08) with zero prior production track record. Drift is a
serious data-integrity signal, but:

1. It's off by default and only affects regions that explicitly
   opted in.
2. It is NOT customer-facing on its own — served data isn't
   automatically wrong just because one historical ledger drifted;
   most of what it will catch (based on the founding incident) is
   local/upstream object corruption that self-heals or needs a
   narrow, deliberate fix, not urgent firefighting.
3. Ticket severity gets prompt, deliberate investigation (this
   runbook) without training on-call to treat an unproven signal as
   an emergency. Revisit this severity once the detector has a real
   production track record — if it proves reliable and drift turns
   out to correlate with actual served-data incidents, escalate to
   P2/P1 in a follow-up PR.

## Root cause analysis

For the postmortem, capture: the drifted ledger sequence(s), the
three-way header-hash comparison (current bucket object / SDF checkpoint /
ClickHouse original) for each, which region(s) observed it,
and whether any served pricing/trade data derived from the affected
ledger(s) before detection.

## Known false-positive patterns

- **hashdb file corruption is indistinguishable from real drift from
  this alert alone** — a torn write to the hashdb file itself (disk
  full mid-Append, power loss) would also show up as a "mismatch" on
  next Verify, but it's OUR record that's wrong, not the source data.
  The header-hash comparison in the decision tree disambiguates
  this: if current = SDF = original, hashdb's own record
  is the thing that's wrong (recreate the hashdb file from a fresh
  `Create` + let it re-populate; historical entries before that point
  are simply un-verifiable, not proof of anything). A record is 32
  bytes after a 16-byte header, so 1 record in 128 straddles a 4 KiB
  page and a crash can tear it on a non-CoW filesystem (ext4/xfs); the
  torn record reads as drift permanently (INV-2592). r1 runs on ZFS,
  which cannot tear it; R2/R3 need the format change first.
- **A ledger the indexer re-appended after a restart** with genuinely
  different bytes than its first observation, where BOTH are valid
  (this shouldn't happen for a single ledger sequence under normal
  Stellar consensus — a closed ledger's bytes are immutable by
  protocol design — but a bug in `openOrCreateHashDB`'s "open
  existing, don't overwrite" logic could in principle re-Append over
  a stale/wrong record). If suspected, check whether the hashdb file
  was ever manually edited or restored from an inconsistent backup.

## Related

- [ADR-0016](../../adr/0016-per-region-storage-strategy.md) — the
  per-region trust model this detector implements.
- `internal/hashdb/` — the on-disk format + `Append`/`Verify` library
  (package doc has the full file-format + concurrency contract).
- `internal/archivecompleteness/hashdb_verify.go` — the
  transport-agnostic `HashDBWindowVerifier` that tallies
  Verified/Drifted/Missing/OutOfRange for a window.
- [archive-files-missing](archive-files-missing.md) — the sibling
  ADR-0017 alert for archive *presence* gaps (this alert is about
  content *fidelity*, a different failure mode).
- [galexie-archive-tip-lag](galexie-archive-tip-lag.md) — check this
  if `hashdb_verify_failing` correlates with a bucket-connectivity
  incident.
- [archival-node-bringup.md](../archival-node-bringup.md) — disaster-
  recovery triage tree for the "our local copy is corrupt" mitigation
  path.

## Changelog

- 2026-10-05 — drift adjudication rewritten at header level (SDF holds no LCM); torn-record note (INV-2593, INV-2592).
- 2026-07-09 — initial draft alongside wiring hashdb into production
  (ADR-0016, ROADMAP #46). Founding case: ledger 63332650.
