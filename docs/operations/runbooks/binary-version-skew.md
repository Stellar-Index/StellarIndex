---
title: Runbook — binary version skew alerts
last_verified: 2026-10-05
status: draft
---
# Runbook — binary version skew

Rules: `deploy/monitoring/rules/binary-version-skew.yml` and `configs/prometheus/rules.r1/binary-version-skew.yml` (identical), group `stellarindex.binary_version_skew`, `severity: ticket` (P3), `component: deploy`.

**Producer.** `stellarindex-binary-version-probe.timer` (every 30 min, `*:09,39 UTC`) runs `stellarindex-binary-version-probe.service` -> `/usr/local/sbin/stellarindex-binary-version-probe.sh` (`configs/ansible/roles/archival-node/tasks/10-observability.yml`), writing `/var/lib/node_exporter/textfile_collector/stellarindex_binary_version.prom`. Metrics: `stellarindex_binary_version_skew` (distinct versions among managed binaries minus one; 0 = agree), `stellarindex_binary_version_info{binary,managed,version}`, `stellarindex_binary_version_binaries_total`, `stellarindex_binary_version_unmanaged_total`, `stellarindex_binary_version_probe_success` (0/1 canary).

**Scope.** Release-managed set only: `stellarindex-aggregator`, `-api`, `-indexer`, `-migrate`, `-ops`, `-sla-probe` (the `MANAGED` list in the probe). Hand-built one-offs are reported with `managed="false"` and excluded from skew; growth in `unmanaged_total` is cruft, not a deploy fault. `*.prev-*`, `*.rolledback-*`, `*.bak`, `*.tmp` are skipped (if counted, the probe's `case` filter regressed). It asserts that binaries agree with each other, not with an expected version. A stale `stellarindex-ops` (exec'd by the verify/rollup gates) silently runs retired rules and PASSES where a current binary would reject.

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

P3 ticket. MTTR minutes: re-run the deploy naming the stale binary. Risk is a stale gate binary running retired rules.

## stellarindex_binary_version_skew

**Symptom.** `stellarindex_binary_version_skew > 0`, `for: 45m` (spans one probe interval, so a deploy in flight never alerts).

**Fix.**

1. Re-run the deploy workflow naming the stale binary at the version the others report: `binaries=stellarindex-ops`, `version=<version the other binaries report>`. `stellarindex-ops`, `-migrate`, `-sla-probe` are in `cli_binaries` in `configs/ansible/playbooks/deploy-binary.yml`: `-version` smoke test, no restart.
2. Confirm skew returns to 0 within 30 min, or at once via `systemctl start stellarindex-binary-version-probe.service`.
3. If the binary was missing from the `binaries` default in `.github/workflows/deploy.yml`, add it there in the same PR.
4. If it was `stellarindex-ops`, treat earlier gate PASSes as unproven and re-run: `systemctl start verify-archive-tier-a.service`, `systemctl start archive-completeness.service`.

False positives: a deliberately pinned binary (rollback under investigation) is a correct firing; silence it explicitly. A migrate bump is always one deploy behind (`stellarindex-migrate up` runs before the swap); it converges and is safe. **Escalate:** none (ticket).

## stellarindex_binary_version_probe_degraded

**Symptom.** `stellarindex_binary_version_probe_success == 0`, `for: 2h`: a PRESENT managed binary did not answer `-version` (non-executable, wrong architecture, 10 s timeout) and is excluded from skew, so skew is understated.

An entirely ABSENT binary is not detected (skew 0, success 1); it surfaces as the unit that fails to start.

**Fix.** Compare `stellarindex_binary_version_binaries_total` with the `stellarindex-*` files in `/usr/local/bin`, run the host loop above to find the failing one, fix permissions/architecture or redeploy as under skew.

## stellarindex_binary_version_probe_stale

**Symptom.** `for: 10m`: `(time() - max without (file) (node_textfile_mtime_seconds{file="/var/lib/node_exporter/textfile_collector/stellarindex_binary_version.prom"})) > 5400` OR `absent_over_time(stellarindex_binary_version_probe_success[90m])`. The probe crashed or the timer stopped; node_exporter keeps serving old values, so the other two alerts cannot see drift.

**Fix.** `systemctl status stellarindex-binary-version-probe.timer`; `systemctl start stellarindex-binary-version-probe.service`; check the journal and that the `.prom` file is rewritten at mode 0644 (node_exporter skips unreadable files). Missing unit: apply the `observability` tag of `configs/ansible/roles/archival-node/tasks/10-observability.yml`.

## Related

- [stellar-stack-version-lag](stellar-node.md#stellar-stack-version-lag), [Alerts catalogue](../alerts-catalog.md)
