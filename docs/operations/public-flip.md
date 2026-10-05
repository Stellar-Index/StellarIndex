---
title: Public-flip strategy — publishing Stellar Index to a public repo at v1.0
last_verified: 2026-05-03
status: historical — the flip happened 2026-07-03; see v1-launch-plan.md
---

# Public-flip strategy

> **HISTORICAL.** The repo went public 2026-07-03 via a different mechanism than
> planned here (with a vault exposure; see `credential-rotation.md`). The org is
> `Stellar-Index/StellarIndex`. Nothing here is executable; residuals live in
> [`v1-launch-plan.md`](v1-launch-plan.md).

Binding decision: **publish to a NEW public repo; do NOT rewrite-and-force-push
the private repo.** The private repo holds the only copy of the internal archive,
WASM-audit evidence and per-region notes; a bad rewrite plus a failed backup loses
everything. A new-repo publish has zero force-push risk and keeps the audit trail.

New repo: org `StellarIndex`, name `stellar-index` (module path
`github.com/Stellar-Index/StellarIndex`), Apache-2.0, default branch `main`, initial
commit `Initial public release — Stellar Index v1.0` (full tree, no history),
CalVer releases from the launch tag with no parallel releases on both repos.

## Pre-flip checklist (all verified 2026-04-30)

| ✓ | Item | Evidence |
|---|---|---|
| ☑ | Predecessor-probe Postgres password, r1 public IP scrubbed; `configs/ansible/inventory/r1.yml` untracked + `.gitignore`d | commit 01a417654 |
| ☑ | `SECURITY.md` lists `security@stellarindex.io` | `SECURITY.md:9` |
| ☑ | `CODEOWNERS` external @-handles only | `CODEOWNERS` |
| ☑ | `README.md` public landing page; `CONTRIBUTING.md` welcomes externals (triage/review SLA, CoC link); `CODE_OF_CONDUCT.md` Contributor Covenant v2.1; `LICENSE` Apache-2.0 | files in root |
| ☑ | `.github/dependabot.yml` public registries only; CI workflows need no internal secrets | `.github/workflows/{ci,api-docs}.yml` |
| ☑ | `AGENTS.md` and `docs/operations/r1-deployment-state.md` free of private paths, credentials, IPs | reviewed |
| ☑ | Every ADR "Status" current (0001-0024 `Accepted`; 0012 reserved-future per multi-region-topology.md) | `docs/adr/` |
| ☑ | `gitleaks detect --source .` clean | gitleaks 8.30.1, 0 leaks / 553 commits |

## Final 24-hour pre-cutover dry-run

Re-run the gates 24 h before tagging v1.0; any failing row is a launch blocker:

1. `gitleaks detect --source . --redact --exit-code 1` from a clean checkout.
2. File-level scrub: no `*.env`, `*.key`, `*.pem`, `secrets/*`, `inventory/r1.yml`
   or other 01a417654-scrub patterns.
3. `make test && make test-integration` both green (a flake counts as red).
4. `last_verified` older than 90 days in `AGENTS.md` / `docs/architecture/*.md`
   goes to the L6.5 docs sweep.
5. `.github/workflows/ci.yml` has a green `main` run within 24 h (else a no-op
   commit to force one; the only non-read-only step).
6. `security@stellarindex.io` is monitored; the CODEOWNERS handle can triage
   day-1 PRs or has a delegate; `gh repo view Stellar-Index/StellarIndex` is 404.

## Cut-over mechanics

```sh
# 1. Tag and verify the v1.0 source on private
cd ~/code/stellarindex
git checkout main && git pull --ff-only
gitleaks detect --source . --redact --exit-code 1   # last secret scan
make test                                           # one final green build

# 2. Fork into a fresh working dir with no history
cd ~/code
git clone --no-local --no-hardlinks stellarindex stellarindex-public
cd stellarindex-public

# 3. Orphan-branch the public initial commit
git checkout --orphan public-v1
git add -A
git commit -m "Initial public release — Stellar Index v1.0

This is the first public release of the Stellar Index source code.
History prior to this commit lives in a private development repo
that is not published; CalVer release notes from this point forward
live in this repository.

See CHANGELOG.md and docs/architecture/semver-policy.md."

# 4. Verify working tree matches private at v1.0
diff -r --brief --exclude=.git ../stellarindex . | head -50
# Expect: zero diff. Anything reported is a publish-time slip-up.

# 5. Create the GitHub repo + push
gh repo create Stellar-Index/StellarIndex \
    --public \
    --description "Stellar Index API: Stellar protocol explorer, complete history, supply, and aggregated VWAP/TWAP/OHLC prices" \
    --license Apache-2.0
git remote remove origin
git remote add origin git@github.com:Stellar-Index/StellarIndex.git
git push -u origin public-v1:main

# 6. Tag the release on the public repo
git tag YYYY.MM.DD.N
git push origin YYYY.MM.DD.N
gh release create YYYY.MM.DD.N \
    --title "Stellar Index YYYY.MM.DD.N — Initial public release" \
    --notes-file /tmp/release-notes.md \
    --verify-tag
```

`--no-local --no-hardlinks` is deliberate: hardlinked clones share object storage,
so `--orphan` on the clone could affect the private repo's reflog.

## Post-flip

1. **Branch protection** on the public repo: require every workflow job as a
   status check, 1 approving review, forbid force-push to and deletion of `main`.
2. **Re-create CI secrets** (e.g. AWS credentials for the goreleaser job); audit-log
   the addition.
3. **Re-create issue templates / labels** from the private `.github/` (not migrated
   by clone-and-push).
4. **DNS cutover.** `docs.stellarindex.io` → public-repo GitHub Pages (or
   equivalent; L3.15); `status.stellarindex.io` → status page (L4.11).
5. **Stop CI on private**: `workflow_dispatch`-only; keep the repo as the audit trail.
6. **Announcement** in `#stellar-index-public` Discord.
7. **Re-scope** cron jobs / Renovate / Dependabot from private to public.

## Two-repo coexistence

- **Private** — full history and audit trail; new work lands here first, mirrored to
  public by weekly batch-merge (immediate for security fixes).
- **Public (`Stellar-Index/StellarIndex`)** — clean derived artefact; external PRs
  land here and are backported privately if they need internal-context discussion.

Purely internal work (e.g. runbooks referencing private IPs) is never mirrored.

## Cross-references

- [`semver-policy.md`](../architecture/semver-policy.md), [`release-process.md`](release-process.md)
- [`launch-readiness-backlog.md`](../architecture/launch-readiness-backlog.md) §Finalization — L6.3 (this doc) and L6.4 (production cutover)
- [`SECURITY.md`](../../SECURITY.md), [`CONTRIBUTING.md`](../../CONTRIBUTING.md)
