---
title: Runbook — stellar-stack-version-lag
last_verified: 2026-07-09
status: living
severity: P3 | P1
---

# Runbook — `stellarindex_stellar_stack_lagging` / `stellarindex_stellar_stack_protocol_lag`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_stellar_stack_lagging` (P3, ticket, any lag ≥ 1 sustained 2d) · `stellarindex_stellar_stack_protocol_lag` (P1, page, lag ≥ 2 sustained 6h) |
| Severity | P3 → P1 escalation, same metric family |
| Detected by | `deploy/monitoring/rules/stellar-stack-version.yml` (and `configs/prometheus/rules.r1/stellar-stack-version.yml`) |
| Metric source | `node_exporter` textfile_collector reads `/var/lib/node_exporter/textfile_collector/stellar_stack_version.prom`, refreshed daily by `stellar-stack-version-probe.timer` → `/usr/local/sbin/stellar-stack-version-probe.sh` |
| Components | `core` (stellar-core), `galexie` (stellar-galexie), `archivist` (stellar-archivist) — via the `component` label |
| Typical MTTR | Not a "restart and it clears" alert — clearing it means completing the coordinated-upgrade checklist below, which is a deliberate maintenance-window operation, not an emergency reflex. |
| Impact | None while only `stellarindex_stellar_stack_lagging` is firing (lag=1, same protocol). At lag=2 (`stellarindex_stellar_stack_protocol_lag`), impact is a countdown to a real outage: the next network protocol-activation vote will freeze/crash-loop the lagging component exactly as it did on 2026-07-08/09. |

## Why this exists

Two incidents in one week, both the same root cause: nothing watched
whether the installed Stellar toolchain lagged upstream.

- **2026-07-08** — mainnet Protocol 27 activated. r1's stellar-core
  was on 26.0.1, which had no Protocol 27 support. Ingest froze for
  2.5 hours until an operator upgraded core by hand.
- **2026-07-09** — the very next day, the hand-installed galexie
  binary (an April build) crash-looped on the first ledger carrying
  CAP-0071 XDR (the new envelope shape Protocol 27 introduced).
  `galexie-v27.0.0` had been published upstream on **2026-06-10** —
  a full month before the incident — and nobody noticed. 5+ hours of
  on-chain ingest loss.

Both incidents share the same shape: a new Stellar protocol version
activates on the network's own schedule (a validator vote, not ours),
and every component in the ingest path that decodes ledger data has
to be upgraded to match BEFORE that activation, not after. There was
no automated signal that any of `stellar-core` / `stellar-galexie` /
`stellar-archivist` had fallen behind upstream. This probe closes
that gap.

## Symptoms

- `stellarindex_stellar_stack_lagging{component="..."}` firing: the
  probe found `component` behind the latest available upstream
  version, sustained across 2+ consecutive daily runs. Not urgent by
  itself if `stellarindex_stellar_stack_protocol_lag` is NOT also
  firing for the same component — it means a patch/point release is
  available within the SAME protocol major.
- `stellarindex_stellar_stack_protocol_lag{component="..."}` firing:
  `component` is a full protocol-major version behind upstream. This
  is the exact precondition for both founding incidents.
- `stellarindex_stellar_stack_probe_success == 0`: the probe itself
  degraded this run (see [Known false-positive
  patterns](#known-false-positive-patterns)) — not the same signal as
  an actual lag; check this before assuming the lag value is current.

## Quick diagnosis (≤ 5 min)

```sh
# 1. See exactly which component(s) are lagging + by how much, and
#    the raw installed version string for each.
ssh r1 'grep "^stellarindex_stellar_stack_" \
  /var/lib/node_exporter/textfile_collector/stellar_stack_version.prom'

# 2. Confirm the probe itself is healthy (ran recently, wrote fresh
#    output) rather than serving a stale value from a broken timer.
ssh r1 'systemctl status stellar-stack-version-probe.timer'
ssh r1 'systemctl status stellar-stack-version-probe.service'
ssh r1 'stat /var/lib/node_exporter/textfile_collector/stellar_stack_version.prom'

# 3. Cross-check against the live upstream state directly.
dpkg-query -W -f='${Version}\n' stellar-core stellar-archivist   # (run ON r1)
apt-cache policy stellar-core stellar-archivist                  # (run ON r1)
curl -s https://api.github.com/repos/stellar/stellar-galexie/releases/latest | jq -r '.tag_name, .published_at'
/usr/local/bin/galexie version                                   # (run ON r1)
```

## Decision tree

| What fired | Meaning | Action |
| ---------- | ------- | ------ |
| `stellarindex_stellar_stack_lagging` only, lag=1 | A same-protocol patch/point release is available | Schedule the upgrade during ordinary maintenance; no rush |
| `stellarindex_stellar_stack_protocol_lag`, lag=2 | A NEW PROTOCOL MAJOR is available and we're not on it | Treat as P1: schedule the coordinated-upgrade maintenance window below **now**, well before any activation vote — check https://stellar.org (or the SDF Discord/dashboard) for the announced activation date and work backward from it |
| `stellarindex_stellar_stack_probe_success == 0` with no lag change | The probe degraded (GitHub API, apt state) — not evidence either way | Re-run manually: `ssh r1 'sudo systemctl start stellar-stack-version-probe.service'`, then re-check the textfile |

## Remediation — the coordinated-upgrade checklist

**Do all applicable components in ONE maintenance window.** A
protocol upgrade is a coordinated cutover across the whole ingest
path — upgrading only `stellar-core` and leaving `galexie` behind (or
vice versa) just moves the incident from one component to the other,
which is exactly what happened across 2026-07-08 → 2026-07-09.

- [ ] **Confirm the target protocol version** and its network
      activation date (SDF announcements / stellar-protocol repo
      CAP status) before starting — this sets how much runway you
      have, not how fast you need to move today.
- [ ] **stellar-core** (apt):
      ```sh
      ssh r1
      sudo apt-get update
      apt-cache policy stellar-core   # confirm the candidate is the target version
      sudo systemctl stop galexie     # captive-core will restart cleanly once core is upgraded
      sudo apt-get install --only-upgrade stellar-core
      sudo systemctl start galexie
      ```
      Then codify: bump any `stellar-core` version reference in
      `configs/ansible/roles/archival-node/tasks/06-stellar-core.yml`
      comments / `VERSIONS.md` in the same PR (the apt package itself
      tracks the repo's candidate, so there's no version pin to bump
      in ansible for core the way there is for galexie — but
      VERSIONS.md's pinned-snapshots table is documentation of record
      and goes stale otherwise).
- [ ] **stellar-galexie** (build from tag — no apt package upstream,
      see `configs/ansible/roles/archival-node/tasks/07-galexie.yml`).
      Exact build recipe (matches what `07-galexie.yml`'s `go install`
      task does, spelled out for a manual/emergency upgrade or for
      verifying the ansible apply did the right thing):
      ```sh
      # On r1, or any build host with the same Go toolchain:
      git clone https://github.com/stellar/stellar-galexie.git
      cd stellar-galexie
      git checkout galexie-vNN.N.N        # the target tag
      CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
        go build -trimpath -o galexie .
      # Verify before installing:
      ./galexie version

      # Install with a rollback backup:
      sudo cp /usr/local/bin/galexie /usr/local/bin/galexie.prev
      sudo systemctl stop galexie
      sudo install -o root -g root -m 0755 galexie /usr/local/bin/galexie
      sudo systemctl start galexie
      sudo systemctl status galexie      # confirm it's running + past initial catchup
      /usr/local/bin/galexie version     # record the new pseudo-version string
      ```
      Then codify in the SAME PR (this is the step that was skipped
      on both 2026-07-08 and 2026-07-09, which is why this alert
      exists):
      - `configs/ansible/roles/archival-node/defaults/main.yml`:
        bump both `galexie_version` (the go-install tag) AND
        `galexie_expected_version_string` (the exact `galexie
        version` output stamped above — this is what
        `07-galexie.yml`'s drift-guard assert compares against, and
        what this probe's lag classification treats as "installed").
      - `VERSIONS.md`: update the `stellar/stellar-galexie` pinned-
        snapshot row (SHA + tag + date).
      - Run `ansible-playbook -i inventory/r1.yml
        playbooks/archival-node.yml --tags galexie --check --diff`
        to confirm the codified state now matches reality (zero
        diff expected).
- [ ] **stellar-archivist** (apt):
      ```sh
      ssh r1
      sudo apt-get update
      sudo apt-get install --only-upgrade stellar-archivist
      ```
      No running service to restart — `stellar-archivist` is invoked
      on-demand by `stellarindex-ops verify-archive -tier archivist`
      and by ad-hoc operator scans, not a daemon. Update the pinned
      SHA in `VERSIONS.md`'s `stellar/rs-stellar-archivist` row if a
      new commit/tag is available upstream.
- [ ] **go-stellar-sdk** (go.mod — separate code change, not part of
      this ops runbook). If the protocol upgrade changed any XDR
      shape our decoders touch (CAP-0071 is exactly this class of
      change), bump `github.com/stellar/go-stellar-sdk` in `go.mod`,
      run `go build ./...` + the full unit suite, and audit
      `internal/scval` + any decoder that hand-parses XDR structures
      the CAP changed. This is a normal PR through CI, not an
      r1-side operator step — land and deploy it BEFORE the
      operator-side upgrades above if the SDK bump is what makes the
      new XDR shapes parseable at all (check the CAP's XDR diff
      against what's currently vendored).
- [ ] **Verify the probe clears**: `sudo systemctl start
      stellar-stack-version-probe.service` on r1, then re-check the
      textfile — every upgraded component's `stellarindex_stellar_stack_version_lag`
      should read 0 (or the metric may be briefly absent for one run
      if GitHub rate-limits the immediate re-check; that's expected
      and self-heals on the next daily run).
- [ ] **Watch ingest** for the hour after cutover — a protocol
      upgrade is exactly the class of change most likely to surface
      a new decode-error class; check
      `stellarindex_source_decode_errors_total` and the completeness
      verdict (`docs/operations/runbooks/completeness-incomplete.md`)
      aren't regressing.

## Why the galexie comparison is date-based, not version-based

`/usr/local/bin/galexie version` always reports a Go pseudo-version
(`v0.0.0-<14-digit-date>-<short-sha>`), never the release tag —
galexie's `galexie-vX.Y.Z` tags aren't a Go-modules-recognised semver
tag for this module path, so `go install
github.com/stellar/stellar-galexie@galexie-vX.Y.Z` always resolves to
that tag's commit and reports it as a pseudo-version (this is also
why `07-galexie.yml`'s idempotency check compares against a separate
`.galexie.tag` stamp file rather than the binary's own version
output). The probe therefore compares the pseudo-version's embedded
commit DATE against the GitHub release's target-commit date, and
infers a protocol-major gap (lag=2 vs lag=1) by checking whether the
release immediately before the latest one shares its major version
number — see the inline comments in
`stellar-stack-version-probe.sh` (installed via
`configs/ansible/roles/archival-node/tasks/10-observability.yml`) for
the exact logic. This is a deliberate design choice: reading the
`.galexie.tag` stamp file instead would have made the probe blind to
exactly the failure mode it exists to catch (a hand-installed binary
swap that bypassed ansible never updates that stamp).

## Known false-positive patterns

- **`stellarindex_stellar_stack_probe_success == 0` with stale lag
  values**: the GitHub API is unauthenticated in this probe (no
  token) and subject to a low unauthenticated rate limit (60
  requests/hour per source IP) — up to 3 calls per run for galexie.
  A shared egress IP hitting other GitHub API consumers around the
  same time can transiently exhaust it. Self-heals on the next daily
  run; only escalate if `probe_success` stays 0 across multiple
  consecutive days.
- **`core` or `archivist` lag metric absent entirely**: `apt-cache
  policy` found no Candidate for a dpkg-installed package. This
  usually means the `apt.stellar.org` repo isn't in
  `/etc/apt/sources.list.d/` on this host — check whether
  `06-stellar-core.yml` actually ran here (it's gated behind
  `run_stellar_core`, which is `false` on r1; the `stellar-core`
  package r1 actually runs as galexie's captive-core dependency may
  therefore not be tracked by the SAME apt source the ansible role
  expects — this is a known gap, not a probe bug; see
  `docs/operations/r1-ansible-drift-2026-07-03.md`).
- **`galexie` lag flips 1→2 or 2→1 across consecutive days without an
  actual upgrade**: the "is the latest release a new major relative
  to the one before it" heuristic looks at the two MOST RECENT
  releases upstream. If upstream ships an unusual release cadence
  (two point releases back-to-back, then a major), the classification
  can shift as the "previous release" reference point moves. Treat
  the underlying DATE gap (visible via
  `stellarindex_stellar_stack_installed_info`'s version string,
  cross-referenced against the GitHub releases page) as ground truth
  over the derived 1-vs-2 classification if they ever seem to
  disagree.

## Related

- [galexie-catchup-refused](galexie-catchup-refused.md) — the
  2026-07-05 captive-core wedge; a different failure mode in the same
  subsystem (galexie/captive-core health, not version currency).
- `configs/ansible/roles/archival-node/tasks/07-galexie.yml` — the
  galexie install/build task, including the `galexie_expected_version_string`
  drift-guard assert this probe's design mirrors.
- `configs/ansible/roles/archival-node/defaults/main.yml` —
  `galexie_version`, `galexie_expected_version_string`,
  `galexie_expected_binary_sha256` (optional).
- `VERSIONS.md` — the pinned-snapshots table of record for every
  upstream Stellar binary/repo this project depends on.
- [docs/operations/r1-ansible-drift-2026-07-03.md](../r1-ansible-drift-2026-07-03.md)
  — background on r1 configuration drift generally.

## Changelog

- 2026-07-09 — initial draft alongside the stellar-stack-version-probe,
  in direct response to the P27 core-freeze (2026-07-08) and galexie
  CAP-0071 crash-loop (2026-07-09) incidents.
