---
title: Runbook — binary version skew alerts
last_verified: 2026-10-05
status: draft
---
# Runbook — binary version skew

Rules: `deploy/monitoring/rules/binary-version-skew.yml` and `configs/prometheus/rules.r1/binary-version-skew.yml` (identical), group `stellarindex.binary_version_skew`, `severity: ticket` (P3), `component: deploy`. Typical MTTR: minutes (re-run the deploy naming the stale binary; the lasting fix is a one-line PR).

**Producer.** `stellarindex-binary-version-probe.timer` (`OnCalendar=*-*-* *:09,39:00 UTC`, every 30 min) runs `stellarindex-binary-version-probe.service` -> `/usr/local/sbin/stellarindex-binary-version-probe.sh` (defined in `configs/ansible/roles/archival-node/tasks/10-observability.yml`), which writes `/var/lib/node_exporter/textfile_collector/stellarindex_binary_version.prom` for node_exporter's textfile collector. Metrics: `stellarindex_binary_version_skew` (distinct installed versions among managed binaries, minus one; 0 = all agree), `stellarindex_binary_version_info{binary,managed,version}`, `stellarindex_binary_version_binaries_total` (managed binaries that answered), `stellarindex_binary_version_unmanaged_total`, `stellarindex_binary_version_probe_success` (0/1 canary). The post-deploy served-path smoke in `.github/workflows/deploy.yml` starts the same service and asserts skew 0 / probe_success 1.

**Scope.** The release-managed set only, one binary per `cmd/` dir: `stellarindex-aggregator`, `-api`, `-indexer`, `-migrate`, `-ops`, `-sla-probe` (explicit `MANAGED` list in the probe). Hand-built one-offs (e.g. `stellarindex-ops-ch`, `-ops-claimable`, `-ops-sacfix`, referenced by no systemd unit) are reported with `managed="false"` and counted in `unmanaged_total` but excluded from skew: no deploy ever updates them, so including them would pin the alert firing forever. Growth in `unmanaged_total` is cruft on the box, not a deploy fault. `*.prev-*`, `*.rolledback-*`, `*.bak`, `*.tmp` (parked deploy copies) are skipped; if they are counted, the probe's `case` filter regressed. The probe tries `-version`, `version`, `--version` per binary and shape-checks the answer.

**Why it matters.** The same miss happened twice: the deploy workflow's default `binaries` list (`.github/workflows/deploy.yml`) omitted a binary (`stellarindex-sla-probe`, then `stellarindex-ops`), and a deploy that never touches a binary still exits 0. `stellarindex-ops` is exec'd by data-integrity gates (`verify-archive-tier-a`, `verify-archive-tier-b`, `archive-completeness`, `ch-schema-drift`, `ch-schema-snapshot`, `restore-drill`), rollups/writers (`census-rollup`, `holders-rollup`, `creators-rollup`, `sponsors-rollup`, `supply-snapshot`, `sep1-refresh`, `directory-sync`, `cap67-movements`) and `galexie-archive-trim`. A stale gate checks against retired rules, so it PASSES when a current binary would reject (false negative). `stellarindex-migrate` is the schema-migration runner: `deploy-binary.yml` runs `stellarindex-migrate up` before any binary swap (F-1220), so a stale migrate applies every release's migrations with an old wrapper.

**Design.** The alert deliberately does not compare to an "expected" version (the host has no authoritative notion of the current release); it asserts that binaries from one release tag agree, so it works for binaries added later.

**Diagnose (<= 2 min).**

```promql
stellarindex_binary_version_info            # which binary is the odd one out
stellarindex_binary_version_skew            # distinct versions minus one
stellarindex_binary_version_probe_success   # 0 => skew is UNDERSTATED: a binary that will not run dropped out of the count
```

On the host (`stellarindex-migrate` answers `version`, not `-version`):

```bash
for b in /usr/local/bin/stellarindex-*; do
  case "$b" in *.prev-*|*.rolledback-*) continue ;; esac
  printf '%-34s ' "$b"; { "$b" -version || "$b" version; } 2>&1 | head -1
done
```

## At a glance

- [`stellarindex_binary_version_skew`](#stellarindex_binary_version_skew)
- [`stellarindex_binary_version_probe_degraded`](#stellarindex_binary_version_probe_degraded)
- [`stellarindex_binary_version_probe_stale`](#stellarindex_binary_version_probe_stale)

## stellarindex_binary_version_skew

- **Trips:** `stellarindex_binary_version_skew > 0`, `for: 45m`. 45m spans one probe interval (30m) plus slack, so a deploy mid-flight (binaries swap one at a time) resolves within one run and never alerts; real drift persists across runs.
- **Impact:** none directly; the risk is a stale gate binary silently running retired rules.
- **Fix:**
  1. Re-run the deploy workflow with an explicit list naming the stale binary at the version the others report: `binaries=stellarindex-ops`, `version=<version the other binaries report>`. `stellarindex-ops`, `stellarindex-migrate` and `stellarindex-sla-probe` are in `cli_binaries` in `configs/ansible/playbooks/deploy-binary.yml` (the deny-list): they get a `-version` smoke test and no service restart.
  2. Confirm skew returns to 0 within one probe interval (<= 30 min), or immediately via `systemctl start stellarindex-binary-version-probe.service`.
  3. Fix the cause: if the binary was missing from the `binaries` input default in `.github/workflows/deploy.yml`, add it there in the same PR, otherwise the next release reintroduces the drift.
  4. If the drifted binary was `stellarindex-ops`, re-run gates that ran while stale and treat their earlier PASS as unproven: `systemctl start verify-archive-tier-a.service`, `systemctl start archive-completeness.service`.
- **False positives:** a deploy in flight (absorbed by `for: 45m`); a deliberately pinned binary (e.g. rollback under investigation): the alert is correct, so silence it explicitly for the duration rather than editing the rule.
- **Ordering note:** `stellarindex-migrate up` runs before binaries are swapped and the migrate binary is swapped in that same later step, so the deploy that first ships a new migrate still applies its migrations with the previous runner; a migrate bump is always one deploy behind. It converges and is safe; making the runner update first is a structural change to the migration path on a live money database and needs its own reviewed change.

## stellarindex_binary_version_probe_degraded

- **Trips:** `stellarindex_binary_version_probe_success == 0`, `for: 2h`. A probe that stops reporting must not read as "no skew".
- **Means:** the probe ran but at least one MANAGED binary that IS PRESENT did not answer `-version` (non-executable, wrong architecture, or timed out; the probe bounds each call with `timeout 10s`). That binary is excluded from skew, so `stellarindex_binary_version_skew` is understated while this fires.
- **Not covered:** an ENTIRELY ABSENT binary. The probe globs the install dir, so an absent release binary yields skew=0 and probe_success=1. Detecting absence needs a host-derived expected set (installed systemd units, or an ansible-templated set keyed on `run_aggregator`/region), deliberately not a hardcoded count of six (would pin the alert red on testnet/futurenet, where fewer binaries are installed by design). Until then absence is caught by the unit that fails to start.
- **Diagnose/fix:** compare `stellarindex_binary_version_binaries_total` to the number of `stellarindex-*` files in `/usr/local/bin` to see how many dropped out; run the host loop above to find the one that fails; fix its permissions/architecture or redeploy it as under `stellarindex_binary_version_skew`.

## stellarindex_binary_version_probe_stale

- **Trips:** `(time() - max without (file) (node_textfile_mtime_seconds{file="/var/lib/node_exporter/textfile_collector/stellarindex_binary_version.prom"})) > 5400` OR `absent_over_time(stellarindex_binary_version_probe_success[90m])`, `for: 10m`. The mtime arm catches a frozen file (the only signal that stops advancing while the gauges stay present); the absent arm catches a file never written. 90m tolerates two missed 30-min runs.
- **Means:** the probe crashed before writing its textfile or `stellarindex-binary-version-probe.timer` stopped firing; node_exporter keeps serving the last values, so `_skew` and `_probe_degraded` cannot detect drift or their own absence.
- **Diagnose/fix:** `systemctl status stellarindex-binary-version-probe.timer` on the host; `systemctl start stellarindex-binary-version-probe.service` and check the journal and that `/var/lib/node_exporter/textfile_collector/stellarindex_binary_version.prom` is rewritten and mode 0644 (node_exporter runs unprivileged and skips unreadable files). If the unit is missing, the `observability` tag of `configs/ansible/roles/archival-node/tasks/10-observability.yml` has not been applied.

Related: [stellar-stack-version-lag](stellar-node.md#stellar-stack-version-lag) (third-party core/galexie/archivist probe).

## Related

- [Alerts catalogue](../alerts-catalog.md)
