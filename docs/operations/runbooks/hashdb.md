---
title: Runbook — hashdb
last_verified: 2026-10-06
status: living
severity: P3
---

# Runbook — hashdb alerts

## At a glance

All alerts are P3 (`component: hashdb`), rules in `deploy/monitoring/rules/hashdb.yml` and `configs/prometheus/rules.r1/hashdb.yml`. Only regions with `[hashdb].enabled = true` (off by default) record anything. Design: [ADR-0016](../../adr/0016-per-region-storage-strategy.md); `internal/hashdb/` records `sha256(LCM)` per ledger on the live read loop, and a sweep (`startHashDBVerifier`, every `[hashdb].verify_interval_minutes`, default 60) re-reads a trailing window and compares. Founding case: ledger 63332650.

Shared commands:

```sh
curl -fs http://<obs.metrics_listen>/metrics | grep '^stellarindex_hashdb_'   # default 127.0.0.1:9464
journalctl -u stellarindex-indexer --since "-2h" | grep -E "hashdb (DRIFT DETECTED|verify sweep|.Append)"
```

## stellarindex_hashdb_drift_detected

Trips on `stellarindex_hashdb_drift_total > 0`, `for: 0m`. The counter is monotonic per process: no auto-resolve until the indexer restarts. Acknowledge after triage, or silence once documented.

Log: ERROR `hashdb DRIFT DETECTED — upstream history rewritten or lake object corrupted` with `verified`, `drifted`, `missing`, `out_of_range`, `drifted_ledgers` (the exact sequences; the counter is a running total, not a list).

Adjudicate each drifted ledger N. SDF archives hold no LCM, so compare ledger-header hashes instead:

```sh
# 1. Re-check through the same marshal path. Non-zero exit + "hashdb DRIFT DETECTED" log = still drifted.
stellarindex-indexer -config /etc/stellarindex.toml -verify-hashdb-from <N> -verify-hashdb-to <N>

# 2. ORIGINAL header hash = what we first ingested.
curl -s 'http://127.0.0.1:8123/' --data-binary \
  "SELECT ledger_hash FROM stellar.ledgers FINAL WHERE ledger_seq = <N>"

# 3. SDF header hash: checkpoint file for C = N | 63 (64 LedgerHeaderHistoryEntry records, C-63..C); take entry N.
#    Path <root>/ledger/XX/YY/ZZ/ledger-<hex8(C)>.xdr.gz (internal/archivecompleteness/cross_anchor.go: checkpointPath;
#    parsing: readArchivedLedgerHash in internal/ops/archive/verify_archive.go).
```

The bucket object's CURRENT header hash has no single-ledger command: decode it with a throwaway Go snippet (`lcm.LedgerHash()`, as `internal/storage/clickhouse/extract.go`) or run `verify-archive` over a range containing it and read the mismatch output.

| Finding (current = bucket object, SDF, original = ClickHouse) | Cause | Next |
| --- | --- | --- |
| Re-running `-verify-hashdb-from/-to` reports no drift | Transient read glitch | Note it; escalate if it recurs on the SAME ledger |
| current = SDF = original, LCM hash still drifts | Change is metadata-only (SDF cannot adjudicate), or hashdb's own record is bad (see false positives) | Compare against the B2 lake copy or ClickHouse raw tables for that ledger; escalate if they differ |
| current != SDF | Local corruption: our copy is bad | Re-fetch that object from a known-good source (SDF / peer), replace it in the local bucket; general procedure in [archival-node-bringup](../archival-node-bringup.md) DR triage tree. Then restart the indexer or wait for the next tick and confirm drift clears |
| original != SDF, current = SDF | We ingested a bad object once; it has since self-healed (Galexie re-upload, cold-tier refresh) | Detector worked on our own history; note in postmortem, no ongoing risk |
| original = SDF, current != both | Upstream rewrote history (the ADR-0016 failure mode) | Serious: data-provenance incident. Document which downstream data (trades, prices, completeness snapshots) derived from the ledger(s), check whether any was served, decide with the team whether a targeted re-derive is warranted (`docs/operations/wasm-audits/`-style evidence trail; see `docs/operations/maintainer-workflow.md` "Heavy one-shot jobs"). No wide unwindowed re-derive without the `run-heavy-job.sh` wrapper |

False positives:
- A torn write to the hashdb file (disk full mid-Append, power loss) looks like drift (signature: current = SDF = original). Recreate the hashdb file and let it re-populate. Records straddling a 4 KiB page can tear on non-CoW filesystems (ext4/xfs; INV-2592); r1 is on ZFS and cannot.
- A ledger re-appended after restart with different bytes is impossible; it means a bug in `openOrCreateHashDB` or a hand-edited / inconsistently restored hashdb file.

## stellarindex_hashdb_verify_failing

Trips:
```
sum(rate(stellarindex_hashdb_verify_runs_total{window="recent",outcome="error"}[6h]))
  >
sum(rate(stellarindex_hashdb_verify_runs_total{window="recent",outcome=~"ok|drift"}[6h]))
and
sum(rate(stellarindex_hashdb_verify_runs_total{window="recent",outcome=~"error|ok|drift"}[6h])) > 0
```
`for: 30m`. The sweep cannot complete: the detector is blind. Causes:
- Live bucket lacks the ledgers the window references (indexer mid-catch-up). Self-heals at the live tip.
- `[hashdb].path` filesystem went read-only / full.
- Bucket unreachable (check `galexie-archive-tip-lag`, [runbook](archive.md#stellarindex_galexie_archive_tip_lag_high), and MinIO connectivity).
- **An in-window object was rewritten to bytes that no longer XDR-decode, or deleted.** Potentially the tamper class. The sweep aborts at the first bad object; later ledgers go unverified. A decode failure or "object ... is missing" for an already-ingested ledger warrants the three-way comparison above.

Diagnose: `journalctl -u stellarindex-indexer | grep -E "hashdb verify sweep (failed|incomplete)"` (`incomplete` = stream ended early or no ledger had a recorded baseline: triage like a missing object). Fix the connectivity/disk cause; the sweep retries every 60 minutes with no operator action. If the error names a specific in-window object, escalate per the drift section.

## stellarindex_hashdb_verify_stale

Trips:
```
sum(increase(stellarindex_hashdb_append_total[6h])) > 0
and
absent_over_time(stellarindex_hashdb_verify_runs_total[6h])
```
`for: 30m`. Append has recorded for 6h but `stellarindex_hashdb_verify_runs_total` has no samples: the verifier completed no pass at all (`verify_failing` cannot see this). Cause: every tick took `hashDBVerifySweep`'s early return, or the ticker goroutine died. Check the indexer is alive and `journalctl -u stellarindex-indexer | grep hashdb` for startup/panic. Known gap: `absent_over_time` cannot fire while the counter is seeded at 0.

## stellarindex_hashdb_append_failing

Trips:
```
sum(rate(stellarindex_hashdb_append_total{outcome="error"}[15m]))
  >
sum(rate(stellarindex_hashdb_append_total{outcome="ok"}[15m]))
and
sum(rate(stellarindex_hashdb_append_total{outcome=~"error|ok"}[15m])) > 0
```
`for: 10m`. `hashdb.Append` is failure-tolerant (never stalls ingest), so this is the ONLY signal the write side went silent; ledgers appended meanwhile report Missing, not Drifted. Causes: disk full or permission error on `[hashdb].path`; out-of-range ledger sequence (file made for another region/window). Diagnose: `journalctl -u stellarindex-indexer | grep "hashdb.Append"`. Fix the disk/permission issue; Append retries next ledger.

## Related

- `internal/hashdb/`; `internal/archivecompleteness/hashdb_verify.go` (`HashDBWindowVerifier`).
- [archive-files-missing](archive.md#stellarindex_archive_files_missing): ADR-0017 presence gaps (this family is content fidelity).
