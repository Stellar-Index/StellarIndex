---
title: Runbook — binary-version-skew
last_verified: 2026-08-28
status: living
severity: P3
---

# Runbook — `stellarindex_binary_version_skew` / `stellarindex_binary_version_probe_degraded`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_binary_version_skew` (P3, ticket, skew > 0 sustained 45m) · `stellarindex_binary_version_probe_degraded` (P3, ticket, probe canary 0 for 2h) |
| Severity | P3 — no user-visible impact at the moment it fires; the risk is a data-integrity gate silently running retired rules |
| Detected by | `deploy/monitoring/rules/binary-version-skew.yml` (and `configs/prometheus/rules.r1/binary-version-skew.yml`) |
| Metric source | `node_exporter` textfile_collector reads `/var/lib/node_exporter/textfile_collector/stellarindex_binary_version.prom`, refreshed every 30 min by `stellarindex-binary-version-probe.timer` → `/usr/local/sbin/stellarindex-binary-version-probe.sh` |
| Scope | The **release-managed** set only — one binary per `cmd/` directory (`aggregator`, `api`, `indexer`, `migrate`, `ops`, `sla-probe`). Hand-built operator one-offs and the deploy playbook's parked `*.prev-*` / `*.rolledback-*` copies are excluded from skew; the one-offs are still reported via `managed="false"`. |
| Typical MTTR | Minutes — re-run the deploy workflow naming the stale binary. The lasting fix (adding it to the default list) is a one-line PR. |
| Impact | None directly. The danger is indirect and one-way: a stale **gate** binary validates the lake against retired rules, so it PASSES when it should fail. |

## Why this exists

The same failure happened twice, three months apart, because the first
fix corrected one binary instead of the blind spot.

- **2026-05-13 (F-1314).** `stellarindex-sla-probe` was drifting: the
  deploy workflow's default binary list omitted it, so the SLA-evidence
  binary went stale (and was absent entirely on a fresh host) while
  every deploy reported success. Its presence on r1 came from an
  operator-side install during wave-129, not from the release flow.
  Fixed by adding that one name to the list.

- **2026-08-28.** The identical failure for `stellarindex-ops`, which
  the same list also omitted. Measured on r1: `ops` at **v0.44.7**
  while `indexer` / `aggregator` / `api` / `sla-probe` were all at
  **v0.46.1** — two releases of drift, invisible because *a deploy that
  never touches a binary still exits 0*.

`stellarindex-ops` is not a bystander. Thirteen units on r1 exec it:

| Kind | Units |
| ---- | ----- |
| Data-integrity gates | `verify-archive-tier-a`, `verify-archive-tier-b`, `archive-completeness`, `ch-schema-drift`, `ch-schema-snapshot`, `restore-drill` |
| Served-tier writers | `census-rollup`, `holders-rollup`, `supply-snapshot`, `sep1-refresh`, `directory-sync`, `cap67-movements` |
| Storage maintenance | `galexie-archive-trim` |

- **2026-08-28, found by this probe on its first run.**
  `stellarindex-migrate` at **v0.28.1** against v0.46.1 elsewhere —
  roughly 18 releases, and the worst of the three because it is the
  **schema-migration runner**. `deploy-binary.yml` executes
  `stellarindex-migrate up` (F-1220) *before* any binary swap, so every
  deploy was applying that release's migrations with a months-old
  golang-migrate wrapper. Both `ops` and `migrate` are now in the
  workflow's default list.

The gate case is the one that matters. `verify-archive` and
`archive-completeness` decide whether the lake is trustworthy. A stale
gate checks against rules that have since been retired or tightened, so
its failure mode is a **false negative** — it reports PASS on data a
current binary would reject. That is the single direction a
data-integrity gate must never fail in, and nothing else in the stack
would notice.

## Why the alert is shaped this way

It deliberately does **not** compare against an "expected" version. The
host has no authoritative notion of the current release, and hardcoding
one would make the probe lie after every legitimate release until
something re-templated it. Instead it asserts only that binaries shipped
from a single release tag should all report the same version.
Disagreement among them is self-evidently wrong and needs no external
reference — which also means the alert keeps working for binaries added
in the future without anyone updating a list.

`for: 45m` spans one probe interval (30 min) plus slack, so a deploy
legitimately mid-flight — binaries swap one at a time — resolves within
one run and never alerts. Real drift persists across every subsequent
run.

## Quick diagnosis (≤ 2 min)

```promql
# 1. Which binary is the odd one out?
stellarindex_binary_version_info

# 2. How many distinct versions are installed (minus one)?
stellarindex_binary_version_skew

# 3. Did every binary answer? If this is 0, the skew number is
#    UNDERSTATED — a binary that will not run dropped out of the count.
stellarindex_binary_version_probe_success
```

On the host:

```bash
for b in /usr/local/bin/stellarindex-*; do
  case "$b" in *.prev-*|*.rolledback-*) continue ;; esac
  printf '%-34s ' "$b"; "$b" -version 2>&1 | head -1
done
```

## Remediation

1. **Re-deploy the stale binary.** Run the deploy workflow with an
   explicit list naming it, at the version the others are on:

   ```
   binaries=stellarindex-ops
   version=<the version the other binaries report>
   ```

   `stellarindex-ops`, `stellarindex-migrate` and
   `stellarindex-sla-probe` are in `deploy-binary.yml`'s `cli_binaries`
   deny-list, so they get a `-version` smoke test and no service
   restart — deploying them does not bounce anything.

2. **Confirm the metric clears.** `stellarindex_binary_version_skew`
   returns to 0 within one probe interval (≤ 30 min), or immediately
   via `systemctl start stellarindex-binary-version-probe.service`.

3. **Fix the cause, not the instance.** If the binary was missing from
   the deploy workflow's default `binaries` input
   (`.github/workflows/deploy.yml`), **add it there in the same PR**.
   Skipping this step is exactly how the 2026-05-13 fix failed to
   prevent the 2026-08-28 recurrence.

4. **Re-run any gate that ran stale.** If the drifted binary was
   `stellarindex-ops`, the integrity gates that fired while it was
   behind returned results from the old rules. Re-run them once
   current:

   ```bash
   systemctl start verify-archive-tier-a.service
   systemctl start archive-completeness.service
   ```

   Treat their previous PASS as unproven, not as evidence.

## Known false-positive patterns

- **A deploy in flight.** Binaries swap sequentially, so skew is
  briefly non-zero mid-run. The 45-minute `for:` absorbs this; if you
  are watching the metric directly during a deploy, expect it.
- **A deliberately pinned binary.** If an operator has intentionally
  held one binary back (e.g. a rollback under investigation), this
  alert is correct but expected. Silence it explicitly for the duration
  rather than editing the rule.
- **`*.prev-*` / `*.rolledback-*` files.** The probe skips these by
  design; they are the deploy playbook's parked previous builds. If you
  see them counted, the probe's `case` filter has regressed.
- **Hand-built operator one-offs.** r1 carries
  `stellarindex-ops-ch` (v0.21.3, Jul 29),
  `stellarindex-ops-claimable` and `stellarindex-ops-sacfix` (both
  `dev`, Jul 27), referenced by **no systemd unit**. They are reported
  with `managed="false"` and counted in
  `stellarindex_binary_version_unmanaged_total`, but deliberately
  excluded from skew: no deploy will ever update them, so including
  them would pin this alert firing forever — and a permanently-firing
  alert is the same as no alert. If `unmanaged_total` grows, that is
  cruft accumulating on the box, not a deploy fault.

## Note on `stellarindex-migrate` ordering

`deploy-binary.yml` runs `stellarindex-migrate up` **before** swapping
binaries (F-1220, so a service never starts against a schema older than
it expects). The migrate binary is swapped in that same later step, so
the deploy that first ships a new migrate still applies its migrations
with the previous runner; the new one takes effect from the next
deploy. This converges and is safe, but it means a migrate version bump
is always one deploy behind. Making the runner update before it runs is
a structural change to the migration path on a live money database and
should be its own reviewed change.

## Related

- `docs/operations/runbooks/stellar-stack-version-lag.md` — the sibling
  probe for THIRD-PARTY components (core / galexie / archivist).
- `.github/workflows/deploy.yml` — the default `binaries` list; the
  origin of both incidents.
- `configs/ansible/playbooks/deploy-binary.yml` — `cli_binaries`,
  which decides whether a binary gets a service restart or a version
  smoke test.
- `configs/ansible/roles/archival-node/tasks/10-observability.yml` —
  where the probe script, service and timer are defined.

## Changelog

- **2026-08-28** — created, after `stellarindex-ops` was found at
  v0.44.7 against v0.46.1 elsewhere on r1: the F-1314 class recurring
  because that fix named one binary rather than closing the gap.
