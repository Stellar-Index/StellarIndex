---
title: Runbook — ledgerstream-tier-both-missing
last_verified: 2026-08-29
status: draft
severity: P1
---

# Runbook — `stellarindex_ledgerstream_tier_both_missing`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ledgerstream_tier_both_missing` |
| Severity | P1 (page) |
| Detected by | `configs/prometheus/rules.r1/ledgerstream-tier.yml` (the overlay r1 actually loads); multi-host template: `deploy/monitoring/rules/ledgerstream-tier.yml`. Both trees carry the same expr. |
| Typical MTTR | 10–30 min (rehydrate from AWS) up to a few hours (cross-region rebuild) |
| Impact | The indexer (or a backfill job) cannot read an LCM from EITHER tier — the affected cursor is stalled until the gap is filled. Customer-facing impact depends on which cursor: live-tip stall → API freshness lag growing; backfill stall → completing a historical range is blocked but live serving is unaffected. |

## Background — ADR-0027 in one paragraph

`internal/ledgerstream/tiered.go` reads each LCM from the local
`galexie-archive` MinIO bucket (hot) and, on a `NoSuchKey`, falls
back to the AWS public bucket
`s3://aws-public-blockchain/v1.1/stellar/ledgers/pubnet/` (cold).
`both_missing` is when neither tier has the object.

**Status on r1 (as of 2026-08-29): the cold tier is ENABLED and this
alert CAN fire.** Two dates matter when reading history:

- The tier was enabled in the r1 inventory on **2026-07-25**
  (`3fe41cc0`) — the same change that fixed the credential bug which
  had made every cold read fail with `InvalidAccessKeyId` since the
  feature landed. Before that, cold-tier init failed and ledgerstream
  degraded silently to hot-only by design.
- The `both_missing` counter was **nil in production** until
  **2026-08-01** (`f9f36e17`, W5-mon-3, "register the metric
  unconditionally"). Any incident review that predates 2026-08-01 must
  not read the absence of this page as evidence the condition never
  occurred — it could not have fired.

## Symptoms

- Prometheus counter
  `stellarindex_ledgerstream_tier_read_total{outcome="both_missing"}`
  has increased in the last 5 min.
- The affected binary's logs show `cold read: ... not found` (or
  the AWS-SDK equivalent) for one or more ledger sequences.
- If the affected cursor is live ingest,
  `stellarindex_cursor_last_ledger{source="ledgerstream"}` has stopped
  advancing (`"ledgerstream"` is the only `source` label this gauge
  ever carries — `cmd/stellarindex-indexer/main.go`), and the
  per-source `stellarindex_source_last_event_unix` gauges are ageing.
- If it's a backfill, the backfill range's progress has frozen —
  read it from `stellarindex-ops list-cursors -config /etc/stellarindex.toml`.

> Earlier revisions of this runbook cited an indexer ledger-lag gauge
> and a backfill-cursor gauge. **Neither has ever been registered** —
> don't go looking for them. The three signals above are the real
> ones.

## Quick diagnosis (≤ 5 min)

Identify the failing ledger(s), which tier is at fault, and whether
AWS is reachable at all.

```sh
# 1. Which ledgers are missing — the indexer log line names them.
ssh root@136.243.90.96 'journalctl -u stellarindex-indexer --since="10 min ago" | grep -iE "both.missing|tiered.*not.found" | tail -5'

# 2. Is the local hot tier intact for the affected range?
#    Replace SEQ with the ledger from step 1. Don't hand-compute the
#    partition prefix: partitions are named "%08X--<start>-<end>/"
#    where the hex is MaxUint32-start (so it DESCENDS as the ledger
#    ascends) and each partition spans files_per_partition = 64000
#    ledgers, one object per ledger. Let mc find the object instead.
SEQ=...   # e.g. 23000000
ssh root@136.243.90.96 "mc find local/galexie-archive --name '*--${SEQ}.xdr.*'"

# 3. Is the AWS public bucket reachable from r1?
#    (aws-public-blockchain is in us-east-2, NOT us-east-1.)
ssh root@136.243.90.96 'curl -sf -m 10 https://aws-public-blockchain.s3.us-east-2.amazonaws.com/v1.1/stellar/ledgers/pubnet/ -I | head -3'

# 4. Cold-read latency in the last 30 min (background context for
#    whether AWS is generally slow vs flat-out unavailable). :9464 is
#    the indexer's metrics port; :9100 is node_exporter.
ssh root@136.243.90.96 'curl -s localhost:9464/metrics | grep stellarindex_ledgerstream_cold_read_duration_seconds_count'
```

## Decision tree

### A. AWS unreachable

Step 3 returns a non-2xx or times out. The AWS Open Data bucket is
either down or r1 has lost outbound HTTPS.

- **If r1 has lost outbound HTTPS**, page the on-call sysadmin to
  restore networking. Once restored, the indexer/backfill should
  retry automatically — the cursor isn't advanced past the failure.
- **If AWS Open Data is down**, this is rare but the ADR-0027
  "external dependency on AWS sponsorship" risk. Note that
  rehydration reads the SAME configured cold tier, so it cannot route
  around an AWS outage — wait it out (the cursor isn't advanced past
  the failure) and document the window in
  `docs/operations/incidents/`. Rehydration is the tool for the
  *other* two shapes: a hot range that was trimmed while cold was
  fine, and pre-warming hot before a planned backfill.
  ```sh
  # ⚠ READ THE FLAGS. The real signature is:
  #     rehydrate-galexie-archive -config PATH -from N -to N [-dry-run]
  # There is no -write, no --source and no peer selector: the tool
  # always reads the CONFIGURED cold tier (storage.s3_cold_*, which on
  # r1 is aws-public-blockchain) and writes into the local hot tier.
  # It COMMITS by default — `-dry-run` is the opt-in preview, so the
  # posture is fail-OPEN, the inverse of what this runbook used to say.
  # It refuses outright if the cold tier isn't configured, and it is
  # idempotent (PutFileIfNotExists skips files already hot).
  #
  # Preview first:
  stellarindex-ops rehydrate-galexie-archive \
    -config /etc/stellarindex.toml -from <SEQ> -to <SEQ_END> -dry-run
  # Then commit, under the heavy-job wrapper (CLAUDE.md r1 rule):
  sudo /usr/local/sbin/run-heavy-job.sh rehydrate \
    /usr/local/bin/stellarindex-ops rehydrate-galexie-archive \
      -config /etc/stellarindex.toml -from <SEQ> -to <SEQ_END>
  ```

  If the configured cold tier itself must change (a different
  provider, a peer-region mirror per ADR-0016), that is an ansible
  change to `storage.s3_cold_*` — there is no in-tool peer selector.

### B. AWS reachable but the specific partition is missing

Step 3 succeeds; step 1 lists ledger sequences; AWS GET on those
partitions returns NoSuchKey. Most likely a recent partition that
hasn't propagated to the public bucket yet (the bucket's freshness
SLA is "~30 min behind tip").

- Wait 30 min and check again — most cases self-resolve.
- If sustained beyond 1 h, escalate to AWS Open Data via the
  contact in the bucket's `README.md`.

### C. The local hot tier was trimmed too aggressively

Step 2 confirms the local partition is gone; step 3 also fails.

- A trim job (`galexie-archive-trim`) deleted a range that wasn't
  yet safely in cold storage. This is the failure mode
  `--verify-upstream` is meant to prevent — check the trim log:
  ```sh
  ssh root@136.243.90.96 'journalctl -u galexie-archive-trim.service --since="24 h ago" | tail -50'
  ```
- Rehydrate the trimmed range from cold (command in option A above),
  then **disable the trim timer** until the trim operator is fixed:
  ```sh
  ssh root@136.243.90.96 'systemctl disable --now galexie-archive-trim.timer'
  ```
  File a ticket against stellarindex-ops to add the missing safety.

### D. The indexer/backfill is mis-configured

Step 2 shows the partition is present locally and step 3 reaches
AWS, yet the metric increments. The binary is reading from the
wrong endpoint — its TOML points at a stale bucket or the AWS
config is missing.

- Check the binary's env / config for the right endpoints:
  ```sh
  ssh root@136.243.90.96 'grep -E "s3_bucket|s3_endpoint|s3_cold" /etc/stellarindex.toml'
  ssh root@136.243.90.96 'grep -E "AWS_|S3_" /etc/default/stellarindex'
  ```
  Note the 2026-07-25 lesson (`3fe41cc0`): r1's `AWS_ACCESS_KEY_ID` /
  `AWS_SECRET_ACCESS_KEY` are **MinIO's** credentials, for the HOT
  datastore. The cold client is built separately from
  `s3_cold_access_key_env` / `s3_cold_secret_key_env` — both empty
  means anonymous reads (correct for `aws-public-blockchain`); exactly
  one set is a config error.
- Restart the binary after fixing the config.

## Aftermath

Once the gap is filled, the counter stops incrementing and the
alert resolves on its own (no manual reset). Confirm the affected
cursor has resumed advancing:

```sh
ssh root@136.243.90.96 'curl -s localhost:9464/metrics | \
  grep -E "stellarindex_cursor_last_ledger|stellarindex_source_last_event_unix"'
stellarindex-ops list-cursors -config /etc/stellarindex.toml
```

## Related

- ADR-0027 — LCM cache tiering, the rollout sequence (§Sequencing).
- ADR-0016 — per-region storage strategy (R2 reads AWS direct; R3
  has its own Vultr mirror). Either can become THE cold tier for a
  region, but only by repointing `storage.s3_cold_*` — the rehydrate
  tool has no peer selector.
- `feedback_cold_tier_premature_enable` — bare §3 (enable tiering)
  without §4 (bulk trim) introduces this failure mode without
  benefit; the rollout always lands them together.
- `docs/operations/lcm-cache-tiering.md` — the operator-facing
  tiering guide (bucket, region, credential shape).

## Changelog

- 2026-05-22 — initial draft alongside the page-grade
  `stellarindex_ledgerstream_tier_both_missing` alert.
- 2026-08-29 — re-verified against HEAD (runbook re-verification
  wave K). The rehydrate command named four flags that have never
  existed (`-write`, `--from`, `--to`, `--source`) and inverted the
  tool's posture (it commits by default; `-dry-run` is the opt-in
  preview) — corrected, with the heavy-job wrapper. Two cited gauges
  (an indexer ledger-lag and a backfill cursor) have no producer and
  never had one; replaced with `stellarindex_cursor_last_ledger` /
  `stellarindex_source_last_event_unix` / `list-cursors`, and
  lint-docs §11 widened so a future phantom in these namespaces fails
  CI. The hot-tier `mc ls` prefix arithmetic was wrong
  (partition span is 64000, and the hex prefix is MaxUint32-start) —
  replaced with `mc find`. Metrics port `:9100` → `:9464`; host shapes
  → r1's IP. The "cold tier disabled, alert cannot fire" background was
  stale: enabled 2026-07-25, and the counter was nil in production
  until 2026-08-01.
