---
title: Runbook — hashdb
last_verified: 2026-10-06
status: living
severity: P3
---

# Runbook — hashdb alerts

All five alerts are `severity: ticket` (P3), `component: hashdb`, same rule file in both trees (`deploy/monitoring/rules/hashdb.yml`, `configs/prometheus/rules.r1/hashdb.yml`; only the header comments differ). Only regions with `[hashdb].enabled = true` (off by default) record anything. Served data is not automatically wrong on a drift; P3 until the detector has a production track record.

ADR-0016: regions reading galexie data from a non-local bucket can see upstream retroactively rewrite a ledger's bytes, still chain-consistent and matching SDF history. `internal/hashdb` records `sha256(LCM)` per ledger on the indexer's live read loop (`recordHashdb`); a periodic sweep (`startHashDBVerifier`, `cmd/stellarindex-indexer/main.go`) re-reads a trailing window from the same bucket and compares. Sweep interval `[hashdb].verify_interval_minutes` (default 60). Founding case: ledger 63332650.

Shared commands:

```sh
curl -fs http://<obs.metrics_listen>/metrics | grep '^stellarindex_hashdb_'   # default 127.0.0.1:9464
journalctl -u stellarindex-indexer --since "-2h" | grep -E "hashdb (DRIFT DETECTED|verify sweep|.Append)"
```

## At a glance

- [`stellarindex_hashdb_drift_detected`](#stellarindex_hashdb_drift_detected)
- [`stellarindex_hashdb_verify_failing`](#stellarindex_hashdb_verify_failing)
- [`stellarindex_hashdb_history_verify_failing`](#stellarindex_hashdb_history_verify_failing)
- [`stellarindex_hashdb_verify_stale`](#stellarindex_hashdb_verify_stale)
- [`stellarindex_hashdb_append_failing`](#stellarindex_hashdb_append_failing)

## stellarindex_hashdb_drift_detected

Trips: `stellarindex_hashdb_drift_total > 0`, `for: 0m`. The counter is monotonic per process: it stays tripped until the indexer restarts; there is no auto-resolve. Investigation-bound; acknowledge the ticket after triage, or silence in AlertManager once the finding is documented.

Log: ERROR `hashdb DRIFT DETECTED — upstream history rewritten or lake object corrupted` with `verified`, `drifted`, `missing`, `out_of_range`, `drifted_ledgers` (the exact sequences; the counter is a running total, not a list).

Adjudicate each drifted ledger N. hashdb hashes `sha256(lcm.MarshalBinary())` (re-marshaled canonical XDR); SDF archives hold headers, tx sets and results, never an LCM, so SDF cannot confirm that hash. Compare ledger-header hashes instead (hashing raw compressed bytes would false-alarm on re-compression):

```sh
# 1. Re-check through the same marshal path. Non-zero exit + "hashdb DRIFT DETECTED" log = still drifted.
stellarindex-indexer -config /etc/stellarindex.toml -verify-hashdb-from <N> -verify-hashdb-to <N>

# 2. ORIGINAL header hash = what we first ingested.
curl -s 'http://127.0.0.1:8123/' --data-binary \
  "SELECT ledger_hash FROM stellar.ledgers FINAL WHERE ledger_seq = <N>"

# 3. SDF header hash: checkpoint file for C = N | 63 (64 LedgerHeaderHistoryEntry records, C-63..C); take entry N.
#    Path <root>/ledger/XX/YY/ZZ/ledger-<hex8(C)>.xdr.gz, hex8 = %08x of C, XX/YY/ZZ = its first three bytes
#    (internal/archivecompleteness/cross_anchor.go: checkpointPath; parsing: readArchivedLedgerHash in
#    internal/ops/archive/verify_archive.go).
```

The bucket object's CURRENT header hash has no single-ledger command: `verify-archive` (`-tier` A/B/D/E/sdf-sample) and `verify-decoders` check ranges/checkpoints and do not print one ledger's header hash. Decode the object with a throwaway Go snippet (`lcm.LedgerHash()`, as `internal/storage/clickhouse/extract.go`) or run `verify-archive` over a range containing it and read the mismatch output.

| Finding (current = bucket object, SDF, original = ClickHouse) | Cause | Next |
| --- | --- | --- |
| Re-running `-verify-hashdb-from/-to` reports no drift | Transient read glitch | Note it; escalate if it recurs on the SAME ledger |
| current = SDF = original, LCM hash still drifts | Change is metadata-only (SDF cannot adjudicate), or hashdb's own record is bad (see false positives) | Compare against the B2 lake copy or ClickHouse raw tables for that ledger; escalate if they differ |
| current != SDF | Local corruption: our copy is bad | Re-fetch that object from a known-good source (SDF / peer), replace it in the local bucket; general procedure in [archival-node-bringup](../archival-node-bringup.md) DR triage tree. Then restart the indexer or wait for the next tick and confirm drift clears |
| original != SDF, current = SDF | We ingested a bad object once; it has since self-healed (Galexie re-upload, cold-tier refresh) | Detector worked on our own history; note in postmortem, no ongoing risk |
| original = SDF, current != both | Upstream rewrote history (the ADR-0016 failure mode) | Serious: data-provenance incident. Document which downstream data (trades, prices, completeness snapshots) derived from the ledger(s), check whether any was served, decide with the team whether a targeted re-derive is warranted (`docs/operations/wasm-audits/`-style evidence trail; see `docs/operations/maintainer-workflow.md` "Heavy one-shot jobs"). No wide unwindowed re-derive without the `run-heavy-job.sh` wrapper |

RCA to capture: drifted sequence(s), the three-way comparison per ledger, which region(s) observed it, whether served pricing/trade data derived from them before detection.

False positives:
- A torn write to the hashdb file itself (disk full mid-Append, power loss) looks like drift but our record is wrong. Signature: current = SDF = original. Recreate the hashdb file (fresh `Create`, let it re-populate); earlier entries become un-verifiable, not proof of anything. A record is 32 bytes after a 16-byte header, so 1 in 128 straddles a 4 KiB page and can tear on a non-CoW filesystem (ext4/xfs), then reads as drift permanently (INV-2592). r1 runs on ZFS (cannot tear); R2/R3 need the format change first.
- A ledger re-appended after restart with different valid bytes should be impossible (closed-ledger bytes are immutable); it would indicate a bug in `openOrCreateHashDB`'s "open existing, don't overwrite" logic. Check whether the hashdb file was manually edited or restored from an inconsistent backup.

## stellarindex_hashdb_verify_failing

Trips:
```
sum(rate(stellarindex_hashdb_verify_runs_total{window="recent",outcome="error"}[6h]))
  >
sum(rate(stellarindex_hashdb_verify_runs_total{window="recent",outcome=~"ok|drift"}[6h]))
and
sum(rate(stellarindex_hashdb_verify_runs_total{window="recent",outcome=~"error|ok|drift"}[6h])) > 0
```
`for: 30m`. The sweep itself cannot complete: the detector is blind. Causes:
- Live bucket lacks the ledgers the window references (indexer mid-catch-up from a large backfill; see "Known limitation" in `startHashDBVerifier`'s doc comment). Self-heals at the live tip.
- `[hashdb].path` filesystem went read-only / full.
- Bucket unreachable (check `galexie-archive-tip-lag`, [runbook](galexie-archive.md#stellarindex_galexie_archive_tip_lag_high), and MinIO connectivity).
- **An in-window object was rewritten to bytes that no longer XDR-decode, or deleted.** Potentially the tamper class, not blindness. The sweep streams strictly (missing objects are errors) and aborts at the first bad object; later ledgers in the window went unverified. A decode failure or "object … is missing" naming an already-ingested ledger (not mid-catch-up) warrants the three-way comparison above. If drift was tallied before the stream error, the run records `outcome="drift"` (stream error attached to the log), not `error`.

Diagnose: `journalctl -u stellarindex-indexer | grep -E "hashdb verify sweep (failed|incomplete)"` (WARN includes the error; `incomplete` = stream ended early without error, or no ledger in the window had a recorded baseline: triage like a missing object). Fix the connectivity/disk cause; the sweep retries every `verify_interval_minutes` with no operator action. If the error names a specific in-window object, escalate per the drift section.

## stellarindex_hashdb_history_verify_failing

Trips:
```
sum(increase(stellarindex_hashdb_verify_runs_total{window="history",outcome="error"}[6h])) > 0
unless
sum(increase(stellarindex_hashdb_verify_runs_total{window="history",outcome=~"ok|drift"}[6h])) > 0
```
`for: 30m`. Each sweep also re-verifies a random 1000-ledger slice of pre-window history from the archive bucket (`window="history"`); this means 6h (6 hourly sweeps) with at least one error and no ok/drift. `hashdb_verify_failing` cannot see it (the healthy recent window's ok runs mask it). Find the slice in the indexer log (`slice=history`, with `seed`, `from`, `to`), then re-run it:
`stellarindex-indexer -config /etc/stellarindex.toml -verify-hashdb-from FROM -verify-hashdb-to TO`.
Usual causes: archive bucket unreachable or missing objects for that range, or no ledger in the slice has a recorded baseline.

## stellarindex_hashdb_verify_stale

Trips:
```
sum(increase(stellarindex_hashdb_append_total[6h])) > 0
and
absent_over_time(stellarindex_hashdb_verify_runs_total[6h])
```
`for: 30m`. Append has been recording for 6h (hashdb enabled, ingest live) but `stellarindex_hashdb_verify_runs_total` has had zero samples: the verifier completed no pass at all (not clean, drifted or errored). `verify_failing` cannot catch it: both sides of its ratio are zero and its `> 0` guard reads that as no signal. Cause: every tick took `hashDBVerifySweep`'s early return, or the ticker goroutine died. Check the indexer process is alive and its logs for hashdb verifier startup/panic (`journalctl -u stellarindex-indexer | grep hashdb`).

## stellarindex_hashdb_append_failing

Trips:
```
sum(rate(stellarindex_hashdb_append_total{outcome="error"}[15m]))
  >
sum(rate(stellarindex_hashdb_append_total{outcome="ok"}[15m]))
and
sum(rate(stellarindex_hashdb_append_total{outcome=~"error|ok"}[15m])) > 0
```
`for: 10m`. `hashdb.Append` runs once per ledger on the live LCM read loop and is deliberately failure-tolerant (logs, increments the counter, never stalls ingest; see `recordHashdb`), so this counter is the ONLY signal the write side went silent. The sweep then has nothing to compare against: it reports Missing, not Drifted, for ledgers appended while this fires, even if real drift occurred. Causes: disk full or permission error on `[hashdb].path`; out-of-range ledger sequence (hashdb file created for a different region/window). Diagnose: `journalctl -u stellarindex-indexer | grep "hashdb.Append"`. Fix the disk/permission issue; Append retries next ledger.

## Related

- [ADR-0016](../../adr/0016-per-region-storage-strategy.md); `internal/hashdb/` (format + `Append`/`Verify`); `internal/archivecompleteness/hashdb_verify.go` (`HashDBWindowVerifier`: Verified/Drifted/Missing/OutOfRange).
- [archive-files-missing](archive-completeness.md#stellarindex_archive_files_missing): ADR-0017 presence gaps (this family is content fidelity).
