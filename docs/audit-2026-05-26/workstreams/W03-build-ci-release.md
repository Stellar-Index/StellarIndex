# W03 — Build, reproducibility, CI/CD, release controls

## Scope

`Makefile`, `scripts/dev/verify.sh`, `scripts/dev/cut-release.sh`,
and every workflow in `.github/workflows/`: ci.yml, api-audit.yml,
api-docs.yml, deploy.yml, docs-deploy.yml, explorer-deploy.yml,
k6-weekly.yml, release-validate.yml, release.yml,
status-page.yml.

Plus the lint scripts under `scripts/ci/`:
`lint-imports.sh`, `lint-actions-pinning.sh`, `lint-docs.sh`.

## Inputs

- `Makefile`
- `.github/workflows/*.yml`
- `scripts/dev/*.sh`, `scripts/ci/*.sh`
- `commitlint.config.js`
- branch protection state via `gh api`
- 2026-05-26 GitHub Actions incident postmortem (was
  `actions/setup-go@<sha>` unreachable for release.yml 3 times)

## Checks

| # | Check | Method |
| --- | --- | --- |
| W03.1 | `Makefile` targets all reproducible from a clean clone | shell |
| W03.2 | `scripts/dev/verify.sh` matches `.github/workflows/ci.yml` step-for-step | side-by-side diff |
| W03.3 | Every workflow file is syntactically valid + uses pinned actions | `lint-actions-pinning.sh` execution |
| W03.4 | `release.yml` cross-compiles linux/amd64, computes SHA256SUMS, extracts CHANGELOG, creates GitHub Release | workflow inspection |
| W03.5 | `deploy.yml` stage → backup → atomic install → restart → health probe → auto-rollback | playbook inspection |
| W03.6 | `cut-release.sh` validates SemVer + branch + tree + sync + non-empty CHANGELOG + verify.sh | script audit |
| W03.7 | Branch protection on `main`: required checks include ci.yml, monitoring-rules; signed commits? signature required? | `gh api` |
| W03.8 | `commitlint` rules hold against last 200 commits | rebuild commitlint |
| W03.9 | `lint-imports.sh` enforces architectural boundaries (no horizon import, xdr-scoped-to-scval, etc.) | script audit |
| W03.10 | `lint-docs.sh` enforces frontmatter age + alert-runbook pairing | script audit |
| W03.11 | NEW: 2026-05-26 GH Actions incident — release.yml retries failed on `actions/setup-go@40f158...` CDN unavailability; consider workflow-side retry mechanism | postmortem |
| W03.12 | `release.yml` does NOT publish container images (per CHANGELOG rc.74) — verify no GHCR step | workflow inspection |
| W03.13 | Workflow secrets vs documented secret list (each secret used by a workflow appears in operator docs) | secrets cross-ref |
| W03.14 | docker/* Dockerfiles: base image pinned, USER non-root, HEALTHCHECK, multi-arch | per-Dockerfile audit |
| W03.15 | govulncheck + gitleaks run in CI (not just in Makefile) | workflow inspection |

## Closure criteria

Every workflow audited. Findings on any drift, missing pin, or
secret leak.
