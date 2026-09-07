---
title: Deploy workflow — pushing a tagged release to a region
last_verified: 2026-09-07
status: living doc
---

# Deploy workflow

How a tagged binary release lands on a host. End-to-end pipeline:

```
git tag vX.Y.Z          → release.yml fires automatically
                        → cross-compiles binaries, pushes to GitHub Release
                        # No GHCR push — F-1221 (codex audit-2026-05-12);
                        # docker/<binary>.Dockerfile remains for self-host builds.

operator triggers       → deploy.yml workflow_dispatch (region + version + binaries)
                        → downloads binaries from GitHub Release
                        → SHA256SUMS verification
                        → Ansible playbook over SSH
                        → backup (prev binary → /usr/local/bin/<b>.prev-<tag>)
                        → install → restart → health probe → rollback on fail
```

This doc covers the **deploy** half. The **release** half is in
[`release-process.md`](release-process.md) §Cut.

## Triggering a deploy

```sh
gh workflow run deploy.yml \
  -f region=r1 \
  -f version=v0.2.0 \
  -f config_acknowledged=true
```

Or use the GitHub Actions UI: Actions → deploy → Run workflow,
fill in the dropdowns.

`config_acknowledged` is **not optional in practice.** The
**Config-apply gate** runs after the binaries land and FAILS the job
when any config surface (ansible, Prometheus rules, systemd units, DB
schema) changed since the previous release tag and the flag is not
`true` — a binary deploy applies none of those, so the feature they
gate would ship dead and silent (the 2026-08-25 declared-peg and
`rules.d` incidents). Set it only once you have applied, or are about
to apply, that config: see
[`deploy-config-apply.md`](deploy-config-apply.md). A config-free
release passes the gate either way.

Defaults if `binaries` is omitted — **six**, not three
(`.github/workflows/deploy.yml`, `binaries.default`):
`stellarindex-indexer`, `stellarindex-aggregator`, `stellarindex-api`,
`stellarindex-sla-probe`, `stellarindex-ops`, `stellarindex-migrate`.
`stellarindex-ops` and `stellarindex-sla-probe` are in the default set
deliberately: omitting them let `ops` sit two releases behind on r1
while thirteen units — the data-integrity gates and served-tier
writers — exec'd it, with every deploy reporting success.

The workflow refuses to run unless `version` matches
`vX.Y.Z[-prerelease][+build]` and the GitHub Release exists.

## The region binary manifest

The three regions run different unit sets, and until 2026-09-07 nothing
declared that to the workflow — it carried the one default list above for
all three. Both of that day's binary-set failures follow from it:

- the six-binary default dispatched at **futurenet**, which has no
  `stellarindex-aggregator` unit. `deploy-one-binary.yml` restarts
  `<binary>.service` and then requires `systemctl is-active`, so a unit
  systemd does not have is a failed deploy by construction. The health
  probe failed, the binary rolled back, and the residue is still on that
  host as `/usr/local/bin/stellarindex-aggregator.failed-v0.62.0`;
- a **four-of-six** set at r1, which left `stellarindex-migrate` and
  `stellarindex-sla-probe` two releases behind and tripped
  `stellarindex_binary_version_skew` *after* everything was live.

[`scripts/dev/region-binaries.tsv`](../../scripts/dev/region-binaries.tsv)
is the manifest and the workflow reads it — there is no second copy. Two
steps use it:

**Resolve the region's binary set** runs before anything is staged, offline.
A requested binary the region's row records as not deployable (futurenet's
aggregator, as `stellarindex-aggregator:unit-not-found`) is **skipped**
loudly: that absence is a checked-in, reviewed fact, so refusing would make
the documented default permanently undispatchable there. A requested binary
the row has **no opinion about** is **refused** — nobody has checked its
unit, and deploying it blind is what rolled back. Every later step, and the
job summary, uses the effective set, so a skip is never silent.

**Reconcile the binary set against the host** runs after the SSH path is
proven and before the playbook, and answers two questions against the host
rather than the file:

- *Is the manifest still true?* Every daemon the row calls deployable must
  have a unit systemd can load, and every one it calls undeployable must
  still have none. Either disagreement refuses, naming
  `scripts/dev/preflight-deploy.sh --refresh-manifest`. This is how a stale
  row is detected: the file is a cache of a host property, and the host is
  read on every deploy.
- *Would this dispatch leave skew?* A partial set is not wrong in itself —
  re-running with only the stale binary named is the recovery path the
  served-path smoke prints. It is wrong when a binary the dispatch omits is
  not already on the deploying version, which is checked against the
  `deployed-versions` sidecars.

> **Deployability is unit presence, not enablement.** Measured 2026-09-07:
> testnet's aggregator is `UnitFileState=disabled` with
> `ActiveState=active` — off at boot, running now, and deployed there by
> this workflow at v0.62.0. An enablement test would drop it and re-create
> the very skew this exists to prevent. futurenet's is
> `LoadState=not-found`, which is the real exclusion. The reconciliation
> reads `LoadState` and nothing else. The CLI binaries (`ops`, `migrate`,
> `sla-probe`) have no unit at all and are exempt from the rule.

Both refusals are pinned by `scripts/ci/config-apply-gate-test.sh`, which
extracts the two steps from the workflow and runs them.

## Per-region setup

Each region needs four secrets configured in the repo's GitHub
Secrets settings:

| Secret | What it is |
| --- | --- |
| `<REGION>_HOST` | Public IP/hostname of the deploy target (e.g. `136.243.90.96` for r1) |
| `<REGION>_USER` | SSH user (defaults to `root` if unset) |
| `DEPLOY_SSH_PRIVATE_KEY` | OpenSSH private key whose public counterpart is in the host's `~/.ssh/authorized_keys`. Generate with `ssh-keygen -t ed25519 -f deploy-key`; the secret holds the contents of `deploy-key` (private). |
| `<REGION>_SSH_KNOWN_HOSTS` | Base64-encoded output of `ssh-keyscan -t ed25519 <host>`. Pinning known_hosts prevents MITM during the deploy connection. Use `ssh-keyscan -t ed25519 <host> \| base64` to produce. |

Currently only `r1` is wired. Adding `r2` / `r3`:

1. Add the four `R2_*` / `R3_*` secrets above.
2. Add the region to the workflow's `region` choice list in `.github/workflows/deploy.yml`.
3. Extend the `case` in the "Resolve region inventory" step to map the new region's secrets.
4. Optionally configure a GitHub Environment named after the region with required reviewers (forces manual approval before the deploy job runs).

## What the playbook does

Before the per-binary loop starts, the playbook's `pre_tasks` sync
the release tag's `migrations/` tree to the host and run
`stellarindex-migrate … up` — see
[§Migrations run before binaries, and are not rolled back](#migrations-run-before-binaries-and-are-not-rolled-back)
below for what that means for rollback.

[`configs/ansible/playbooks/deploy-binary.yml`](../../configs/ansible/playbooks/deploy-binary.yml)
loops over each requested binary and includes
[`configs/ansible/tasks/deploy-one-binary.yml`](../../configs/ansible/tasks/deploy-one-binary.yml).

Per-binary sequence:

1. **Resolve previous version** from the sidecar
   `/var/lib/stellarindex/deployed-versions/<binary>`. First-deploy
   fallback is a UTC timestamp.
2. **Stage** the new binary as `<install_dir>/<binary>.new`
   (controller → host copy via SSH).
3. **Backup** the current `<binary>` → `<binary>.prev-<previous-tag>`.
4. **Atomic rename** `.new` → live path.
5. **Write sidecar** with the new version tag.
6. **`systemctl restart <binary>.service`**.
7. **Grace period** (default 15s) before health probe.
8. **Health probe**:
   - `stellarindex-api`: `curl http://127.0.0.1:3000/v1/healthz` expects 200 (5 retries × 3s)
   - other binaries: `systemctl is-active` expects `active` (5 retries × 3s)
9. **Rollback on probe failure**:
   - Stop the failing service.
   - Move bad binary to `<binary>.failed-<new-version>` (preserved for post-mortem).
   - Restore `<binary>.prev-<previous-tag>` → live path.
   - Restore the previous sidecar.
   - Restart with old binary.
   - Fail the play (workflow surfaces non-zero).
10. **Prune backups** beyond the most-recent 5 to bound disk usage.

## Backup naming + rollback

Backups land at `/usr/local/bin/<binary>.prev-<tag>` where `<tag>` is
the SemVer of the previous deploy (resolved from the sidecar).
Examples after a few deploys:

```
/usr/local/bin/stellarindex-api
/usr/local/bin/stellarindex-api.prev-v0.2.0
/usr/local/bin/stellarindex-api.prev-v0.1.3
/usr/local/bin/stellarindex-api.prev-v0.1.2
```

To roll back manually (workflow path is preferred — see
release-process.md §Rollback):

```sh
ssh root@<host> "
  systemctl stop stellarindex-api
  mv /usr/local/bin/stellarindex-api /tmp/bad-stellarindex-api
  cp /usr/local/bin/stellarindex-api.prev-v0.1.3 /usr/local/bin/stellarindex-api
  echo v0.1.3 > /var/lib/stellarindex/deployed-versions/stellarindex-api
  systemctl start stellarindex-api
"
```

Then re-run `gh workflow run deploy.yml -f version=v0.1.3 …` to get
the workflow's state back in sync (idempotent — it'll be a no-op if
the sidecar already says v0.1.3 and the binary is healthy).

## Migrations run before binaries, and are not rolled back

`stellarindex-migrate … up` runs in `deploy-binary.yml`'s `pre_tasks`,
**before any binary is touched** (F-1220, codex audit-2026-05-12) — a
binary built against a newer schema must never restart onto an older
one. It's idempotent (`golang-migrate` skips already-applied
versions), so re-running the same deploy is a no-op here.

The per-binary rollback described above (§What the playbook does,
step 9) restores only the **binary**. It never runs `migrate down`.
So if a binary fails its health probe and rolls back, the database is
left on the **new** schema while the **old** binary runs again —
"atomic rollback" covers the binary, not the schema (CS-099,
`docs/audit-2026-06-30/01-cold-system-findings.md`).

**This is intentional, not a gap.** Auto-running `migrate down` on a
production rollback would be actively unsafe: down-migrations can be
data-destructive (dropped columns, narrowed types), and there is no
way for the pipeline to know whether something already depends on the
schema it would be reverting. The policy this repo already practices
instead — now codified in [`migrations/README.md`](../../migrations/README.md)
rule 9 — is that every up-migration must be **additive and
old-binary-safe**: the previous released binary has to keep running
correctly against the new schema. A schema that's one migration ahead
of the binary it's serving is therefore a supported, safe state, not
a bug — the old binary was written to tolerate exactly that (extra
nullable columns or tables it doesn't touch, no column it still reads
renamed or dropped out from under it). `down.sql` files exist for
local/dev iteration, not as a production rollback lever.

**If the migration step itself fails mid-deploy** (i.e. before any
binary was touched), the playbook fails at the `Apply outstanding
migrations` task and no binary is installed — the host is left on its
*old* binary and on whatever migrations applied before the failing
one (each migration commits in its own transaction per
`migrations/README.md` §Conventions, so a single failing migration
doesn't half-apply, but earlier migrations in the same run already
did). Operator checklist:

1. `stellarindex-migrate -migrations /usr/local/share/stellarindex/migrations status`
   on the host to see exactly which version it stopped at.
2. Read the failing migration's `.up.sql` alongside the Ansible task
   output for the actual Postgres error (permission, syntax, lock
   timeout, etc.).
3. Fix forward with a **new** migration — never hand-edit a migration
   that may have partially run in production (`migrations/README.md`
   rule 1).
4. Re-run the deploy once the fix lands in a new tag; `migrate up`
   resuming from where it stopped is exactly its designed idempotent-
   resume behaviour.

## Failure modes

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| Workflow fails at "Validate inputs" | `version` doesn't match SemVer | Re-run with a valid `vX.Y.Z` tag |
| "Region <X> host secret is unset" | `<REGION>_HOST` not configured in GitHub Secrets | Add the secret per §Per-region setup |
| "Bad binary preserved at …failed-vX.Y.Z" | New binary failed health probe; rolled back | Inspect `/usr/local/bin/<binary>.failed-<v>` on the host; `journalctl -u <binary> -n 200` shows why |
| SSH timeout / "permission denied" | Stale key, removed `authorized_keys` entry, host firewall change | Verify `DEPLOY_SSH_PRIVATE_KEY` is current; SSH manually from a known-good box |
| Fails at "Apply outstanding migrations" | A migration errored (permission, syntax, lock timeout) before any binary was touched | See §Migrations run before binaries, and are not rolled back — check `stellarindex-migrate … status`, fix forward with a new migration |
| "binary version skew on r1 after deploy" | The **Served-path smoke** step runs `stellarindex-binary-version-probe.service` and asserts `stellarindex_binary_version_skew == 0` / `probe_success == 1`. Non-zero means a release-managed binary is on a different version (binaries carry `-version`, e.g. `cmd/stellarindex-indexer/main.go`); a MISSING unit means the observability config has not been applied to r1 — the config-apply-gate class | Re-run the deploy naming the stale binary. Runbook: [binary-version-skew](runbooks/binary-version-skew.md) |
| "Config-apply gate" fails | A config/schema surface changed since the previous tag and `config_acknowledged` was not `true`. Non-destructive — the binaries are already live; the gate is flagging an outstanding config apply | Apply the config per [deploy-config-apply.md](deploy-config-apply.md), then re-run with `-f config_acknowledged=true` |
| "test net(s) behind vX.Y.Z" warning after an r1 deploy | The **Fleet release parity** step reads `/v1/version` on `api.testnet.stellarindex.io` and `api.futurenet.stellarindex.io` and compares it with the tag just deployed. Expected right after an r1 deploy — r1 goes first by design — which is why it warns rather than fails. It becomes drift if it persists: [`fleet-release-drift.yml`](../../.github/workflows/fleet-release-drift.yml) checks daily with a 24h grace measured from the oldest release the test net lacks and opens a tracking issue | Run the two dispatches the step prints (`region=testnet` / `futurenet`, `binaries=stellarindex-indexer,stellarindex-api,stellarindex-ops`). Procedure: [testnet-futurenet-deployment.md](testnet-futurenet-deployment.md#keeping-the-test-nets-in-step-with-r1) |

## Cross-references

- [`docs/operations/local-preflight.md`](local-preflight.md) — answer every question below locally, before the dispatch (`make preflight-deploy REGION=… VERSION=…`)
- [`docs/operations/release-process.md`](release-process.md) — the cut-tag side of the pipeline
- [`docs/architecture/semver-policy.md`](../architecture/semver-policy.md) — version tag rules
- [`.github/workflows/release.yml`](../../.github/workflows/release.yml) — produces the artefacts this consumes
- [`.github/workflows/deploy.yml`](../../.github/workflows/deploy.yml) — the workflow itself
- [`.github/workflows/fleet-release-drift.yml`](../../.github/workflows/fleet-release-drift.yml) — the daily test-net release-drift check that escalates a lag this workflow's parity step only warns about
- [`configs/ansible/playbooks/deploy-binary.yml`](../../configs/ansible/playbooks/deploy-binary.yml) — top-level playbook
- [`configs/ansible/tasks/deploy-one-binary.yml`](../../configs/ansible/tasks/deploy-one-binary.yml) — per-binary task list
- [`migrations/README.md`](../../migrations/README.md) — the additive-migrations policy §Migrations run before binaries, and are not rolled back depends on
