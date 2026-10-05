---
title: LCM cache tiering — operator runbook
last_verified: 2026-08-29
status: current
---

# LCM cache tiering — operator runbook

Executes [ADR-0027](../adr/0027-lcm-cache-tiering.md) §Steps 3-5 on r1.

## Concept refresher

- **Hot tier** = r1's local galexie-archive MinIO bucket, ~4.5 TB of the
  ~18.3 TB usable raidz1 pool (`docs/architecture/storage-considerations.md`).
- **Cold tier** = `aws-public-blockchain` S3, `v1.1/stellar/ledgers/pubnet/`.
  Read-only, authoritative, ~80 ms per GET amortised over 64-ledger partitions.
- **TieredDataStore** (`internal/ledgerstream`) reads hot first, falls
  through to cold on `IsNotFound` only, never on transient errors. Writes
  go to hot only.
- **Trim** = `stellarindex-ops trim-galexie-archive` (deletes cold-eligible
  hot files; 5-layer safety stack).
- **Rehydrate** = `stellarindex-ops rehydrate-galexie-archive` (cold → hot
  for a range; the rollback).

## Before you start

- [ ] SSH root on r1 (`ssh root@136.243.90.96`) and read access to
  /var/log/stellarindex + journals.
- [ ] 30 minutes of attention; no step is abandonable mid-stream.
- [ ] `mc` has `local` + `aws-public` aliases (`docs/operations/galexie-backfill.md`).
- [ ] Read ADR-0027 §Decision.

## Step 3 — enable the dual-source flag in r1's TOML

The flag is the presence of `storage.s3_cold_*` in `/etc/stellarindex.toml`.

1. Under `[storage]`:

   ```toml
   s3_cold_endpoint        = "https://s3.us-east-2.amazonaws.com"
   s3_cold_region          = "us-east-2"
   s3_cold_bucket_archive  = "aws-public-blockchain/v1.1/stellar/ledgers/pubnet"
   # Empty = anonymous (the bucket is public-read). Both empty or both
   # naming an env var; half a pair fails config.StorageConfig.validate.
   s3_cold_access_key_env  = ""
   s3_cold_secret_key_env  = ""
   ```

   - **Region is `us-east-2`.** The cold client is path-style and needs
     the regional endpoint; `s3.us-east-1.amazonaws.com` and the global
     endpoint answer `301 PermanentRedirect`. Check:
     `curl -sI https://aws-public-blockchain.s3.amazonaws.com/ | grep bucket-region`
     → `x-amz-bucket-region: us-east-2`.
   - The `*_env` fields hold the **name** of an env var, never the
     credential. A named-but-unset var is a startup error, not a fallback
     to anonymous. `pipeline.NewColdDataStore` inherits nothing from the
     ambient environment, so MinIO's `AWS_ACCESS_KEY_ID` in
     `/etc/default/stellarindex` cannot leak into AWS requests (symptom if
     it did: `InvalidAccessKeyId`).

2. Restart consumers:

   ```sh
   systemctl restart stellarindex-indexer stellarindex-aggregator stellarindex-api
   ```

3. `stellarindex_ledgerstream_tier_read_total{outcome="hot"}` should
   increment on every ledger read; `outcome="cold"` ≈ 0 before any trim.

4. Smoke-test a backfill over a hot-only range: no slower than before, or
   revert the TOML block and capture diagnostics.

**Rollback**: remove the cold fields and restart; behaviour is restored
byte-for-byte.

## Step 4 — first bulk trim (operator-triggered)

One-shot, not on a timer. Reclaims ~3-4 TB.

1. **Cutoff** (90 d hot window at 17280 ledgers/day):

   ```sh
   TIP=$(sudo -u postgres psql stellarindex -tAc \
     "SELECT MAX(last_ledger) FROM ingestion_cursors WHERE source='ledgerstream';")
   CUTOFF=$(( TIP - 90 * 17280 ))
   echo "tip=$TIP cutoff=$CUTOFF"
   ```

2. **Dry-run first. Always.**

   ```sh
   /usr/local/bin/stellarindex-ops trim-galexie-archive \
     -config /etc/stellarindex.toml \
     -older-than-ledger "$CUTOFF" \
     -dry-run
   ```

   Expect JSON on stderr: `{"msg":"trim plan ready","candidates":<N>,
   "skipped_too_fresh":<M>,"skipped_not_in_cold":0,"verify_errors":0,
   "dry_run":true,...}`. Non-zero `skipped_not_in_cold` means local files
   absent from aws-public-blockchain: investigate before committing.

3. **Trim in 1M-ledger chunks** so a partial failure leaves a clear position.

   - A chunk is done only at `enumeration_complete=true`: `-max-files
     100000` caps each pass at ~10% of a 1M-object chunk, so the inner
     loop is load-bearing.
   - `--verify-upstream` cold HEADs run ~7/s serial (2,000 candidates =
     4m47s on r1): ~40 h per 1M chunk, ~80 days for ~50M objects. Use this
     loop for surgical trims and the partition straddling the cutoff. For
     bulk reclaim, verify per partition (local vs cold object-count parity
     plus sampled md5) and delete verified partitions wholesale.

   ```sh
   for CHUNK in $(seq 2 1000000 $CUTOFF); do
     CHUNK_END=$(( CHUNK + 999999 ))
     if (( CHUNK_END > CUTOFF )); then CHUNK_END=$CUTOFF; fi
     echo "=== chunk: $CHUNK → $CHUNK_END ==="
     while : ; do
       OUT=$(/usr/local/bin/stellarindex-ops trim-galexie-archive \
         -config /etc/stellarindex.toml \
         -older-than-ledger "$CHUNK_END" \
         -max-files 100000 \
         -commit 2>&1 | tee /dev/stderr)
       echo "$OUT" | grep -q '"enumeration_complete":true' && break
     done
     zpool list -H data   # stop if %CAP climbs unexpectedly
   done
   ```

> **Set the hot floor in the SAME change that trims.**
> `stellarindex_archive_hot_floor` (inventory var; role default 2 = no
> floor) renders to `/etc/default/galexie-archive-fill`, `ARCHIVE_FROM` on
> archive-completeness, and the verify-archive tiers' cold-start `-from`.
> Trim without raising it and the next hourly fill re-downloads everything
> (~3.7 TB at the 49,984,000 boundary); raising it without trimming only
> skips work.

4. **Verify**: `zpool list` shows ~3-4 TB recovered; `mc du
   local/galexie-archive` dropped by the same.

5. **Cold-read sanity test**: run `stellarindex-ops backfill` (`-dry-run`
   or a small range) over a trimmed range, then
   `curl -s localhost:9100/metrics | grep ledgerstream_tier_read_total`:
   `outcome="cold"` must be > 0 and growing.

**Rollback**: `stellarindex-ops rehydrate-galexie-archive -from <start>
-to <end> -write` (see [credentials](#rehydrate-the-undo-button--credentials)).
Idempotent (`PutFileIfNotExists`); without `-write` it only lists files.

## Step 5 — monthly trim cadence (installed, not enabled)

`galexie-archive-trim.timer` (`*-*-01 03:17:00 UTC`) starts
`galexie-archive-trim.service`; its `ExecStartPre` `compute-trim-cutoff.sh`
reads the tip from `ingestion_cursors`, subtracts 1,555,200 ledgers,
persists that as the fill's hot floor and writes `TRIM_CUTOFF` for
`trim-galexie-archive -older-than-ledger ${TRIM_CUTOFF} -commit`.

The archival-node role installs but does not enable the timer (ADR-0027
§Decision: cold tier and bulk trim ship as one step). Enable only once
`s3_cold_bucket_archive` is set in the indexer/API TOML **and**
`stellarindex_ledgerstream_tier_read_total{outcome="cold"}` > 0 after
Step 4's sanity test (a set bucket key alone proves nothing):

```sh
systemctl enable --now galexie-archive-trim.timer
systemctl list-timers galexie-archive-trim.timer
```

Until then, re-run Step 4's chunked loop monthly.

## Common failure modes

- **`s3_cold_bucket_archive` missing**: trim refuses to run. Fix the TOML.
- **`cold.Exists failed` warnings**: AWS blip; trim skips those files
  (safety posture). Re-run.
- **Pool capacity rises during trim**: ZFS writes metadata before
  reclaiming. If it persists past the chunk, pause and `zpool scrub data`.
- **Indexer `cold.GetFile` errors** (not `NoSuchKey`): check AWS status.
  Transient errors propagate by design; if extended, rehydrate the range.

## Metrics

- `stellarindex_ledgerstream_tier_read_total{outcome=hot|cold|both_missing}`:
  ~100% hot for live ingest; cold only while backfilling trimmed ranges.
- `stellarindex_ledgerstream_cold_read_duration_seconds`: p50 < 200 ms is
  healthy; sustained multi-second suggests a cross-Atlantic network issue.

## Rehydrate (the undo button) — credentials

- The service identity in `/etc/default/stellarindex` can READ and DELETE
  on `galexie-archive` but **not PUT** (deliberate); under it rehydrate
  fails `hot.PutFileIfNotExists ... 403 AccessDenied`.
- **Use `galexie-archive-writer`** (mc alias `archivewriter`; Put/Get/List
  on `galexie-archive`). `09-minio.yml` creates it from vault; if
  `--tags minio` has not been applied on the host its secret fails
  `SignatureDoesNotMatch`: check first.

```sh
set -a; . /etc/default/stellarindex; set +a
# Same pair /etc/default/galexie-backfill carries; no secret retyped.
export AWS_ACCESS_KEY_ID=$(sed -n 's/^AWS_ACCESS_KEY_ID=//p'     /etc/default/galexie-backfill)
export AWS_SECRET_ACCESS_KEY=$(sed -n 's/^AWS_SECRET_ACCESS_KEY=//p' /etc/default/galexie-backfill)
stellarindex-ops rehydrate-galexie-archive -config /etc/stellarindex.toml -from <ledger> -to <ledger> -write
```

- **Break-glass only:** the `local` mc alias is MinIO **root** (the hourly
  fill still writes as it; see the credential-hygiene section of
  [credential-rotation.md](credential-rotation.md)). Never bake it into a
  procedure.
- The cold read stays anonymous regardless of these exports.

## References

- [ADR-0027 — LCM cache tiering](../adr/0027-lcm-cache-tiering.md)
- [ADR-0016 — Per-region storage strategy](../adr/0016-per-region-storage-strategy.md)
- [internal/ledgerstream/tiered.go](../../internal/ledgerstream/tiered.go)
- [internal/ops/archive/trim_galexie_archive.go](../../internal/ops/archive/trim_galexie_archive.go)
- [internal/ops/archive/rehydrate_galexie_archive.go](../../internal/ops/archive/rehydrate_galexie_archive.go)
