# LCM cache tiering — operator runbook

Per [ADR-0027](../adr/0027-lcm-cache-tiering.md). This runbook is
the step-by-step for operators executing the §Steps 3-5 transition
on r1 (the design + code primitives are already live in `main`).

## Concept refresher

- **Hot tier** = local galexie-archive MinIO bucket on r1. Fast
  reads, but consumes ~4.5 TB of the 13.85 TB usable pool.
- **Cold tier** = `aws-public-blockchain` S3 (the AWS Open Data
  Sponsorship bucket — `v1.1/stellar/ledgers/pubnet/`). Read-only,
  authoritative, ~80 ms per-GET amortised over 64-ledger
  partitions.
- **TieredDataStore** = the `internal/ledgerstream` primitive that
  reads hot first, falls through to cold on `IsNotFound`, never
  on transient errors. Writes target hot exclusively.
- **Trim operator** = `stellarindex-ops trim-galexie-archive` —
  deletes cold-eligible hot files (5-layer safety stack).
- **Rehydrate operator** = `stellarindex-ops
  rehydrate-galexie-archive` — copies cold → hot for a range. The
  rollback primitive.

## Before you start

- [ ] You have SSH root access to r1 (`ssh root@136.243.90.96`).
- [ ] You can read /var/log/stellarindex + the systemd journals.
- [ ] You have at least 30 minutes of attention available — none
  of the steps are abandonable mid-stream.
- [ ] `mc` is configured with `local` + `aws-public` aliases (see
  `docs/operations/galexie-backfill.md`).
- [ ] You've read ADR-0027 §Sequencing.

## Step 3 — enable the dual-source flag in r1's TOML

The feature flag is the presence of `storage.s3_cold_*` fields in
`/etc/stellarindex.toml`. Until populated, every read goes
through the legacy single-source path.

1. Edit `/etc/stellarindex.toml` and add (or uncomment) under
   `[storage]`:

   ```toml
   s3_cold_endpoint        = "https://s3.amazonaws.com"
   s3_cold_region          = "us-east-1"
   s3_cold_bucket_archive  = "aws-public-blockchain/v1.1/stellar/ledgers/pubnet"
   # Leave the *_env fields empty — the AWS public bucket needs
   # anonymous access. The SDK falls back to anonymous creds when
   # no env vars are set.
   s3_cold_access_key_env  = ""
   s3_cold_secret_key_env  = ""
   ```

2. Restart the consumer services:

   ```sh
   systemctl restart stellarindex-indexer stellarindex-aggregator stellarindex-api
   ```

3. Verify the tiered read path is active by checking the new
   metrics in Prometheus:

   ```
   stellarindex_ledgerstream_tier_read_total{outcome="hot"}
   ```

   Should be incrementing on every ledger read. At this stage
   no files have been trimmed, so `outcome="cold"` should be 0 or
   nearly 0 (only edge cases like manifest reads might hit cold).

4. Smoke-test a backfill against a hot-only range — should be
   no slower than pre-change. If it's slower, revert the TOML
   block and capture diagnostics.

**Rollback at this stage**: remove the cold-tier fields from
TOML and restart. Pre-change behaviour is restored byte-for-byte.

## Step 4 — first bulk trim (operator-triggered)

This step is intentionally one-shot operator-driven, not on a
timer. The first trim reclaims ~3-4 TB; you watch the pool drop
in real-time.

1. **Compute the cutoff ledger.** ADR-0027 specifies a 90 d hot
   window. At ~17280 ledgers per day (5 s ledger close):

   ```sh
   TIP=$(sudo -u postgres psql stellarindex -tAc \
     "SELECT MAX(last_ledger) FROM ingestion_cursors WHERE source='ledgerstream';")
   CUTOFF=$(( TIP - 90 * 17280 ))
   echo "tip=$TIP cutoff=$CUTOFF"
   ```

2. **Dry-run first.** Always.

   ```sh
   /usr/local/bin/stellarindex-ops trim-galexie-archive \
     -config /etc/stellarindex.toml \
     -older-than-ledger "$CUTOFF" \
     -dry-run
   ```

   Expect: `trim plan ready candidates=<N> skipped_too_fresh=<M>
   skipped_not_in_cold=0 verify_errors=0 dry_run=true`. Sanity-
   check: `skipped_not_in_cold` should be 0 or very small —
   non-zero means files exist locally that aren't in
   aws-public-blockchain, which is unusual and worth investigating
   before committing.

3. **Run trim in 1M-ledger chunks** so a partial failure leaves
   a clear position cursor. For each chunk:

   ```sh
   for CHUNK in $(seq 2 1000000 $CUTOFF); do
     CHUNK_END=$(( CHUNK + 999999 ))
     if (( CHUNK_END > CUTOFF )); then CHUNK_END=$CUTOFF; fi
     echo "=== chunk: $CHUNK → $CHUNK_END ==="
     /usr/local/bin/stellarindex-ops trim-galexie-archive \
       -config /etc/stellarindex.toml \
       -older-than-ledger "$CHUNK_END" \
       -max-files 100000 \
       -commit
     # Watch pool capacity between chunks.
     zpool list -H data
     # Stop if capacity climbs unexpectedly (defrag pressure can
     # temporarily raise %CAP during heavy delete loads).
   done
   ```

4. **Verify the post-trim pool size.** `zpool list` should
   show ~3-4 TB recovered. `mc du local/galexie-archive` should
   show the bucket dropped by the same amount.

5. **Sanity test a cold-tier read.** Pick a trimmed ledger range
   and confirm `stellarindex-ops backfill` (with `-dry-run` or a
   small range) successfully reads from cold:

   ```sh
   # Check the cold-read metric is incrementing.
   curl -s localhost:9100/metrics | grep ledgerstream_tier_read_total
   ```

   `outcome="cold"` should now be > 0 and growing as backfill
   pulls trimmed-range LCMs from AWS.

**Rollback**: `stellarindex-ops rehydrate-galexie-archive -from
<start> -to <end>` re-fetches the trimmed range from cold back
into hot. Idempotent (`PutFileIfNotExists`).

## Step 5 — monthly trim cadence (deferred)

A `trim-galexie-archive.timer` that fires monthly is documented
in ADR-0027 but **not yet shipped**. It requires the operator
to add a `--older-than-duration 90d` mode to the trim subcommand
(currently only `--older-than-ledger` is supported); that needs
a way to resolve the current tip at execution time (read from
`ingestion_cursors`) plus the time-to-ledger conversion. Until
that lands, operators re-run Step 4's chunked invocation manually
once per month.

## Common failure modes

### Cold tier check fails (`s3_cold_bucket_archive` missing)

Trim refuses to run. Fix the TOML and retry.

### `cold.Exists failed` warnings during trim

Network blip to AWS. Trim treats these as "not present" and
skips (safety posture). Re-run trim; the same files will be
verified again next pass.

### Pool capacity rises during trim

ZFS deletes write metadata before reclaiming space. Brief
capacity bump is normal; if it persists past the chunk, pause
trims and run `zpool scrub data` to force reclaim.

### Indexer reports `cold.GetFile` errors

The cold tier returned an unexpected error (not `NoSuchKey`).
Check AWS status. The TieredDataStore propagates transient
errors rather than masking them — this is intentional. If
extended, rehydrate the affected range to restore hot service:

```sh
stellarindex-ops rehydrate-galexie-archive -from <N> -to <M>
```

## Metrics

- `stellarindex_ledgerstream_tier_read_total{outcome=hot|cold|both_missing}`
  — read tier breakdown. Production steady-state should be
  ~100% hot for live ingest; cold non-zero during backfill of
  trimmed ranges.
- `stellarindex_ledgerstream_cold_read_duration_seconds`
  — p50/p95/p99 of cold tier reads. Sub-200 ms p50 is healthy;
  multi-second sustained suggests cross-Atlantic network issue.

## References

- [ADR-0027 — LCM cache tiering](../adr/0027-lcm-cache-tiering.md)
- [ADR-0016 — Per-region storage strategy](../adr/0016-per-region-storage-strategy.md)
- [internal/ledgerstream/tiered.go](../../internal/ledgerstream/tiered.go)
- [cmd/stellarindex-ops/trim_galexie_archive.go](../../cmd/stellarindex-ops/trim_galexie_archive.go)
- [cmd/stellarindex-ops/rehydrate_galexie_archive.go](../../cmd/stellarindex-ops/rehydrate_galexie_archive.go)
