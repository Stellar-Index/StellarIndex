---
title: Testnet + Futurenet deployment
last_verified: 2026-08-27
status: current
---

# Testnet + Futurenet deployment

How to stand up StellarIndex Testnet and Futurenet instances. Companion:
[testnet-futurenet-reset-runbook.md](./testnet-futurenet-reset-runbook.md).

## Topology decision

- **One Hetzner box, two libvirt/KVM VMs** — one VM per network. Testnet +
  Futurenet share the box; **Mainnet is the only tier with HA / multiple
  regions** (r1 + future r2/r3).
- **Box:** Hetzner Server Auction **Xeon E-2176G** (6c/12t, 64 GB DDR4
  **ECC**, 2×960 GB **U.2 NVMe Datacenter**), Frankfurt, ~€83/mo, €0 setup
  (AuctionID 3059162-class). IP 95.217.126.13.
- **Storage:** the two NVMe are **RAID0-striped** (mdadm RAID0 via installimage, LVM `vg0` ~1.74 TB) —
  test-net data is disposable/re-ingestable, so no mirror; a drive failure
  just means re-ingest from genesis. The **host** owns ZFS (stripe + zstd);
  each **VM** gets a plain virtual disk (no ZFS-on-ZFS).
- **Why 1-box-2-VMs, not co-tenant bare metal:** the stack isn't
  instance-templated (ports, DB names, galexie buckets, ~40 systemd units
  would all collide), so two VMs reuse the single-network ansible unchanged.

## What makes the code network-correct

A `stellar.network = testnet|futurenet` switch drives everything, because
these were network-parameterized (commit `6b3859d7`, audit 2026-08-26):

| Concern | Mechanism |
| --- | --- |
| Passphrase | `cfg.Stellar.Passphrase()`; ansible `stellar_passphrase` feeds core.cfg + galexie |
| SAC contract addresses | `canonical.InstallNetworkPassphrase` at API start-up |
| Movements feed floor | `soroban_genesis_ledger` / `movements_floor_ledger` = **1** on test nets (else the feed floors above the whole chain) |
| cap67 follow daemon | `-floor-ledger` = `stellar_movements_floor_ledger` |
| History archive | `stellar_history_archive_url` = core-testnet / core-futurenet; a pubnet (core-live) URL on a test net is **rejected** by config validation |
| Cross-anchor archive fill | refuses to write pubnet ledgers into a test-net archive |
| Sources | **SDEX only** (`stellarindex_enabled_sources: [sdex]`): the native Stellar DEX exists on every network, so the explorer shows real on-chain trades. Bespoke Soroban DEX/AMM (soroswap/aquarius/phoenix/blend/comet), oracles (reflector/redstone/band), external CEX/FX markets, and the aggregator/pricing are all **cut** (`run_aggregator: false`) — every asset is $0 off pubnet |
| Start ledger | **coherent recent start** — `galexie_start_ledger` **==** `stellarindex_backfill_from_ledger`. galexie's captive core catches up to that ledger via the archive (fast) and the indexer's live-only floor matches it, so there is no leading gap. NOT genesis: testnet is ~4.3M ledgers (a genesis replay is hours). **Update both together after a reset** (ledgers restart at 1) |

## Deploy set

`galexie + stellarindex-indexer + stellarindex-api + stellarindex-ops`.
**No aggregator** (no real USD markets off pubnet). `stellarindex-ops` is
**required** — it runs the `cap67-movements` follow daemon that writes the
`/v1/accounts/{g}/movements` feed (not the aggregator).

## Steps 1–2 — Host + VMs (libvirt/KVM)

Provisioned reproducibly per
[../../configs/libvirt/README.md](../../configs/libvirt/README.md): Debian 12 via
installimage (`configs/libvirt/installimage-host.conf` — mdadm RAID0 + mirrored
`/boot` + LVM `vg0`), then libvirt/KVM + two Ubuntu 24.04 VMs (`si-testnet`
192.168.122.10, `si-futurenet` 192.168.122.20) created by
`configs/libvirt/provision-vms.sh` with cloud-init (static IPs, both SSH keys).
Each VM: 4 vCPU, 20 GB RAM, a 600 GB LV; on libvirt's private NAT, reached via
ProxyJump through the host (encoded in the inventory). Ubuntu 24.04 matches r1
so the ansible role deploys unchanged. **Not Proxmox** — plain libvirt/KVM is
lighter and keeps the host CLI/ansible-managed.

## Step 3 — Inventory

```sh
cp configs/ansible/inventory/testnet.example.yml  configs/ansible/inventory/testnet.yml
cp configs/ansible/inventory/futurenet.example.yml configs/ansible/inventory/futurenet.yml
```

Fill the `X.X.X.X` VM IPs, `allowed_ssh_cidrs`, and create the vault
secrets (`testnet.secrets.yml` / `futurenet.secrets.yml`) with DB / MinIO /
CH-serving passwords (same shape as `r1.secrets.yml`).

**Testnet is Phase 1** (fully working). **Futurenet is Phase 2** — galexie
has no built-in futurenet preset, so its captive core needs an explicit
passphrase + history archives; provision + test that on the VM before
first run (see the futurenet inventory's flagged TODOs).

## Step 4 — Deploy + bring-up

Run the archival-node role against the inventory (via `deploy.yml` or
directly). The role is r1-safe: ClickHouse (`run_clickhouse`, default **false**)
and Redis installs, the ClickHouse schema apply, the galexie drift-guard
skips, and the from-source binary build are all opt-in flags set only in the
test-net inventory — r1 is never touched. Order per VM:

1. galexie captive-core catches up to `galexie_start_ledger` via the network
   archive, then exports `galexie_start_ledger` → tip → live into `galexie-live`.
   (Fresh/empty bucket only; on restart it **resumes** from the last exported
   ledger. `GALEXIE_START` is honored only on a fresh bucket — if you change it,
   wipe `galexie-live` first, or the resume wins.)

   > ⚠ **Keep `galexie_start_ledger` CLOSE to the live tip** (a few hundred
   > ledgers). captive-core replays start→tip as a *buffered* catchup only when
   > that gap is small; a large gap (thousands of ledgers) makes it do an
   > **online-catchup that SKIPS to live**, leaving galexie parked — it tracks
   > consensus but never exports, cycling "Waiting for trigger ledger". This
   > also bites when a **full role re-deploy restarts galexie repeatedly**: each
   > restart resumes from the last exported ledger, and if the tip has run far
   > ahead meanwhile, galexie strands. **Recovery** = stop galexie+indexer, wipe
   > `galexie-live`, `truncate ingestion_cursors` (+ `account_observer_watermark`),
   > set `galexie_start_ledger`/`backfill_from_ledger` a few hundred below the
   > CURRENT tip, then re-render (`--tags galexie,stellarindex`) so it starts
   > fresh. Binary-only deploys (deploy.yml) don't touch galexie and are safe.
2. indexer reads `galexie-live` live-only from `stellarindex_backfill_from_ledger`
   (== `galexie_start_ledger`) → ClickHouse + PG. Start it once galexie has
   exported the first object at/after that ledger, so its SDK backend does not
   error against an empty bucket.
3. `cap67-movements` follow daemon derives `account_movements` (`-floor-ledger`
   = `stellar_movements_floor_ledger`); the data begins at the recent start.
4. api serves `/v1/*`, bound to `stellarindex_api_listen_addr` (0.0.0.0:3000 on
   the VMs so the host's Caddy can reach it over the NAT).

## Step 5 — Host reverse proxy + DNS/TLS

The VMs have no public IP; the **host** runs one Caddy that fronts all
subdomains. DNS split (deliberate):

| Record | Cloudflare | Why |
| --- | --- | --- |
| `api.testnet.stellarindex.io` → A 95.217.126.13 | **DNS-only (grey)** | API serves SSE (live ledger/movements); proxying buffers and breaks it. Grey also lets Caddy solve ACME HTTP-01 → real LE cert here |
| `api.futurenet.stellarindex.io` → A | DNS-only (grey) | same (Phase 2) |
| `testnet.stellarindex.io` | **proxied (orange)** | serves the static Next.js explorer (cacheable/DDoS-protected); its live data comes from the grey `api.*` origin |
| `futurenet.stellarindex.io` | proxied (orange) | same (Phase 2) |

Install + deploy on the host:

```sh
rsync -a configs/libvirt/ root@95.217.126.13:/root/libvirt/
ssh root@95.217.126.13 'cd /root/libvirt && ./setup-host-proxy.sh'
```

`setup-host-proxy.sh` installs Caddy from its official apt repo and deploys
`configs/libvirt/host-Caddyfile` (LE for the grey `api.*`, `flush_interval -1`
for SSE). The explorer (orange) origins are wired when the explorer is
deployed, with a Cloudflare Origin-CA cert (orange domains can't win HTTP-01).

## Step 6 — Verify

- [ ] Ingest advancing: `/v1/ledger/tip` `latest_ledger` climbing, `stale:false`,
      `lag_seconds` low. galexie earliest exported == `backfill_from_ledger`.
- [ ] External: `curl https://api.testnet.stellarindex.io/v1/ledger/tip` returns
      200 with live data (proves DNS → host Caddy → LE cert → VM API over NAT).
- [ ] `/v1/assets/{id}` shows a **testnet** SAC contract address (not pubnet).
- [ ] No aggregator noise in logs (aggregator not deployed); `enabled_sources`
      is `[sdex]` only.

## Step 7 — Failed units on a fresh test net (triage, 2026-08-27)

A newly-provisioned test net accumulates `failed` units that are NOT all
defects. Triage each before "fixing" it — `systemctl list-units
--state=failed`:

| Unit | Cause | Disposition |
| ---- | ----- | ----------- |
| `galexie-archive-fill` | **Real bug (fixed).** Mirrors from AWS Public Blockchain Data with the `.../ledgers/pubnet/` path hardcoded in 5 places. AWS publishes no testnet/futurenet dataset. | **Omit** — gated on `stellar_network == 'pubnet'`, with a removal block for nets provisioned before the gate. |
| `config-assertions` | **Real bug (fixed).** Asserted ZFS integrity + a mainnet-shaped compression want-list on a no-ZFS lean VM → 4 FAILs forever. | **Fixed in the script** — `have_zfs()` / `is_pubnet()` gates + an explicit `_skipped` series. r1 semantics unchanged. |
| `verify-archive-tier-a` | **Transient startup ordering.** The 03:23 timer fired before the `galexie-archive` bucket was populated; MinIO answers a non-admin with `AccessDenied` for a bucket that isn't there, tip resolution fails, and the job falls back to a `[2,0]` serial walk and errors. | **No fix needed** — passes once the archive exists (verified: 12 parallel chunks, "chain-link integrity OK ✓", state advanced to ledger 313670). The failed state simply persists until something re-runs; `systemctl start verify-archive-tier-a` clears it. |
| `verify-archive-tier-b` | **Real bug (fixed).** Tier B's checkpoint anchor reads a LOCAL archivist mirror (`VERIFY_ARCHIVE_ROOT`, `/srv/history-archive`) that is only ever populated for pubnet. On a test net that path holds a partial PUBNET stub, so Tier B anchors TESTNET ledgers against PUBNET header hashes and mismatches at ledger 63 — every run, forever. | **Omit** — `verify_archive_tier_b_enabled` defaults to `stellar_network == 'pubnet'`, with a removal block for nets provisioned earlier. To enable later, build a network-correct mirror with stellar-archivist from `stellar_history_archive_url_set`, then set the flag true. |
| `archive-completeness` | **Capacity mismatch.** `TimeoutStartUSec=30min`, exceeded by the scan → SIGTERM. Note this will NOT self-resolve when the backfill finishes: `compute-archive-to.sh` sets `ARCHIVE_TO` from the ingestion cursor, which is already at the network tip (4,354,688 on testnet), so the scan already covers the full range today. | **Open** — needs a longer timeout on the lean VMs, or a windowed rather than full scan. |
| `pgbackrest-backup` | **Real bug (fixed).** Exit **127** nightly — the timer is active but this role installs pgBackRest nowhere (no apt task exists); it is provisioned out-of-band on the pubnet hosts. A backup timer that cannot run also reads as "backups are configured" on a glance at the unit list. | **Omit** — `pgbackrest_backup_enabled` defaults to `stellar_network == 'pubnet'` (set explicitly in all four test-net inventories, real **and** `.example`), with a removal block. The offsite-backup refusal assert is now gated on the same flag — it previously ran unconditionally and would hard-fail the play for a host that takes no backups at all. |
| `stellar-core` | Expected: the galexie path runs with `run_stellar_core: false`. | **Residue** — disabled, not timer-driven. `reset-failed`. |
| `stellarindex-aggregator` | Exit **203** (exec failure) — deliberately not deployed on the lean SDEX-only nets. | **Residue** — disabled, not timer-driven. `reset-failed`. |

Two traps worth internalising:

- **A `disabled` unit in `failed` state is residue, not an ongoing failure.**
  Check `systemctl list-timers --all` before treating it as live: only
  timer-driven units recur.
- **Never reproduce a unit by rebuilding its environment on the command
  line.** `sudo -u stellarindex env $(grep ... | xargs) <cmd>` puts every
  secret in the env file into argv, and sudo logs the whole line to the
  journal (promtail then ships it to Loki). Reproduce it as the unit —
  `systemctl start <unit>`, or
  `sudo -u <user> bash -c 'set -a; . /etc/default/<file>; set +a; exec <cmd>'`.
  Running it *without* the unit's identity is also how you get a false
  diagnosis: a credential-less run returns `AccessDenied` and looks like a
  MinIO policy gap when the policy is in fact correct.

## Keeping the test nets in step with r1

The test nets take releases AFTER r1 and only by a separate `deploy.yml`
dispatch per region, so they drift by default: on 2026-09-03 they were a
release and three migrations behind (`v0.57.0` / migrations head 0150 against
r1's `v0.58.0` / 0153) one day after a catch-up, and nothing in the system
could say so — r1's Prometheus scrapes r1 only and the VMs are NAT-only, so no
fleet-wide series exists for an alert to compare. Two automatic checks now
close that:

| Check | When | What it does |
| --- | --- | --- |
| `deploy.yml` → **Fleet release parity** step | after every deploy | After an r1 deploy, reads `/v1/version` on both test nets and prints them against the tag just deployed with the two catch-up dispatches; after a test-net deploy, confirms that region is now in step with live r1. A `::warning::`, never a failure — r1 goes first by design. |
| [`fleet-release-drift.yml`](../../.github/workflows/fleet-release-drift.yml) | daily 07:25 UTC | Same comparison against live r1 with a **24h grace** measured, per test net, from the **oldest r1 release it lacks** — never from r1's newest tag, which would re-open the window on every release (on 2026-09-03 r1 cut `v0.58.0` at 01:26Z and `v0.59.1` at 07:30Z; a test net still on `v0.57.0` at the next morning's run had lacked `v0.58.0` for 29h, whatever the newest tag's age). Past the grace it opens/updates one issue (`Fleet release drift: a test net is behind r1`, labelled `fleet-release-drift`) carrying the dispatches, fails the run so the scheduled-failure mail reaches the maintainer, and closes the issue only once every test net serves r1's release — a test net inside the grace keeps it open. An unreadable host is reported as `DEGRADED`, never as in step. |

Both run [`scripts/ci/check-fleet-release-drift.sh`](../../scripts/ci/check-fleet-release-drift.sh)
(fixture-tested in CI; the v0.57.0-vs-v0.59.1 drift and the 2026-09-03 release
cascade are pinned red on the real tag dates).

**What is read, and what it proves.** `GET /v1/version` on the **`api.*`**
names — the served binary's `git describe`. That handler is static: it answers
whether or not the binary is ready, so it proves which release a host **runs**
and nothing more. It is not `/v1/healthz` (static `ok`, what Caddy
health-checks) and not `/v1/readyz` (the readiness probe, which answers 503
when its `schema` check fails). An HTTP error (the KVM host's Caddy answers 502
while a VM is down — during a reset, for one), a redirect, or a body without
`.data.version` is *unreadable* and the verdict is `DEGRADED`. The bare
`testnet.stellarindex.io` / `futurenet.stellarindex.io` names 301 to mainnet's
API and would read as "in step" forever; the script never follows a redirect.

**What it does not prove.** The version is what the host *says* it runs, and
nothing on the public surface can check it: a host serving a version higher
than its binary reads as *ahead*, which is not drift, so the verdict can be
`in-step` and the tracking issue closes. This is a tripwire against accidental
drift — a region nobody redeployed — not a control against a compromised or
lying host. Reading each host costs at most three attempts (`--retry 2`) five
seconds apart, each capped at 20s, so one wedged region costs about 70s against
the job's 10-minute budget.

**Migrations are derived from the tags, not read from the hosts.** A binary's
`ExpectedSchemaVersion` is its tag's `migrations/` head, and on the default
deploy path `deploy-binary.yml` applies the tag's migrations before the binary
swap — so a host serving `vX.Y.Z` has that tag's migrations, "N releases
behind" carries "M migrations behind" with it, and `git ls-tree <tag>
migrations/` answers M with no host access. The one deploy the derivation
cannot see is `migrations_skip=true` (a `deploy.yml` input for hot-fixes): the
binary then serves its tag's version over an older schema. Such a host answers
503 on `/v1/readyz` with the `schema` check failing, but `/v1/healthz` stays
200 and `/v1/version` still answers, so the drift check reports it as in step.
Observing the schema through `/v1/readyz` per host would close that gap and is
not built.

**Version strings, pre-releases and dirty builds.** A released tag and every
`git describe` label on it (`v0.59.1-3-gabc1234`, `v0.59.1-dirty`, what a local
`make` build serves) sit on the tag's base and compare equal to it. Any other
suffix (`v0.60.0-rc.1`) is a pre-release and sorts below the release it
precedes. A pre-release **reference is fail-closed**: while r1 serves
`v0.60.0-rc.1` no released tag dates what the test nets lack (`v0.60.0` is not
cut), so no grace can be measured — every test net behind it is `DRIFT` at the
next daily run, with no grace, the message naming the missing base tag and the
release/migration counts printed as `?`. r1 on a pre-release is itself an
exceptional state; the same-day issue says so rather than guessing a window.
It closes once r1 serves a release again and the test nets carry it. Under a
pre-release reference only an **identical** version string counts as in step:
every pre-release of a base compares as one version and no ordering over `-rc.N`
suffixes is attempted, so r1 on `v0.60.0-rc.2` beside a test net on
`v0.60.0-rc.1` is drift, not a quiet run that closes the ticket on a test net a
candidate behind. A test net that already carries the base — the cut release, or
a newer one — is drift under that reference too, reported as *not on* r1's
version rather than as lacking a base it has.

**Host-supplied bytes.** Nothing a host serves reaches the report, a step
output or the issue unsanitised: a served version that is not a version is
rendered in the version alphabet (`[0-9A-Za-z.-]`, every other byte shown as
`?`), free text (a curl error, an error page) is flattened to one line of
printable ASCII without backticks, both capped at 120 bytes, and a *conforming*
version is bounded by the pattern itself at 77 bytes (three digits per
component — the width the version key can hold — and 64 bytes of suffix against
the ~54 of the longest `git describe` label), so the caps bind every byte a host
serves and not only the malformed ones. The workflow terminates its multi-line
step output with a random delimiter. A host answering
a "version" that carries newlines and a `verdict=in-step` line therefore cannot
end the workflow's output value early and silence a `DEGRADED` run; the
self-test pins that fixture.

Catch up = the dispatches the report prints, one per region, test-net binary
set (no aggregator):

```sh
gh workflow run deploy.yml -f region=testnet   -f version=vX.Y.Z -f binaries=stellarindex-indexer,stellarindex-api,stellarindex-ops
gh workflow run deploy.yml -f region=futurenet -f version=vX.Y.Z -f binaries=stellarindex-indexer,stellarindex-api,stellarindex-ops
```

Verify: `curl -sf https://api.testnet.stellarindex.io/v1/version | jq -r .data.version`
equals r1's; the next scheduled run closes the issue on its own.

## Related

- [testnet-futurenet-reset-runbook.md](./testnet-futurenet-reset-runbook.md) — reset handling.
- `configs/ansible/inventory/testnet.example.yml`, `futurenet.example.yml`.
- `internal/config/config.go` `StellarConfig` — the network knobs.
- Protocol-upgrade pipeline: Futurenet → Testnet → Mainnet (futurenet is
  the early-warning network for new protocol op/event types).
