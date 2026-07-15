---
title: Release process — cutting a Stellar Index binary release
last_verified: 2026-07-10
status: living doc
---

# Release process

End-to-end procedure for cutting a Stellar Index binary release. This
is the runbook the on-rotation release engineer follows; it
implements the policy ratified in
[`docs/architecture/semver-policy.md`](../architecture/semver-policy.md).

SemVer tag format: `vX.Y.Z` (root tag, no prefix). Pre-v1, breaking
changes bump the minor; minor + patch follow the standard rules.

The pipeline is:

```
git tag vX.Y.Z      → release.yml fires
                    → cross-compiles linux/amd64
                      (arm64 dropped 2026-05-08; every region is
                       amd64; re-add when an arm64 host lands)
                    → uploads binaries + SHA256SUMS to GitHub Releases
                    → operator runs deploy.yml (or manual scp)
```

> **No container images.** `release.yml` deliberately does NOT push
> to ghcr.io — F-1221 (codex audit-2026-05-12) flagged old docs that
> implied otherwise. Self-hosters who want OCI images build them
> locally from the per-binary Dockerfiles under `docker/`. See
> `docker/README.md`.

Run the `release.yml` and `deploy.yml` workflows in
`.github/workflows/`; this doc captures the human-side decisions
they don't automate.

## Pre-flight

Done **before** cutting the tag — discovering any of these failed
mid-release wastes a tag and forces a `.N+1` cut.

1. **`main` is green.** The latest commit's CI run is all-passing on
   GitHub. No "merged with optional check failures" — every required
   AND optional job must be green.
2. **Working tree matches `main`.** `git checkout main && git pull
   --ff-only origin main`.
3. **CHANGELOG.md `[Unreleased]` is curated.** Walk it top to bottom
   and confirm every entry has a PR citation, every section heading
   that has no entries has been deleted, and that the order matches
   user-relevance (operator-visible at the top, internal refactors
   at the bottom).
4. **`pkg/*` version bumps are tagged.** If this release ships a new
   `pkg/client` version, that module's tag (`pkg/client/vX.Y.Z`)
   already exists on `main` from an earlier landed PR — **do not**
   bump `pkg/*` versions in the same commit as a CalVer release.
5. **Build dry-run is clean.** `make build` completes for every
   checked-in binary without errors. If the release will deploy
   the showcase site (`web/explorer/`) alongside the binaries —
   which is the launch-week default — also run
   `NEXT_PUBLIC_API_BASE_URL=http://api.local-stub.invalid make
   web-build` and confirm it produces `web/explorer/out/`. CI
   already gates on this per the `web/explorer` job, but local
   verification before tagging catches the rare case where a
   merge-conflict fix on `main` slipped past the per-PR gate.
6. **Stellar protocol is documented.** The protocol version the
   release was tested against is known (e.g. `23` for post-Whisk).
   Pulled from `stellar-core --version` on a test node, or from the
   pubnet block-explorer header.

## Cut

1. **Decide the tag.** Apply the bump rules from
   [`semver-policy.md` §"What constitutes a breaking change for
   binaries"](../architecture/semver-policy.md). Examples:
   - Adds a new SSE endpoint, no schema change → minor bump (`v0.2.0 → v0.3.0`)
   - Bug fix only, no operator-visible change → patch bump (`v0.3.0 → v0.3.1`)
   - Removes a `[external]` config key → minor bump pre-v1.0 (`v0.3.1 → v0.4.0`); major bump post-v1.0
2. **Promote the CHANGELOG `[Unreleased]` block.** In a one-commit
   PR:
   - Replace `## [Unreleased]` with `## [vX.Y.Z] — YYYY-MM-DD`
   - Add a fresh empty `## [Unreleased]` block above it
   - At the bottom of the file, update the version-comparison links
     to point at the new tag
   - Title the PR `release: vX.Y.Z`
3. **Merge the release PR.** Squash-merge once CI is green. **Do
   not** tag before this PR has landed on `main` — the tag must
   point at the commit that contains the promoted CHANGELOG block.
4. **Create + push the tag.**
   ```sh
   git checkout main && git pull --ff-only origin main
   git tag vX.Y.Z
   git push origin vX.Y.Z
   ```
   The tag push triggers `.github/workflows/release.yml` which:
   - Cross-compiles every binary in `cmd/` for `linux/amd64` (and
     `linux/arm64` if the matrix is enabled)
   - Computes SHA256 sums
   - Uploads the binaries + `SHA256SUMS` + the CHANGELOG section as
     release notes to GitHub Releases
   - **Does not** publish container images. The previous GHCR job
     was dropped (search the git log for "release: drop ghcr.io
     push") because no consumer of those images existed. Self-
     hosters who need images build them from `docker/<binary>
     .Dockerfile` locally — see `docker/README.md`.
5. **Verify the release.**
   ```sh
   gh release view vX.Y.Z
   gh release download vX.Y.Z -p stellarindex-indexer-linux-amd64 -O /tmp/v.bin
   /tmp/v.bin --version 2>&1 | head -3   # version line should show vX.Y.Z
   sha256sum /tmp/v.bin                  # cross-check against SHA256SUMS
   ```

   **Verify the cosign signature.** `SHA256SUMS` transitively covers every
   binary in the release (a tampered binary fails the `sha256sum -c` step
   above), so verifying the one manifest signature verifies the whole
   release. As of the first release cut after 2026-07-10 (BACKLOG #51b),
   the durable artifact contract is a **Sigstore bundle** —
   `SHA256SUMS.sigstore.json`, containing the signature, Fulcio
   certificate, and Rekor transparency-log proof in one file:
   ```sh
   gh release download vX.Y.Z -p SHA256SUMS -p SHA256SUMS.sigstore.json
   cosign verify-blob \
     --bundle SHA256SUMS.sigstore.json \
     --certificate-identity-regexp \
       '^https://github.com/Stellar-Index/StellarIndex/\.github/workflows/release\.yml@.*$' \
     --certificate-oidc-issuer https://token.actions.githubusercontent.com \
     SHA256SUMS
   ```
   Requires **cosign v3** (`brew install cosign`, or a binary from
   <https://github.com/sigstore/cosign/releases> — see
   <https://docs.sigstore.dev/cosign/system_config/installation/>).
   cosign v2 cannot read the bundle format at all (no `--bundle` flag).

   **Old-contract releases (cut before 2026-07-10) only published
   `SHA256SUMS.sig` / `SHA256SUMS.pem`** and need **cosign v2** to verify —
   cosign v3 removed `verify-blob --signature`/`--certificate` entirely,
   so a v3-only install cannot check them:
   ```sh
   gh release download vX.Y.Z -p SHA256SUMS -p SHA256SUMS.sig -p SHA256SUMS.pem
   cosign verify-blob \
     --signature SHA256SUMS.sig \
     --certificate SHA256SUMS.pem \
     --certificate-identity-regexp \
       '^https://github.com/Stellar-Index/StellarIndex/\.github/workflows/release\.yml@.*$' \
     --certificate-oidc-issuer https://token.actions.githubusercontent.com \
     SHA256SUMS
   ```
   The first post-migration release publishes **both** shapes as a
   one-release transition courtesy (dual-publish) — don't rely on
   `.sig`/`.pem` still being there on the *next* tag; see the CHANGELOG
   `[Unreleased]` entry for the deprecation note.
6. **Optional manual edits to the Release page.** The auto-generated
   notes pull from the CHANGELOG block. Add the "Tested against
   protocol XX" line manually if the workflow couldn't infer it
   (it tries `stellar-core --version` from the build runner). The
   `.github/RELEASE_NOTES_TEMPLATE.md` mirrors the structure if you
   need to expand sections.

## Post-flight

1. **Announce.** Post the release URL to the operator channel +
   `#stellar-index-public` if applicable.
2. **Update `docs/operations/r1-deployment-state.md`** with the
   running version and any operator action that was taken (e.g.
   migration step, config edit).
3. **Watch dashboards for 1 h.** The standard SLO board + the
   per-pair freshness panel. Any anomaly within the first hour gets
   the same triage as a normal incident — file a SEV before
   considering rollback.
4. **Rollback path** (if needed): see the next section. File a SEV-2
   minimum and a postmortem in `docs/operations/postmortems/`.

## Rollback

The Stellar Index ships as systemd-managed binaries on bare-metal
hosts (per [ADR-0008](../adr/0008-ha-topology.md)) — there is no
container registry to retag and no orchestrator to roll back. A
rollback is a binary swap on each affected host.

### Pre-rollback

1. **Confirm the previous-known-good tag.** Either from `git tag`
   history or from `r1-deployment-state.md`'s "Running version"
   line at the time the current release was cut.
2. **Confirm the previous binary is still on disk.** The deploy
   task in `configs/ansible/tasks/deploy-one-binary.yml` keeps the
   last 5 previous binaries as
   `/usr/local/bin/<binary>.prev-<previous-tag>` and writes a sidecar
   marker to `/var/lib/stellarindex/deployed-versions/<binary>`. Check
   both:
   ```sh
   ssh root@<host> 'ls -lh /usr/local/bin/stellarindex-*.prev-* 2>/dev/null'
   ssh root@<host> 'cat /var/lib/stellarindex/deployed-versions/stellarindex-api'
   ```
   If the wanted `.prev-<tag>` is pruned (>5 releases back),
   rebuild it from the tag (`git checkout <tag> && make build`)
   on a build host before continuing. F-1222 (codex audit-2026-05-12):
   prior docs pointed at `/opt/stellarindex/release-<tag>/` which the
   deploy task does not produce.
3. **Decide the scope.** A bad indexer release does not require
   rolling back the API. Roll back only the affected binary unless
   the failure is shared (e.g. a config schema break).

### Procedure (per host, per binary)

Preferred: trigger the deploy workflow with the previous tag:

```sh
gh workflow run deploy.yml \
  -f region=r1 \
  -f version=v0.2.0 \
  -f binaries=stellarindex-api,stellarindex-indexer
```

The workflow does the host-side backup→swap→restart→health-probe
sequence with automatic rollback on probe failure. Use this path
unless the deploy workflow itself is the thing that broke.

Fallback (manual, per host, per binary):

```sh
PREVIOUS=v0.2.0                               # the known-good tag
BINARY=stellarindex-api                        # or -indexer, -aggregator

ssh root@<host> "
  systemctl stop ${BINARY} && \
  cp /usr/local/bin/${BINARY}.prev-${PREVIOUS} /usr/local/bin/${BINARY} && \
  echo ${PREVIOUS} > /var/lib/stellarindex/deployed-versions/${BINARY} && \
  systemctl start ${BINARY} && \
  systemctl status ${BINARY} --no-pager | head -20
"
```

For the API tier the rollback is **rolling**: drain one host out
of HAProxy via the stats socket (`disable server api_pool/api-01`),
swap that host's binary, re-enable, repeat. Avoids a 30-second
2-of-3-host window during the cutover. Indexer and aggregator are
single-active and can be swapped one at a time without drain.

### Post-rollback

1. Verify the runtime version: `curl -sf http://<host>:3000/v1/version`
   reports the previous tag.
2. The same alert that drove the rollback should clear within 5 min.
3. Update `docs/operations/r1-deployment-state.md` "Running version"
   and note the rollback in the postmortem.
4. The original (broken) tag stays on `main` — DO NOT delete it.
   Cut a `.N+1` hotfix once the underlying bug has a fix.

## Hotfix releases

Same procedure as above, with these differences:

- Branch from the previous release tag (not `main`), apply the fix,
  cut a new `.N` tag on the same day OR a new date if the day has
  changed
- The CHANGELOG entry under the hotfix tag references the originating
  incident's postmortem
- Post-flight notification flags this as a hotfix and includes the
  scope of what changed (one-line + link to PR)

Hotfixes never include unrelated work. If a fix needs additional
changes that aren't strictly required, those go into the next
regular release — never a hotfix.

## Cross-references

- [`docs/architecture/semver-policy.md`](../architecture/semver-policy.md) — the policy this runbook implements
- [`.github/RELEASE_NOTES_TEMPLATE.md`](../../.github/RELEASE_NOTES_TEMPLATE.md) — the template release engineers fill in
- [`CHANGELOG.md`](../../CHANGELOG.md) — every release's entry follows the same structure
- [`docs/operations/sev-playbook.md`](sev-playbook.md) — incident response if a release misbehaves
