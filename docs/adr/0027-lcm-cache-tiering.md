---
adr: 0027
title: LCM cache tiering — local galexie-archive as hot, aws-public-blockchain as cold
status: Accepted
date: 2026-05-20
supersedes: []
superseded_by: null
---

# ADR-0027: LCM cache tiering — local galexie-archive as hot, aws-public-blockchain as cold

## Context

r1 mirrored the whole pubnet LCM history locally and ran its ZFS pool nearly full; a full pool is a write-amplification outage. AWS publishes every pubnet LCM at `s3://aws-public-blockchain/v1.1/stellar/ledgers/pubnet/`, which is cheap to read for bulk backfills even over a long RTT.

## Decision

LCM reads are two-tier. Hot is the local `galexie-archive` bucket (the recent window, default 90 days); cold is the public AWS bucket, read-only and anonymous. `TieredDataStore` (`internal/ledgerstream/tiered.go`) falls back to cold only on a not-found from hot, never on a transient error. Live ingest stays on hot, and `galexie-live` and the history-archive mirror (`data/archive`, ADR-0017) are untouched.

Cold tiering is off unless `s3_cold_bucket_archive` is set (`StorageConfig.ColdTieringEnabled`). Enabling it and the first bulk trim are one operational step: enabling alone adds a cold-path dependency with no headroom gain. `stellarindex-ops trim-galexie-archive --older-than-ledger N` deletes hot files below N, dry-run unless `--commit`, only after verifying the upstream copy exists. The 90-day window comes from the timer's cutoff helper (`compute-trim-cutoff.sh`); the code enforces a 30-day floor. `rehydrate-galexie-archive` restores a range byte-for-byte from cold. A monthly systemd timer runs the trim once the bulk trim is done.

The cold client is built by `pipeline.NewColdDataStore` from a zero-valued `aws.Config`, so hot-tier credentials and `AWS_ENDPOINT_URL` in the environment never reach AWS. `s3_cold_access_key_env` and `s3_cold_secret_key_env` select anonymous (both empty) or static credentials; half a pair is a config error. The bucket is in us-east-2 and needs the regional endpoint:

```toml
s3_cold_endpoint       = "https://s3.us-east-2.amazonaws.com"
s3_cold_region         = "us-east-2"
s3_cold_bucket_archive = "aws-public-blockchain/v1.1/stellar/ledgers/pubnet"
```

## Invariant

Trim verifies the upstream copy exists before deleting a local LCM; skipping the check needs both `--no-verify-upstream` and `--i-have-verified-cold-out-of-band`. A cold-init failure degrades to hot-only with a warning, so enabling the tier is verified by `stellarindex_ledgerstream_tier_read_total{outcome="cold"}` increasing, never by the absence of errors. `stellarindex_ledgerstream_tier_both_missing` alerts on any ledger found in neither tier (`deploy/monitoring/rules/ledgerstream-tier.yml`).

## Consequences

- Frees several TB on the pool and makes hot-corruption recoverable from AWS rather than from an off-site restore.
- Cold backfills pay long-RTT latency (hours for a ~12M-ledger range) and depend on a sponsored, not contractually guaranteed, public bucket.
- The bulk trim is hours-long and IOPS-heavy; run it in a quiet window.
- Capacity figures: `docs/architecture/storage-considerations.md`.

## Evidence

- `internal/ledgerstream/tiered.go`, `internal/pipeline/coldstore.go`, `internal/config/config.go` (`s3_cold_*`).
- `internal/ops/archive/trim_galexie_archive.go`; timer `configs/ansible/roles/archival-node/templates/systemd/galexie-archive-trim.timer.j2`.
- Metrics in `internal/obs/metrics.go`; alerts in `deploy/monitoring/rules/ledgerstream-tier.yml`.
