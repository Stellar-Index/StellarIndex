---
title: Runbook — archive-divergence
last_verified: 2026-08-29
status: current
severity: P1
---

# Runbook — `stellarindex_stellar_archive_divergence`

> **LIVE (wired 2026-08-29, issue #282).** Two separate defects
> kept this page from ever firing; both are fixed:
>
> - **No producer.** The counter
>   `stellarindex_verify_archive_mismatches_total` only left the
>   verify-archive process through the opt-in `-metrics-listen`
>   HTTP endpoint, which the `verify-archive-tier-a` / `-tier-b`
>   units never passed and `configs/prometheus/prometheus.r1.yml`
>   has no scrape job for (and a one-shot job is gone between
>   scrapes anyway). Both units now pass `-textfile-output`, so the
>   counter reaches Prometheus through node_exporter's textfile
>   collector — cumulative across runs, zero-seeded on all three
>   reasons, labelled `tier` (`chain` for tier-a,
>   `checkpoint` for tier-b). Emitter:
>   `internal/ops/archive/verify_archive_textfile.go`.
> - **A 1h lookback on a nightly producer.** `increase(…[1h])`
>   made the step visible for one hour in every twenty-four, so the
>   page self-resolved before an operator saw it. The window is now
>   **26h** (24h timer cadence + jitter + cushion).
>
> Pinned by `deploy/monitoring/rule-tests/stellar_test.yml`,
> `internal/ops/archive/verify_archive_unit_wiring_test.go` (the units
> wire an export path, and the tier-b ansible block is reachable under
> the `ops-jobs` tag the apply below uses) and
> `internal/ops/archive/verify_archive_chunks_test.go` (a boundary
> divergence reaches the counter).
>
> **Takes effect on r1 only after the ansible role is applied** —
> the templates under `configs/ansible/…/templates/systemd/` are
> the authority for the running units, so a binary-only deploy
> ships this dead. Until then a mismatch still surfaces only as
> `stellarindex_verify_archive_unit_failed` (severity ticket).
> The unit render is behind the low-risk `ops-jobs` tag (it does
> not touch galexie):
>
> ```sh
> # 1. Render BOTH units. tier-b lives in its own
> #    `verify_archive_tier_b_enabled` block; it carries the
> #    ops-jobs tag too, pinned by
> #    TestVerifyArchiveUnits_ReachableUnderOpsJobsTag. Sanity-check
> #    the selection first if you want:
> #      ansible-playbook --list-tasks -i inventory/r1.yml \
> #        playbooks/archival-node.yml --tags ops-jobs
> ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
>   --tags ops-jobs --check --diff
> ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
>   --tags ops-jobs
>
> # 2. Prime BOTH producers once, one at a time — they share
> #    run-heavy-job.sh's "verify-archive" singleton lock, so a
> #    concurrent start just fails on the lock. This is also what
> #    puts the zero baseline on disk BEFORE the first nightly run
> #    (blind spot 2 below), so do not skip it. A non-zero exit here
> #    is itself a finding: read the journal.
> systemctl start verify-archive-tier-a.service
> systemctl start verify-archive-tier-b.service
>
> # 3. FAIL-CLOSED CONFIRM — run it, do not eyeball it. Both tiers
> #    must be exporting; a missing tier means the apply is
> #    HALF-DONE and this page is still dead for that tier.
> for t in chain checkpoint; do
>   curl -sf localhost:9100/metrics \
>     | grep -q "stellarindex_verify_archive_mismatches_total{tier=\"$t\"" \
>     || { echo "MISSING tier=$t — apply is HALF-DONE; archive-divergence cannot fire for that tier"; exit 1; }
> done; echo "both tiers exporting"
> ```
>
> On a healthy host all six samples (2 tiers × 3 reasons) read 0 —
> the zero-seed IS the proof the producer is alive. If step 3 prints
> `MISSING tier=checkpoint`, start the tier-b unit by hand
> (`systemctl start verify-archive-tier-b.service`) and re-run it;
> do not treat a green `tier="chain"` as an applied fix.
>
> Blind spots this page still has are listed under
> [Known blind spots](#known-blind-spots) — read them before you
> trust a silent dashboard.

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_stellar_archive_divergence` |
| Severity | P1 (`severity: page` — SEV-1) |
| Detected by | `configs/prometheus/rules.r1/stellar.yml` (group `stellarindex.stellar`, `severity: page`, `for: 0s`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/stellar.yml`. |
| Typical MTTR | hours |
| Impact | The verify-archive walk found a mismatch. Since the F-1329 repoint this fires equally on **lake-integrity failures** — a hash-chain break or ledger-sequence gap in our own archive — and on cross-archive checkpoint divergence vs a reference (SDF / LOBSTR / Satoshipay). Either way it is a **correctness incident**: our archive's bytes are not the network's bytes, by corruption, bug, or compromise. |

## Symptoms

- `increase(stellarindex_verify_archive_mismatches_total[26h]) > 0`.
  Fires immediately (`for: 0s`) — there's no such thing as a
  "transient" divergence. The textfile-exported series is labelled
  by `tier` (which unit found it) and `reason ∈ {chain | sequence |
  checkpoint}` — the reason label tells you which failure class you
  have (chain break / sequence gap = our lake's internal integrity;
  checkpoint = cross-archive comparison). The in-process
  `-metrics-listen` view of the same counter additionally carries
  `chunk_idx`; the textfile aggregates that away because a chunk
  index is a per-run worker slot with no cross-run meaning.
- The same event ALSO trips
  [`verify-archive-unit-failed.md`](verify-archive-unit-failed.md)
  (severity ticket) — a mismatch aborts the run non-zero. Expect
  both; this page is the one to work.

## Who produces the signal

There is no "history-scanner job". The real producers are:

- **Tier A** — nightly incremental chain walk
  (`verify-archive-tier-a.timer` → `-tier chain
  -from-last-verified` with a state file, under
  `run-heavy-job.sh` semantics).
- **Tier B** — checkpoint cross-check against the local
  archivist mirror (`verify-archive-tier-b.{service,timer}`).
- **Tier D** — weekly multi-peer sampling: root cron, Sunday
  04:23 (`-tier peers -peer-samples 50`, output to journald tag
  `stellarindex-tier-d`). **Still metric-less** — Tier D runs
  outside the systemd units that carry `-textfile-output`, so a
  Tier D divergence pages nobody. Read the journal.

Tier A and Tier B publish through node_exporter's textfile
collector:

| Unit | `-tier` | `.prom` file | series |
| ---- | ------- | ------------ | ------ |
| `verify-archive-tier-a.service` | `chain` | `/var/lib/node_exporter/textfile_collector/verify_archive_tier_a.prom` | `…mismatches_total{tier="chain",reason=…}` |
| `verify-archive-tier-b.service` | `checkpoint` | `/var/lib/node_exporter/textfile_collector/verify_archive_tier_b.prom` | `…mismatches_total{tier="checkpoint",reason=…}` |

The files are written on EVERY exit, success or failure (a mismatch
aborts the walk, and that is exactly the run whose counter matters).
Totals are cumulative across runs — a clean run re-emits the
previous total rather than resetting it, which is what makes
`increase()` meaningful. If a `.prom` file is missing or its mtime
is older than the last timer trigger, the page is blind: check
`journalctl -u verify-archive-tier-a | grep textfile`.

### Known blind spots

Every divergence the Tier A / Tier B walk can detect now increments
`stellarindex_verify_archive_mismatches_total`, including the two
seams that are checked OUTSIDE the per-chunk walk and used to abort
the run without touching the counter (so they surfaced only as the
severity-ticket `verify-archive-unit-failed`, never as this page):
cross-chunk boundaries (`stitchChunks`, ~11 of them in every
12-worker nightly run) and the cross-run resume seam
(`checkResumeFromHash`, which only runs under `-safety-overlap 0`;
the r1 units pass 5000, so on r1 the overlap re-walk covers that seam
instead). Both are counted as of 2026-08-29 and pinned by
`TestStitchChunks_BoundaryBreakIsPageable` /
`TestCheckResumeFromHash_MismatchIsPageable`.

What this page still cannot see:

1. **Tier D and Tier E.** Tier D (weekly multi-peer sampling, root
   cron) and Tier E (`rs-stellar-archivist` scan, operator-run) run
   outside the two systemd units that carry `-textfile-output` and
   emit no metric of any kind. A Tier D divergence surfaces in
   journald (`journalctl -t stellarindex-tier-d`) only — nothing
   pages. Wiring them is not part of #282.
2. **The first run on a host that has no `.prom` file yet.** The
   file is written when the run exits, so the very first run
   publishes a series that APPEARS at its final value. If that
   first-ever run is also the one that finds a break, the series
   appears at 1 and stays flat, and `increase()` has no earlier
   sample to subtract — it reads 0, so this page waits for the NEXT
   run's increment (the following night). Priming both units by hand
   during the apply (step 2 of the banner) is what closes this
   window: it puts the zero baseline on disk before any nightly run,
   narrowing the exposure to that single manual run. Read the
   priming run's exit status rather than trusting the page for it.

## Quick diagnosis (≤ 5 min)

```sh
ssh root@136.243.90.96

# Which checkpoint, which bucket, what's the mismatch?
journalctl -u verify-archive-tier-a -n 200 --no-pager
journalctl -u verify-archive-tier-b -n 200 --no-pager
journalctl -t stellarindex-tier-d -n 200 --no-pager   # weekly peer sweep

# Pull the hash from our archive. /srv/history-archive is a plain
# ZFS path (dataset data/archive) — NOT a MinIO bucket; mc has no
# business here. Checkpoint hex 0xAABBCCDD →
# history/AA/BB/CC/history-aabbccdd.json:
cat /srv/history-archive/history/<AA>/<BB>/<CC>/history-<ckpt-hex>.json | jq .currentBuckets

# Pull the same checkpoint from a reference
curl -s https://history.stellar.org/prd/core-live/core_live_001/history/<AA>/<BB>/<CC>/history-<ckpt-hex>.json | jq .currentBuckets

# Diff the bucket hashes — there'll be at least one row that
# differs. That's the corrupted / diverged bucket. (For
# reason="chain"/"sequence" the journal names the exact ledger
# range instead — that's an internal lake break, no reference
# needed.)
```

## Typical root causes (in order of how much you should worry)

1. **Bit rot / storage corruption.** ZFS scrubs should catch this
   before a reader does, but a drive silently returning bad data
   between scrub cycles can corrupt buckets.
   - Investigation: `zpool status -v data` for any scrub errors on
     the archive pool.

2. **Core binary bug** — stellar-core produced a different bucket
   hash than the rest of the network. Not applicable on today's
   posture (we don't publish — the mirror was filled by
   `stellar-archivist mirror`); retained for Phase-3 (ADR-0004
   validator rollout).
   - Investigation: are we running stock stellar-core from a
     tagged release?

3. **Compromised archive storage**. Someone (malicious or mistaken
   operator) overwrote our archive with wrong data. Check
   access logs + recent filesystem mtimes under
   `/srv/history-archive`.

4. **Scanner bug.** verify-archive itself reports divergence when
   the truth is our archive is correct. Rare but possible —
   cross-check one of the reference archives against another to
   confirm it's us.

## Mitigation (urgent)

- [ ] Step 1 — **stop advertising the affected checkpoints** so
      downstream consumers stop relying on our data. (Applies when
      we serve the archive publicly / Phase-3; today the blast
      radius is our own ingest + verification chain.)

- [ ] Step 2 — determine the extent. Is it one bucket or many?
      One checkpoint or several? For `reason="chain"/"sequence"`:
      how wide is the broken ledger range?

- [ ] Step 3 — if storage corruption: restore the affected range
      from an independent copy. Today that means the AWS public
      blockchain bucket or an external reference archive (SDF /
      LOBSTR / Satoshipay) — we run **one** local mirror; the
      "three geographically-separated validator archives" are the
      ADR-0004 **aspiration**, not a present-tense resource.

- [ ] Step 4 — if core-binary bug (Phase-3 posture): this is a
      sev-1 engineering incident. Contact SDF; coordinate with the
      broader core community; potentially do an emergency binary
      swap.

- [ ] Step 5 — if compromise: SECURITY incident. Rotate archive
      credentials, inspect access logs, engage security-ops. See
      SECURITY.md.

- [ ] Verification: a full verify-archive pass over the affected
      range completes with zero mismatches; bucket hashes match
      references for all recent checkpoints.

## Root cause analysis

- Forensic copy of the diverged bucket (DO NOT overwrite it until
  analysis is done).
- Hash chain: which checkpoint was the first to diverge? Work
  back to that point.
- Storage-layer evidence: `zpool status -v data`, scrub history,
  SMART state of the backing drives.
- (Phase-3) Core version + host state when the checkpoint was
  generated.

## Known false-positive patterns

Very few. The spec is deterministic; a divergence is almost
always real. But:

- **Scanner race with an in-flight write** — scanning the mirror
  *during* a re-mirror / repair pass can read a partial file. The
  scanner should retry; if it's alerting without retry, fix the
  scanner.
- **Reference archive is the one that's wrong.** Improbable
  (they're the source of truth) but if two of the three reference
  archives agree with us and only one disagrees, it might be
  them. Cross-verify before panicking.

## Related

- `verify-archive-unit-failed.md` — the ticket-severity sibling a
  mismatch also trips (the run exits non-zero).
- `verify-archive-run-stale.md` — the timer-staleness sibling.
- `archive-publish.md` — when we fail to publish at all
  (Phase-3; inert everywhere today).
- ADR-0004 (three-validator aspiration + independent archives).
- ADR-0016 (per-region storage + trust model).
- SECURITY.md — if compromise suspected.

## Changelog

- 2026-08-29 (later, #282) — **the page is no longer inert.** The
  tier-a/tier-b units now pass `-textfile-output`, so
  `stellarindex_verify_archive_mismatches_total` reaches Prometheus
  via node_exporter's textfile collector instead of dying with the
  process (new emitter:
  `internal/ops/archive/verify_archive_textfile.go`; cumulative,
  zero-seeded, `tier`-labelled). The rule's lookback widened
  `1h → 26h` because the producer is a nightly timer — a 1h window
  showed the step for one hour in twenty-four and the SEV-1 page
  self-resolved before morning. Banner rewritten; symptoms updated
  (`chunk_idx` is not on the exported series). Pinned by
  `deploy/monitoring/rule-tests/stellar_test.yml` +
  `internal/ops/archive/verify_archive_unit_wiring_test.go`.
  Requires an ansible apply on r1 to take effect.
- 2026-08-29 — re-verified against HEAD. Symptom metric corrected:
  `stellarindex_archive_divergence_total` never existed — the rule
  (repointed 2026-06-11, F-1329) was
  `increase(stellarindex_verify_archive_mismatches_total[1h]) > 0`,
  `for: 0s`, labels `chunk_idx` + `reason∈{chain|sequence|checkpoint}`.
  Banner replaced (was triple-false: cited the nonexistent
  `scripts/ops/archive-cross-check.sh`, the wrong metric, and
  claimed the alert was live): the counter is only exported via
  the CLI's opt-in `-metrics-listen` flag which the tier-a/b units
  don't pass, prometheus.r1.yml has no verify-archive scrape job,
  and Tier D emits no metric — so the alert is INERT IN PRACTICE
  and a real mismatch surfaces as `verify_archive_unit_failed`
  (ticket, not this page); tracked in #282, with a TODO to wire or
  repoint — closed out by the entry above, later the same day. `mc cat myminio/history-archive/...` replaced —
  /srv/history-archive is a plain ZFS path (dataset data/archive),
  hex-sharded layout shown. "History-scanner job" replaced with
  the real producers (nightly Tier A incremental walk, Tier B
  mirror cross-check, weekly Tier D peer cron with journald tag
  stellarindex-tier-d — added to diagnosis). Impact widened to
  lake-integrity failures (chain/sequence reasons). "Restore from
  a replica archive (we run three)" corrected: one mirror today +
  AWS public bucket + external references; three archives are the
  ADR-0004 aspiration. `zpool status -v data` (pool name). Rule
  citation → `rules.r1/stellar.yml`; commands use r1 shapes.
- 2026-04-23 — initial draft. Urgency justified: this is a
  correctness guarantee we've explicitly committed to in ADR-0004.
- 2026-04-30 — top-of-file deployment-posture callout. r1 doesn't
  publish today (stellar-core removed 2026-04-23); the alert
  remains live via the cross-check script, but root causes /
  mitigation steps tied to a publishing core don't apply on the
  current posture. Retained for Phase-3 validator rollout.
  (Superseded 2026-08-29 — the cross-check script never existed;
  see the current banner.)
