---
title: Release process — cutting a Stellar Index binary release
last_verified: 2026-11-20
status: living doc
---

# Release process

Procedure for cutting a binary release; implements
[`docs/architecture/semver-policy.md`](../architecture/semver-policy.md).
Tag format `vX.Y.Z` (root tag, no prefix); pre-v1, breaking changes bump
the minor.

```
git tag vX.Y.Z  → .github/workflows/release.yml
                → cross-compiles linux/amd64 (every region is amd64)
                → uploads binaries + SHA256SUMS (+ signatures, provenance) to GitHub Releases
                → operator runs deploy.yml (or manual scp)
```

**No container images**: `release.yml` does not push to ghcr.io.
Self-hosters build OCI images from the per-binary Dockerfiles under
`docker/` (`docker/README.md`).

## Pre-flight

A failure found mid-release wastes a tag and forces a `.N+1` cut.

1. **`main` is green**: every required AND optional job on the latest commit.
2. **Working tree matches `main`**: `git checkout main && git pull --ff-only origin main`.
3. **Write the release section** from `git log vPREV..HEAD --format=%s`,
   filtered to `feat`/`fix`/`perf` with a user-visible scope; `!` and
   `BREAKING CHANGE:` footers become Breaking/Deprecated entries. One line
   per change ([CONTRIBUTING.md §Changelog](../../CONTRIBUTING.md#changelog);
   only the newest five releases stay in the file).
4. **Name every breaking `pkg/*` change in it.** This is one Go module
   (ADR-0005): `pkg/client` ships inside the root `vX.Y.Z`, so never cut a
   `pkg/client/vX.Y.Z` tag. The CHANGELOG is the consumer's only notice
   (semver-policy.md, opening section: one root tag clock).
5. **Build is clean**: `make build`. If the release deploys the showcase
   site (`web/explorer/`, the launch-week default), also
   `NEXT_PUBLIC_API_BASE_URL=http://api.local-stub.invalid make web-build`
   and confirm `web/explorer/out/` exists.
6. **Record the Stellar protocol version tested against** (from
   `stellar-core --version` on a test node or the pubnet explorer header).

## Cut

1. **Decide the tag** per [`semver-policy.md` §"SemVer rules for binary
   releases"](../architecture/semver-policy.md#semver-rules-for-binary-releases):
   - new SSE endpoint, no schema change → minor (`v0.2.0 → v0.3.0`)
   - bug fix only, no operator-visible change → patch (`v0.3.0 → v0.3.1`)
   - removes a `[external]` config key → minor pre-v1.0 (`v0.3.1 → v0.4.0`), major post-v1.0
2. **Write the CHANGELOG section** in a one-commit PR titled
   `release: vX.Y.Z`: insert `## [vX.Y.Z] - YYYY-MM-DD` (UTC date) with
   the pre-flight step 3 text under the empty `## [Unreleased]` heading.
   `release.yml` extracts notes from the block headed exactly `## [vX.Y.Z]`
   up to the next `## [`. No link references to update.
3. **Squash-merge it once CI is green.** Never tag before it lands: the tag
   must point at the commit carrying the section.
4. **Tag with `scripts/dev/cut-release.sh`, never by hand.**
   ```sh
   git checkout main && git pull --ff-only origin main
   bash scripts/dev/cut-release.sh vX.Y.Z --dry-run   # every gate, no tag
   bash scripts/dev/cut-release.sh vX.Y.Z --yes       # tag + push
   ```
   It refuses unless: SemVer tag shape; on `main`, clean, in sync with
   `origin/main`; the tag does not exist locally or on origin; a
   **non-empty** `## [vX.Y.Z] - YYYY-MM-DD` section exists; the newest
   `docs/operations/sla-proof-YYYY-MM-DD.md` is within
   `SLA_PROOF_MAX_AGE_DAYS` (default 45) and records `Verdict: PROVEN`
   (any other verdict refuses unless `SLA_PROOF_OVERRIDE_REASON` carries a
   non-empty reason, which the script echoes); and
   `make prepush` prints `ALL REQUIRED CHECKS PASSED` (~20 min) with `main`
   unmoved. Non-TTY runs must pass `--yes` or `--dry-run` (exit 2 otherwise).
   Before prepush it dispatches the full `ci.yml` run that `release.yml`
   requires on the tagged commit, so the two overlap; `--dry-run` skips it.

   The tag push triggers `release.yml`, which cross-compiles every `cmd/`
   binary for `linux/amd64`, computes SHA256 sums, signs, attests, and
   creates the release with the CHANGELOG section as notes. It **refuses an
   existing release** for the tag: never re-run it or `gh release create`
   by hand; if different bytes are needed, bump the version.
5. **Verify the release**, every check before running anything you
   downloaded. The signer identity is pinned to `release.yml` at this tag
   (or a `main` dispatch), the same regex deploy.yml enforces in
   **Verify release signature, then checksums**; `release.yml@` on any other ref is a
   build from an unreviewed branch:
   ```sh
   gh release view vX.Y.Z
   VER=vX.Y.Z
   IDENTITY="^https://github\.com/Stellar-Index/StellarIndex/\.github/workflows/release\.yml@refs/(tags/${VER//./\\.}|heads/main)$"
   gh release download "$VER" -p SHA256SUMS -p SHA256SUMS.sigstore.json \
     -p stellarindex-indexer-linux-amd64
   ```

   **Cosign signature.** `SHA256SUMS` covers every binary, so its one
   signature verifies the release. The durable artifact is the Sigstore
   bundle `SHA256SUMS.sigstore.json` (signature + Fulcio cert + Rekor
   proof); needs **cosign v3** (`brew install cosign`; v2 has no `--bundle`):
   ```sh
   cosign verify-blob \
     --bundle SHA256SUMS.sigstore.json \
     --certificate-identity-regexp "$IDENTITY" \
     --certificate-oidc-issuer https://token.actions.githubusercontent.com \
     SHA256SUMS
   grep ' stellarindex-indexer-linux-amd64$' SHA256SUMS | sha256sum -c -   # macOS: shasum -a 256 -c -
   ```
   `release.yml` also publishes the retiring `SHA256SUMS.sig` /
   `SHA256SUMS.pem` (extracted from the bundle; do not rely on them
   staying). Releases cut before the bundle contract have only those and
   need **cosign v2** (v3 removed `--signature`/`--certificate`):
   ```sh
   gh release download "$VER" -p SHA256SUMS.sig -p SHA256SUMS.pem
   cosign verify-blob \
     --signature SHA256SUMS.sig \
     --certificate SHA256SUMS.pem \
     --certificate-identity-regexp "$IDENTITY" \
     --certificate-oidc-issuer https://token.actions.githubusercontent.com \
     SHA256SUMS
   ```

   **Build provenance.** Each binary and `migrations.tar.gz` carries a
   SLSA attestation tying it to the tagged commit and `release.yml` run
   (older releases carry none). Pin the signer the same way:
   ```sh
   gh attestation verify stellarindex-indexer-linux-amd64 \
     --repo Stellar-Index/StellarIndex --cert-identity-regex "$IDENTITY"
   ```

   Only after all three pass, on a linux/amd64 host:
   ```sh
   chmod +x stellarindex-indexer-linux-amd64
   ./stellarindex-indexer-linux-amd64 --version 2>&1 | head -3   # shows vX.Y.Z
   ```
6. **Add "Tested against protocol XX" to the Release page by hand**:
   `release.yml` has no protocol inference. `.github/RELEASE_NOTES_TEMPLATE.md`
   mirrors the structure if sections need expanding.

## Post-flight

1. **Announce** the release URL in the operator channel (and
   `#stellar-index-public` if applicable).
2. **Record operator actions** taken (migration step, config edit) in
   `docs/operations/r1-deployment-state.md`. The running version is
   whatever `/v1/version` reports.
3. **Watch dashboards for 1 h**: the SLO board and the per-pair freshness
   panel. An anomaly gets normal incident triage; file a SEV before
   considering rollback.
4. **Rollback** if needed (next section): SEV-2 minimum and a postmortem
   in `docs/operations/postmortems/`.
5. **Publish the docs**: `gh workflow run docs-deploy.yml --ref main`. It is
   manual, and when it is skipped the public API reference drifts from the repo.

## Rollback

Binaries run under systemd on bare metal ([ADR-0008](../adr/0008-ha-topology.md));
a rollback is a binary swap per affected host.

### Pre-rollback

1. **Previous known-good tag**: `git tag` history, the release workflow's
   runs (`gh run list --workflow release.yml`), or the `.prev-<tag>`
   names from step 2.
2. **Previous binary on disk**: `configs/ansible/tasks/deploy-one-binary.yml`
   keeps the last 5 as `/usr/local/bin/<binary>.prev-<previous-tag>` and
   writes `/var/lib/stellarindex/deployed-versions/<binary>`:
   ```sh
   ssh root@<host> 'ls -lh /usr/local/bin/stellarindex-*.prev-* 2>/dev/null'
   ssh root@<host> 'cat /var/lib/stellarindex/deployed-versions/stellarindex-api'
   ```
   If pruned (>5 releases back), rebuild from the tag
   (`git checkout <tag> && make build`) on a build host.
3. **Scope**: the whole region set. The deploy refuses a partial set whose
   omitted binaries are not already on the target tag, which after a release
   is all of them.

### Procedure

Re-dispatch the deploy workflow with the previous tag, the whole set and
`migrations_skip=true`, then watch the run. The command, and why each part
is required, is in
[`rollback.md` §Rolling back the binaries](rollback.md#rolling-back-the-binaries):

```sh
gh workflow run deploy.yml \
  -f region=r1 \
  -f version=<previous-tag> \
  -f migrations_skip=true
```

Fallback, only if the deploy workflow itself is broken, per binary for every
binary the region runs (CLI binaries have no unit: `cp` and sidecar only):

```sh
PREVIOUS=<previous-tag>
BINARY=stellarindex-api

ssh root@<host> "
  systemctl stop ${BINARY} && \
  cp /usr/local/bin/${BINARY}.prev-${PREVIOUS} /usr/local/bin/${BINARY} && \
  echo ${PREVIOUS} > /var/lib/stellarindex/deployed-versions/${BINARY} && \
  systemctl start ${BINARY} && \
  systemctl status ${BINARY} --no-pager | head -20
"
```

On a multi-host API tier roll one host at a time: drain it in HAProxy via
the stats socket (`disable server api_pool/api-01`), swap, re-enable,
repeat. Indexer and aggregator are single-active; swap without drain.

### Post-rollback

1. `curl -sf http://<host>:3000/v1/version` reports the previous tag.
2. The alert that drove the rollback clears within 5 min.
3. Note the rollback in the postmortem.
4. Never delete the broken tag; cut a `.N+1` hotfix once fixed.

## Hotfix releases

Same procedure, except:

- Branch from the previous release tag (not `main`), apply the fix, cut
  the next patch tag.
- The CHANGELOG entry references the originating incident's postmortem.
- The announcement flags it as a hotfix with a one-line scope + PR link.

Hotfixes never include unrelated work; that goes in the next regular release.

## Cross-references

- [`docs/architecture/semver-policy.md`](../architecture/semver-policy.md) — the policy this runbook implements
- [`.github/RELEASE_NOTES_TEMPLATE.md`](../../.github/RELEASE_NOTES_TEMPLATE.md) — release notes template
- [`CHANGELOG.md`](../../CHANGELOG.md) — section structure
- [`docs/operations/sev-playbook.md`](sev-playbook.md) — incident response if a release misbehaves
