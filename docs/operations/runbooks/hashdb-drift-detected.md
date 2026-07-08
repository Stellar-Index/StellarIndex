---
title: Runbook — hashdb-drift-detected
last_verified: 2026-07-09
status: living
severity: P3
---

# Runbook — `stellarindex_hashdb_drift_detected`

This runbook also covers the companion alert
`stellarindex_hashdb_verify_failing` (same rule file, same
underlying worker) — see [Companion alert](#companion-alert-hashdb_verify_failing)
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

# 3. For each drifted ledger, compare against a THIRD, independent
#    source (not the bucket hashdb already compared against) — SDF's
#    public history archive is the natural anchor:
curl -fsS "https://history.stellar.org/prd/core-live/core_live_001/ledger/<XX>/<YY>/<ZZ>/ledger-<hex8>.xdr.gz" \
  -o /tmp/sdf-ledger.xdr.gz
# (XX/YY/ZZ/hex8 per the checkpoint-path scheme in
#  internal/archivecompleteness/cross_anchor.go's checkpointPath —
#  the drifted seq needs to be rounded to its checkpoint if you want
#  the SDF anchor directly; for a byte-exact single-ledger compare,
#  re-fetch the object from the SAME galexie bucket path instead and
#  diff manually.)
```

## Decision tree

| What you find | Likely cause | Next step |
| -------------- | ------------ | --------- |
| The bucket object's CURRENT bytes hash-match hashdb's recorded value on re-fetch (i.e. re-running the verify window now finds no drift) | Transient read glitch (partial read, bit flip in transit) — not a real rewrite | No action beyond noting it; if it recurs on the SAME ledger, escalate |
| The bucket object's CURRENT bytes differ from hashdb's recorded value AND differ from SDF's signed history for that ledger | **Local corruption** — our copy is bad, upstream's isn't | Re-fetch that ledger's object from a known-good source (SDF / peer) and re-place it in the local bucket; see the archival-node-bringup runbook's disaster-recovery triage tree for the general "corrupt-in-place" procedure |
| The bucket object's CURRENT bytes differ from hashdb's recorded value AND MATCH SDF's signed history — but hashdb's recorded (ORIGINAL) value does NOT match SDF | **We recorded a bad value originally** — the indexer ingested a corrupt object once, and it has since self-healed (Galexie re-upload, cold-tier refresh) | Confirms the detector worked as intended on our OWN historical bad data, not an upstream rewrite. Note in the postmortem; no ongoing risk |
| The bucket object's CURRENT bytes differ from hashdb's recorded (ORIGINAL) value AND the ORIGINAL value is what matches SDF | **Upstream rewrote history** — the exact ADR-0016 failure mode this detector exists for | This is the serious case. Escalate: the region may be silently serving/have served data derived from bytes SDF never signed. Check whether any pricing/trade data derived from the drifted ledger has already been served, and whether a re-derive is warranted (`docs/operations/wasm-audits/`-style evidence trail; coordinate before any bulk re-derive — see CLAUDE.md "Heavy one-shot jobs on r1") |

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
      without the `run-heavy-job.sh` wrapper (CLAUDE.md).
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

Diagnosis: `journalctl -u stellarindex-indexer | grep "hashdb verify
sweep failed"` — the WARN line includes the underlying error.
Mitigation: fix the underlying connectivity/disk issue; the sweep
retries every `[hashdb].verify_interval_minutes` (default 60) with no
operator action needed once the cause clears.

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
three-way comparison (hashdb's recorded value / the bucket's current
value / SDF's signed history) for each, which region(s) observed it,
and whether any served pricing/trade data derived from the affected
ledger(s) before detection.

## Known false-positive patterns

- **hashdb file corruption is indistinguishable from real drift from
  this alert alone** — a torn write to the hashdb file itself (disk
  full mid-Append, power loss) would also show up as a "mismatch" on
  next Verify, but it's OUR record that's wrong, not the source data.
  The three-way SDF comparison in the decision tree disambiguates
  this: if the bucket's current bytes match SDF, hashdb's own record
  is the thing that's wrong (recreate the hashdb file from a fresh
  `Create` + let it re-populate; historical entries before that point
  are simply un-verifiable, not proof of anything).
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

- 2026-07-09 — initial draft alongside wiring hashdb into production
  (ADR-0016, ROADMAP #46). Founding case: ledger 63332650.
