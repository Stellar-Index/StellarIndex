---
title: Runbook — archive-files-missing
last_verified: 2026-08-29
status: current
severity: P2
---

# Runbook — `stellarindex_archive_files_missing`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_archive_files_missing` |
| Severity | P2 (`severity: ticket`) |
| Detected by | `deploy/monitoring/rules/archive-completeness.yml` + `configs/prometheus/rules.r1/archive-completeness.yml` (identical in both trees) |
| Expr | `archive_files_missing > 0` for `4h` |
| Typical MTTR | 5–15 min (the next daily run refills); 1–4 h (manual after the fallback chain is exhausted) |
| Impact | Reduced redundancy on the cross-region integrity guarantee. API responses get `flags.reduced_redundancy = true` while the gap persists; rate data itself remains served correctly from CAGGs. |

## Symptoms

- Prometheus gauge `archive_files_missing{archive="cross-anchor"}` > 0 for
  > 4 h. **Only that label value can appear today**: the shipped
  `archive-completeness` flow enforces the cross-anchor archive only
  (`internal/archivecompleteness/report.go` — `Report.Primary` "Nil in the
  current repo snapshot", so `writeFilesMissing` never emits a
  `galexie-archive` series). Galexie's own primary archive is covered by
  [galexie-archive-contiguity](galexie-archive-contiguity.md) and
  [galexie-archive-tip-lag](galexie-archive-tip-lag.md) instead.
- The daily `archive-completeness.timer` is still running — i.e.
  `archive_completeness_last_success_timestamp` is within 26 h (that gauge is
  the freshness signal, and the expression the sibling
  `stellarindex_archive_completeness_stale` alert uses). If it is *older* than
  26 h you are in [archive-completeness-stale](archive-completeness-stale.md),
  not here.
- The status page may already be in *Degraded performance* if R1 is affected,
  or *Operational* if only R2/R3.

## Quick diagnosis (≤ 5 min)

Identify how many files are missing and which fallback sources are failing.

```sh
# 1. The gauge + the freshness signal that proves the daily run is alive
ssh r1 'curl -s localhost:9100/metrics | grep -E "^archive_files_missing|^archive_completeness_last_success_timestamp"'

# 2. Which fallback sources are degraded (per-source counters, label `source`)
ssh r1 'curl -s localhost:9100/metrics | grep -E "^archive_completeness_repair_(attempts|failures)_total"'

# 3. The current gap report (post-fix state of the last run)
ssh r1 'jq ".range, (.cross_anchor.missing_count), (.cross_anchor.missing[0:5])" /var/lib/galexie/last-completeness-report.json'

# 4. Is the fallback chain reachable from r1? These nine ARE the chain
#    (internal/archivecompleteness/cross_anchor_fill.go::DefaultCrossAnchorSources)
#    — there is no AWS source in it.
ssh r1 '
for u in https://history.stellar.org/prd/core-live/core_live_001 \
         https://history.stellar.org/prd/core-live/core_live_002 \
         https://history.stellar.org/prd/core-live/core_live_003 \
         https://bootes-history.publicnode.org \
         https://lyra-history.publicnode.org \
         https://hercules-history.publicnode.org \
         https://archive.v1.stellar.lobstr.co \
         https://archive.v2.stellar.lobstr.co \
         https://archive.v5.stellar.lobstr.co
do
  code=$(curl -s -o /dev/null -m 10 -w "%{http_code}" "$u/.well-known/stellar-history.json")
  echo "$code $u"
done'
```

If most of the chain answers `200` and only a handful of checkpoints are
missing: a per-checkpoint 404 that the chain didn't resolve on the last pass.
Go to mitigation step 1 — the next run usually clears it.

If the whole chain is unreachable from r1, this is an egress/DNS incident on
the host, not an archive incident — every other outbound poller will be failing
too. Triage the host first ([host-down](host-down.md),
[all-ingestion-down](all-ingestion-down.md)) and come back once egress is
restored; the next daily run refills on its own.

## Mitigation (≤ 15 min)

- [ ] **Step 1 — re-run the daily unit.** This is the whole command; the unit's
      `ExecStartPre` (`compute-archive-to.sh`) derives `-to` from
      `ingestion_cursors` and `ARCHIVE_FROM` tracks the ADR-0027 hot floor, so
      the range is always the right one for this host. It is idempotent and
      only refetches what is still missing.

  ```sh
  ssh r1 'systemctl start archive-completeness.service'
  ssh r1 'journalctl -u archive-completeness.service -f --since="5 min ago"'
  ```

- [ ] **Step 2 — if step 1 didn't clear it, run the range by hand with more
      workers.** The modes are `check` / `fix` / `verify` (`fix` = check +
      fallback-fill; `verify` = check → fix → re-check → emit the Prometheus
      textfile). There is no `-force-all-sources` flag — the chain already
      tries all nine sources in order for every still-missing file.

  **`-to` is REQUIRED and `-to 0` errors out** (`-to is required`), so pass the
  real head. Take `-from` from the unit rather than typing `2`: on a host that
  has been trimmed per ADR-0027 the pre-hot-floor range is deliberately empty
  and a hand-typed `-from 2` reports the trimmed cold range as "missing".

  ```sh
  ssh r1 'systemctl show -p Environment archive-completeness.service; cat /run/archive-completeness.env'
  # → ARCHIVE_FROM=<hot floor>  ARCHIVE_TO=<cursor-derived head>

  ssh r1 'stellarindex-ops archive-completeness fix \
    -from <ARCHIVE_FROM> -to <ARCHIVE_TO> -workers 16 \
    -output-file /var/lib/galexie/last-completeness-report.json'
  ```

  Exit code 0 means no residual missing files; 1 means the fallback chain was
  exhausted on some (proceed to step 3).

- [ ] **Step 3 — if step 2 still leaves files unfilled, log the residual list as a known incident** and escalate to the responder for `archive-completeness-stale`. The remaining files are unrecoverable from any public archive; recovery requires either:
  - Spinning up our own validator and catching up from peers (multi-week, ADR-0004 territory)
  - Out-of-band request to a full-archive operator

  Both are outside the scope of this runbook.

- [ ] **Verification:** `archive_files_missing` should drop to 0 on the run that
      follows step 1 (the gauge is only rewritten when the tool emits its
      textfile, i.e. by `verify`; a hand-run `fix` clears the archive but the
      gauge waits for the next timer tick or a `verify` run). The
      `flags.reduced_redundancy` flag clears on every region's next health-poll
      cycle (~60 s after the gauge clears).

## Root cause analysis

For the postmortem, capture:

- The gap report JSON from `/var/lib/galexie/last-completeness-report.json`
  (`cross_anchor.missing` — which specific checkpoint ledgers, and the `range`
  that was walked).
- The full `archive_completeness_repair_attempts_total` /
  `archive_completeness_repair_failures_total` snapshot before mitigation,
  broken out per `source` label — that names which of the nine chain members
  failed.
- `journalctl -u archive-completeness.service` covering the last 3 daily runs
  (was the gap growing, stable, or sudden?).
- For chain-side failures: `curl -sf -w "%{http_code}"
  https://history.stellar.org/prd/core-live/core_live_001/.well-known/stellar-history.json`
  from r1 to confirm SDF's archive is reachable + valid, and the same for the
  publicnode / lobstr members that show failures.

## Known false-positive patterns

- **First few minutes after a fresh region bring-up.** A new R3 (Vultr) box
  starts with an empty cross-anchor archive; the first daily run after bring-up
  will report a large missing-files gauge until the mirror completes. Expected;
  suppress the alert for the bring-up window using the bring-up runbook's
  silencing step.
- **A hand-run with the wrong `-from` on a trimmed host.** ADR-0027 trims the
  cold range out of the local archive on purpose; verifying from ledger 2 on a
  trimmed host reports the whole cold range as missing. Always take `-from`
  from the unit (`ARCHIVE_FROM`), never from this page.
- **A test-net host.** Cross-anchor *fill* is pubnet-only: the nine default
  sources are pubnet archives, so `NewCrossAnchorFiller` REFUSES on
  `-network testnet|futurenet` rather than writing pubnet checkpoints into a
  test-net store (audit 2026-08-26). Test nets self-heal archive gaps from
  their own galexie/core.

## Related

- [ADR-0017](../../adr/0017-archive-completeness-invariants.md) — the policy decision (4 hard contracts).
- [archive-completeness.md](../archive-completeness.md) — the operational procedure overview.
- [archive-completeness-stale](archive-completeness-stale.md) — companion runbook for when the daily cron itself isn't running (`archive_completeness_last_success_timestamp` older than 26 h / 48 h).
- [archive-repair-source-degraded](archive-repair-source-degraded.md) — one chain member failing repeatedly while the gap still closes.
- [archive-divergence](archive-divergence.md) — different alert, fires when **content** of the archive diverges (hash mismatch); this runbook is for **presence** gaps only.
- [galexie-archive-contiguity](galexie-archive-contiguity.md) / [galexie-archive-tip-lag](galexie-archive-tip-lag.md) — the primary (galexie) archive's own coverage.
- Postmortems tagged `archive-files-missing` — `docs/operations/postmortems/`.

## Changelog

- 2026-08-29 — re-verified against HEAD (runbook Wave L, #319):
  `archive_completeness_runs_total` has never existed — the freshness signal is
  `archive_completeness_last_success_timestamp` within 26 h; only
  `archive="cross-anchor"` can fire (`Report.Primary` stays nil), so the
  galexie-archive arm and its 64k-partition false positive were describing an
  unshipped scan and now point at the galexie-archive alerts; AWS is not in the
  fallback chain (nine sources: SDF core-live ×3, publicnode ×3, lobstr ×3), so
  the AWS reachability probe is replaced by a chain probe; a `fix` mode DOES
  exist (check/fix/verify) but `-force-all-sources` is fictional and `-to 0`
  errors, so step 2 now reads the unit's own `ARCHIVE_FROM`/`ARCHIVE_TO`;
  `network-uplink` was a dead pointer.
- 2026-06-12 — F-1330: replace fictional `archive-completeness fix
  -input-file … -force-all-sources` with the real
  `archive-completeness verify -from … -to … -workers …` (single mode,
  check + fallback-fill in one pass).
- 2026-04-27 — initial draft alongside ADR-0017.
