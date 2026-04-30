# Tree Observations

Snapshot observations recorded from the checked-out tree on `2026-04-29`.
These are used when the evidence is a repo-state fact about presence or
absence rather than a statement inside one source file.

## OBS-0001 — Workflow set

- Observed via `find .github/workflows -maxdepth 1 -type f -print | sort`.
- Files present:
  - `.github/workflows/api-docs.yml`
  - `.github/workflows/ci.yml`

## OBS-0002 — Packaging surfaces

- Observed via `ls -la .goreleaser.yaml` and `find docker -maxdepth 2 -type f`.
- `.goreleaser.yaml` is absent from the checkout.
- `docker/` is absent from the checkout.

## OBS-0003 — Tool pinning helper tree

- Observed via `find tools -maxdepth 2 -type f`.
- `tools/` is absent from the checkout, so there is no checked-in tools
  module or tool manifest in that path for pinning formatter/security
  binaries.
