---
title: LCM cache tiering — operator runbook
last_verified: 2026-08-29
status: current
---

# LCM cache tiering — operator runbook

Per [ADR-0027](../adr/0027-lcm-cache-tiering.md). This runbook is
the step-by-step for operators executing the §Steps 3-5 transition
on r1 (the design + code primitives are already live in `main`).

## Concept refresher

- **Hot tier** = local galexie-archive MinIO bucket on r1. Fast
  reads, but consumes ~4.5 TB of the ~18.3 TB usable pool (r1 is
  raidz1; the 13.85 TB this line used to quote was two-parity math —
  see `docs/architecture/storage-considerations.md`).
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
   s3_cold_endpoint        = "https://s3.us-east-2.amazonaws.com"
   s3_cold_region          = "us-east-2"
   s3_cold_bucket_archive  = "aws-public-blockchain/v1.1/stellar/ledgers/pubnet"
   # Leave the *_env fields empty — the AWS public bucket is
   # public-read, so the cold client signs nothing. Both must be
   # empty or both must name an env var; half a pair is a config
   # error (config.StorageConfig.validate).
   s3_cold_access_key_env  = ""
   s3_cold_secret_key_env  = ""
   ```

   **The region is `us-east-2`, not `us-east-1`** — this runbook
   said `us-east-1` with the global `https://s3.amazonaws.com`
   endpoint until 2026-07-25, and neither works. The cold client is
   built path-style, so it needs the *regional* endpoint, and
   `s3.us-east-1.amazonaws.com` answers `301 PermanentRedirect` for
   this bucket. Verified live 2026-07-25:

   ```sh
   curl -sI https://aws-public-blockchain.s3.amazonaws.com/ | grep bucket-region
   # x-amz-bucket-region: us-east-2
   ```

   Note also that the *_env fields hold the **NAME** of an env var
   (same convention as `s3_access_key_env`), never the credential
   itself. Empty means anonymous; a named-but-unset env var is a
   hard startup error rather than a silent downgrade to anonymous
   — the earlier text here claimed "the SDK falls back to anonymous
   creds when no env vars are set", which was never true for this
   deployment: r1's `/etc/default/stellarindex` exports MinIO's
   credentials as `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` for
   the HOT tier, so the SDK's chain was never empty and the cold
   client signed every AWS request with MinIO's key
   (`InvalidAccessKeyId: The AWS Access Key Id you provided does
   not exist in our records`). The cold client is now built by
   `pipeline.NewColdDataStore`, which inherits nothing from the
   ambient environment.

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
   a clear position cursor.

   > ⚠ **Two corrections (2026-07-25), both from actually running this:**
   >
   > **(a) A chunk is done when `enumeration_complete=true`, not after
   > one invocation.** A 1M-ledger chunk holds ~1M objects but
   > `-max-files 100000` caps each run at 100k, so a single pass trims
   > at most 10% of the chunk and the old loop advanced anyway,
   > silently leaving ~90% behind. Pre-pagination this was invisible
   > because the tool couldn't see past the SDK's 1000-key listing cap
   > and deleted nothing at all; now that it works, the inner
   > repeat-until-complete loop below is load-bearing.
   >
   > **(b) Budget for `--verify-upstream` throughput before choosing
   > this path at bulk scale.** Measured on r1 (2026-07-25): cold HEADs
   > run ~7/s serial — 2,000 candidates took 4m47s, so 1M-object chunks
   > cost ~40 h each and a ~50M-object bulk trim would take ~80 days.
   > This per-file loop is the right tool for SURGICAL trims and for
   > the one partition that straddles the cutoff. For bulk reclaim,
   > verify per-PARTITION instead (local vs cold object-count parity
   > plus a sampled md5 comparison) and delete verified partitions
   > wholesale; keep this loop for the straddle.

   ```sh
   for CHUNK in $(seq 2 1000000 $CUTOFF); do
     CHUNK_END=$(( CHUNK + 999999 ))
     if (( CHUNK_END > CUTOFF )); then CHUNK_END=$CUTOFF; fi
     echo "=== chunk: $CHUNK → $CHUNK_END ==="
     # Repeat until the tool reports enumeration_complete=true for
     # this cutoff — each pass deletes at most -max-files objects.
     while : ; do
       OUT=$(/usr/local/bin/stellarindex-ops trim-galexie-archive \
         -config /etc/stellarindex.toml \
         -older-than-ledger "$CHUNK_END" \
         -max-files 100000 \
         -commit 2>&1 | tee /dev/stderr)
       echo "$OUT" | grep -q '"enumeration_complete":true' && break
     done
     # Watch pool capacity between chunks.
     zpool list -H data
     # Stop if capacity climbs unexpectedly (defrag pressure can
     # temporarily raise %CAP during heavy delete loads).
   done
   ```

> **Set the hot floor in the SAME change that trims.** The
> `stellarindex_archive_hot_floor` inventory var (rendered to
> `/etc/default/galexie-archive-fill`, `ARCHIVE_FROM` on
> archive-completeness, and verify-archive-tier-a's cold-start default)
> tells the hourly mirror and the daily completeness check which
> partitions are trimmed ON PURPOSE. Trim without raising it and the
> next fill run re-downloads everything you deleted (~3.7 TB at the
> 49,984,000 boundary); raise it without trimming and nothing breaks
> (it only skips work).

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
<start> -to <end> -write` re-fetches the trimmed range from cold back
into hot. Idempotent (`PutFileIfNotExists`). Fail-closed: without
`-write` it only lists the would-copy files.

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
stellarindex-ops rehydrate-galexie-archive -from <N> -to <M> -write
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

## Rehydrate (the undo button) — credentials

`rehydrate-galexie-archive` copies cold → hot. Proven end-to-end
2026-07-25 (delete a ledger from hot → rehydrate → byte-identical md5,
288ms), but ONLY under the right identity:

- The service identity in `/etc/default/stellarindex` can READ and
  DELETE on `galexie-archive` but **not PUT** (deliberate: the indexer
  should never write the archive). Under it, rehydrate fails
  `hot.PutFileIfNotExists ... 403 AccessDenied`.
- The `archivewriter` mc alias (`galexie-archive-writer`) was **stale**
  when this was written — its stored secret failed
  `SignatureDoesNotMatch` on every call, because the identity had a
  vault var and an env file but was never created in MinIO by ansible.
  **Fixed 2026-07-25** (`09-minio.yml` now renders its policy, creates
  the user from vault, and attaches): applying `--tags minio` re-syncs
  the secret and the alias works again. Until that apply has run on the
  host, the identity is still broken — check before relying on it.
- **Use `galexie-archive-writer` for rehydrate**, not root. Its policy
  grants Put/Get/List on `galexie-archive`, which is exactly what
  rehydrate needs:

```sh
set -a; . /etc/default/stellarindex; set +a
# The archive-writer identity — same pair /etc/default/galexie-backfill
# carries, so no secret is retyped here.
export AWS_ACCESS_KEY_ID=$(sed -n 's/^AWS_ACCESS_KEY_ID=//p'     /etc/default/galexie-backfill)
export AWS_SECRET_ACCESS_KEY=$(sed -n 's/^AWS_SECRET_ACCESS_KEY=//p' /etc/default/galexie-backfill)
stellarindex-ops rehydrate-galexie-archive -config /etc/stellarindex.toml -from <ledger> -to <ledger> -write
```

- **Break-glass only:** the `local` mc alias is MinIO **root**, and the
  hourly fill job still writes as it (a known least-privilege gap — see
  the credential-hygiene section of
  [credential-rotation.md](credential-rotation.md)). It will make
  rehydrate work if the archive-writer apply has not run yet, but do not
  bake it into a procedure — root credentials in a copy-pasteable
  runbook is how they end up in transcripts.

The cold read stays anonymous regardless of these exports — the cold
client is built with explicit anonymous credentials (2026-07-25 fix)
precisely so ambient writer creds cannot leak into requests to AWS.
