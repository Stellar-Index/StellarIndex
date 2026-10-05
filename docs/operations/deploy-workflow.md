---
title: Deploy workflow — pushing a tagged release to a region
last_verified: 2026-10-05
status: living doc
---

# Deploy workflow

How a tagged release lands on a host. The release half (tag →
`release.yml` → GitHub Release; no GHCR push, `docker/<binary>.Dockerfile`
is for self-host builds) is [`release-process.md`](release-process.md) §Cut.

```
operator → deploy.yml workflow_dispatch (region + version + binaries)
         → download from GitHub Release → cosign signature + SHA256SUMS
         → Ansible over SSH: migrate up → per binary: backup to
           /usr/local/bin/<b>.prev-<tag> → install → restart → health probe
           → automatic rollback on health-probe failure
```

There is no auto-deploy on tag push.

## Triggering a deploy

```sh
gh workflow run deploy.yml \
  -f region=r1 \
  -f version=v0.2.0 \
  -f config_acknowledged=true
```

`version` must match `vX.Y.Z[-prerelease][+build]` and the GitHub Release
must exist.

`config_acknowledged` is required in practice: the **Config-apply gate**
fails the job when any config surface (ansible, Prometheus rules, systemd
units, DB schema) changed since the previous release tag and the flag is not
`true`, because a binary deploy applies none of them. Set it only once that
config is applied or about to be: [`deploy-config-apply.md`](deploy-config-apply.md).
A config-free release passes either way.

Default `binaries` (`binaries.default` in `.github/workflows/deploy.yml`) is
six: `stellarindex-indexer`, `stellarindex-aggregator`, `stellarindex-api`,
`stellarindex-sla-probe`, `stellarindex-ops`, `stellarindex-migrate`. `ops`
and `sla-probe` are in it deliberately — omitting `ops` once left it two
releases behind on r1 while thirteen units exec'd it.

## The region binary manifest

Regions run different unit sets.
[`scripts/dev/region-binaries.tsv`](../../scripts/dev/region-binaries.tsv) is
the single manifest; the workflow filters `binaries` through it, so the
default dispatches everywhere.

**Resolve the region's binary set** (offline, before staging):
- a binary the row marks not deployable (futurenet's
  `stellarindex-aggregator:unit-not-found`) is **skipped** loudly;
- a binary the row has no opinion about is **refused** — its unit was never
  checked, and `deploy-one-binary.yml` requires `systemctl is-active` after
  restart, so a missing unit is a failed deploy and a rollback.

Every later step and the job summary use the effective set.

**Reconcile the binary set against the host** (after SSH is proven, before
the playbook):
- *Is the manifest still true?* Every deployable daemon must have a unit
  systemd can load, every undeployable one must still have none. Either
  mismatch refuses and names `scripts/dev/preflight-deploy.sh --refresh-manifest`.
- *Would this dispatch leave skew?* A partial set is fine (re-running with
  only the stale binary is the recovery path) unless an omitted binary is not
  already on the deploying version, per the `deployed-versions` sidecars.
  Otherwise it trips `stellarindex_binary_version_skew` after the deploy.

Deployability is **unit presence (`LoadState`), not enablement**: testnet's
aggregator is `UnitFileState=disabled`, `ActiveState=active` and is deployed;
futurenet's is `LoadState=not-found`. CLI binaries (`ops`, `migrate`,
`sla-probe`) have no unit and are exempt.

Both refusals are pinned by `scripts/ci/config-apply-gate-test.sh`.

## Per-region setup

Regions wired: `r1`, `testnet`, `futurenet`. Secrets per region:

| Secret | What it is |
| --- | --- |
| `<REGION>_HOST` | Deploy target (r1: `136.243.90.96`; test nets: the VM's private NAT IP `192.168.122.10`/`.20`) |
| `<REGION>_USER` | SSH user, default `root` |
| `<REGION>_SSH_JUMP` | Test nets only: KVM host as `user@public-ip` to ProxyJump through |
| `<REGION>_SSH_KNOWN_HOSTS` | `ssh-keyscan -t ed25519 <host> \| base64` (test nets: both hops) |
| `DEPLOY_SSH_PRIVATE_KEY` | Shared. `ssh-keygen -t ed25519 -f deploy-key`; secret = private half, public half in the host's `authorized_keys` |

The GitHub Environment named after the region must exist with a main-only
branch policy; **Assert the production approval gate is configured** refuses
to deploy otherwise (reviewer half is waivable pre-production; see the step's
comment).

Adding `r2` / `r3`:
1. Add the `R2_*` / `R3_*` secrets.
2. Add the region to the `region` choice list in `.github/workflows/deploy.yml`.
3. Extend the `case` in **Resolve region inventory** (and the known-hosts
   `case` in **Set up SSH**).
4. Create the region's Environment and add its row to `region-binaries.tsv`.

## What the playbook does

`pre_tasks` sync the tag's `migrations/` tree and run
`stellarindex-migrate … up` before any binary is touched (see
[§Migrations](#migrations-run-before-binaries-and-are-not-rolled-back)).
[`deploy-binary.yml`](../../configs/ansible/playbooks/deploy-binary.yml) then
includes [`deploy-one-binary.yml`](../../configs/ansible/tasks/deploy-one-binary.yml)
per binary:

1. Resolve previous version from `/var/lib/stellarindex/deployed-versions/<binary>` (first deploy: UTC timestamp).
2. Stage as `<install_dir>/<binary>.new`.
3. Back up `<binary>` → `<binary>.prev-<previous-tag>`.
4. Atomic rename `.new` → live.
5. Write the sidecar.
6. `systemctl restart <binary>.service` (CLI binaries: `<binary> -version` smoke instead).
7. Wait `health_grace_seconds` (default 15s).
8. Health probe:
   - `stellarindex-api`: `curl http://127.0.0.1:3000/v1/readyz` → 200 (5 retries × 3s).
   - indexer / aggregator: `curl http://127.0.0.1:<port>/readyz` on the metrics
     listener (9464 / 9465, `daemon_ready_ports`) → 200 (20 retries × 3s);
     503 when the applied schema is behind the binary. A daemon with no
     `daemon_ready_ports` entry fails its probe.
   - Stability window: hold one more grace period, then require an unchanged
     `NRestarts` and a still-passing probe (catches a post-probe crash loop).
9. Rollback on failure: stop the service, move the bad binary to
   `<binary>.failed-<new-version>`, restore `.prev-<previous-tag>` and the
   previous sidecar, restart, fail the play.
10. Prune backups beyond the latest 5.

## Backup naming + rollback

Backups are `/usr/local/bin/<binary>.prev-<tag>`, `<tag>` from the sidecar
(e.g. `stellarindex-api.prev-v0.2.0`, `.prev-v0.1.3`, …).

Preferred rollback: re-dispatch the previous tag (release-process.md
§Rollback). Manual:

```sh
ssh root@<host> "
  systemctl stop stellarindex-api
  mv /usr/local/bin/stellarindex-api /tmp/bad-stellarindex-api
  cp /usr/local/bin/stellarindex-api.prev-v0.1.3 /usr/local/bin/stellarindex-api
  echo v0.1.3 > /var/lib/stellarindex/deployed-versions/stellarindex-api
  systemctl start stellarindex-api
"
```

Then `gh workflow run deploy.yml -f version=v0.1.3 …` to resync the
workflow's state (a no-op if the sidecar says v0.1.3 and the binary is
healthy).

## Migrations run before binaries, and are not rolled back

`migrate up` runs in `pre_tasks` so a binary built for a newer schema never
starts on an older one; it is idempotent.

Rollback restores only the **binary** and never runs `migrate down`, so after
a rollback the old binary runs on the new schema. This is intentional:
down-migrations can destroy data, and
[`migrations/README.md`](../../migrations/README.md) rule 9 requires every
up-migration to be **additive and old-binary-safe**. `down.sql` is for
local/dev only.

**If a migration fails mid-deploy**, the play stops at `Apply outstanding
migrations`, no binary is installed, and earlier migrations in the run stay
applied (each commits in its own transaction). Then:

1. `stellarindex-migrate -migrations /usr/local/share/stellarindex/migrations status` on the host.
2. Read the failing `.up.sql` against the Ansible output's Postgres error.
3. Fix forward with a **new** migration; never edit one that may have run (rule 1).
4. Re-deploy the new tag; `migrate up` resumes.

## Failure modes

| Symptom | Cause | Fix |
| --- | --- | --- |
| Fails at "Validate inputs" | `version` not SemVer | Re-run with a valid `vX.Y.Z` |
| "Region <X> host secret is unset" | `<REGION>_HOST` missing | Add it (§Per-region setup) |
| "Bad binary preserved at …failed-vX.Y.Z" | Health probe failed; rolled back | Inspect `/usr/local/bin/<binary>.failed-<v>`; `journalctl -u <binary> -n 200` |
| SSH timeout / "permission denied" | Stale key, removed `authorized_keys` entry, firewall | Check `DEPLOY_SSH_PRIVATE_KEY`; SSH manually from a known-good box |
| Fails at "Apply outstanding migrations" | Migration error before any binary moved | §Migrations above |
| "binary version skew on r1 after deploy" | **Served-path smoke** runs `stellarindex-binary-version-probe.service` and asserts `stellarindex_binary_version_skew == 0` / `probe_success == 1`. A missing unit means observability config is not applied (config-apply class) | Re-run naming the stale binary. [binary-version-skew](runbooks/binary-version-skew.md) |
| "Migration follow-up gate" fails | A migration header declares `-- REQUIRED-FOLLOWUP: <command>` and `followups_acknowledged` is not `true`. Nothing deployed | Re-run with `-f followups_acknowledged=true` when you can run the commands right after, then run them ([`migrations/README.md`](../../migrations/README.md) rule 12) |
| "Config-apply gate" fails | Config/schema surface changed and `config_acknowledged` not `true`. Binaries already live | Apply per [deploy-config-apply.md](deploy-config-apply.md), re-run with `-f config_acknowledged=true` |
| "test net(s) behind vX.Y.Z" warning after r1 | **Fleet release parity** compares `/v1/version` on `api.testnet.stellarindex.io` / `api.futurenet.stellarindex.io` with the tag. Expected (r1 goes first); [`fleet-release-drift.yml`](../../.github/workflows/fleet-release-drift.yml) opens an issue after a 24h grace | Run the two dispatches the step prints. [testnet-futurenet-deployment.md](testnet-futurenet-deployment.md#keeping-the-test-nets-in-step-with-r1) |

## See also

- [local-preflight.md](local-preflight.md) — answer these questions before dispatch (`make preflight-deploy REGION=… VERSION=…`)
- [semver-policy.md](../architecture/semver-policy.md) — tag rules
- [`deploy.yml`](../../.github/workflows/deploy.yml), [`release.yml`](../../.github/workflows/release.yml)
