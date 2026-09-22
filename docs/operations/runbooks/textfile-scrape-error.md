---
title: Runbook — textfile-scrape-error
last_verified: 2026-09-22
status: draft
severity: P3
---

# Runbook — `stellarindex_textfile_scrape_error`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_textfile_scrape_error` |
| Severity | P3 (ticket — a monitoring-coverage hole, not itself a customer-facing outage) |
| Detected by | Prometheus rule in `deploy/monitoring/rules/infra.yml` (and `configs/prometheus/rules.r1/infra.yml`) |
| Typical MTTR | 10–20 min — find the bad file, fix or remove it |
| Impact | node_exporter's textfile collector failed to parse one or more `.prom` files under `/var/lib/node_exporter/textfile_collector/`. Every metric sourced from the broken file stops being exported, so every alert built on it goes silent instead of firing. |

## Background

`node_exporter --collector.textfile.directory=/var/lib/node_exporter/textfile_collector`
re-reads every `*.prom` file in that directory on each scrape. It is the
delivery mechanism for a large share of this host's alert surface —
`archive_completeness.prom`, `sla_probe.prom`, `nvme.prom`,
`process_memory_mappings.prom`, the zfs-snapshot and binary/stack
version-skew probes, and more (see
`configs/ansible/roles/archival-node/tasks/10-observability.yml`).

If any one of those files is malformed — a bad `HELP`/`TYPE` line, a
partial write the collector caught mid-rename, stale permissions —
node_exporter logs the parse error, drops that file's series for the
scrape, and sets the collector-wide gauge `node_textfile_scrape_error`
to `1`. The metric carries no per-file label, so it cannot say which
file broke, only that something did.

Before this alert existed, that failure mode was invisible: the writer
scripts already use an atomic tmp-file + `mv` pattern
(10-observability.yml), so this is not a transient race — it signals a
genuinely broken exposition file that will keep failing every scrape
until someone intervenes, silently starving whichever alert(s) depend
on it.

## Symptoms

- `stellarindex_textfile_scrape_error` firing (ticket).
- One or more alerts that normally depend on a textfile-sourced metric
  (archive-completeness, sla-probe, nvme, process-mappings,
  zfs-snapshots, version-skew) have gone quiet without their underlying
  condition actually clearing — the tell that this is the cause, not a
  coincidence.

## Quick diagnosis (≤ 5 min)

```sh
# node_exporter's own log names the file and the parse error
journalctl -u node_exporter --since -1h | grep -i textfile

# confirm the gauge and which instance
curl -s http://<instance>:9100/metrics | grep node_textfile_scrape_error

# list the collector directory — a 0-byte or half-written file is the
# usual culprit
ls -la /var/lib/node_exporter/textfile_collector/
```

## Mitigation (≤ 15 min)

- [ ] Identify the broken file from the node_exporter log line.
- [ ] If it's a stale partial write (the writer's systemd unit died
      mid-run), remove the file — the owning unit's next run will
      regenerate it via the same tmp+mv path.
- [ ] If the writer script itself is emitting bad exposition syntax
      (a code regression), fix the writer; `scripts/ci/lint_textfile_exposition.py`
      is the CI-time static check for this class — run it against the
      writer's output locally to confirm the fix.
- [ ] Verification: `node_textfile_scrape_error` returns to `0` on the
      next scrape and the file's own metrics reappear.

## Root cause analysis

- Which writer/unit owns the broken file
  (`configs/ansible/roles/archival-node/tasks/10-observability.yml`
  names every textfile producer and its target filename).
- Whether the writer crashed mid-write (permissions, disk full) or is
  emitting malformed output on every run (code regression).
- How long the file was broken — check `journalctl` history for the
  first `textfile` parse-error line — since every alert sourced from it
  was blind for that whole window.

## Known false-positive patterns

None known. The writers all use the tmp+mv atomic-write pattern, so a
genuinely transient mid-write read is not expected to reach a 15-minute
`for:` window; if one does, that itself is worth investigating (a
writer racing its own rename).

## Related

- `configs/ansible/roles/archival-node/tasks/10-observability.yml` —
  every textfile-collector producer on this host.
- `scripts/ci/lint_textfile_exposition.py` — CI-time static lint
  against malformed exposition lines in the writer scripts; catches the
  regression class before it reaches a live host, but does not cover a
  runtime failure (disk full, permissions, a file left mid-write).
- `metrics-registry-absent.md` — the sibling "dead-alert-state made
  observable" alert one level up the stack (in-process Prometheus
  Registry, rather than the node_exporter textfile collector).

## Changelog

- 2026-09-22 — initial draft (T583: no runtime alert existed on
  `node_textfile_scrape_error`).
