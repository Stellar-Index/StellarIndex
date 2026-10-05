---
adr: 0005
title: Monorepo with a single Go module
status: Accepted
date: 2026-04-22
supersedes: []
superseded_by: null
---

# ADR-0005: Monorepo with a single Go module

## Context

The codebase has several binaries (indexer, aggregator, API, ops CLI, migrate), a client SDK and a
shared type surface, plus docs, deploy kits, migrations and the OpenAPI spec. The choice was one repo
per component with a versioned shared-types module, or one repo with one Go module.

## Decision

One repository and one Go module, `github.com/Stellar-Index/StellarIndex`. `internal/` holds private
code, which Go makes non-importable. `pkg/` holds the narrow public surface: the client SDK and the
stable wire types, in `pkg/client`. Binary releases use SemVer (see
[docs/architecture/semver-policy.md](../architecture/semver-policy.md)). Deployment is bare-metal
systemd plus Ansible (`deploy/systemd/`, `configs/ansible/`).

## Invariant

There is one `go.mod`; no multi-module `go.work` setup and no split repos (AGENTS.md invariant 4,
enforced in review).

`internal/` is private; `pkg/` is the only public SemVer surface and may evolve only through
SemVer.

Nothing outside `scripts/` is a one-off script; everything in `internal/` is used by `cmd/*` or
`test/`.

## Consequences

Shared types (`CanonicalTrade`, `Asset`, `Amount`) have one home, and cross-cutting changes land in
one reviewed PR with one CI run and one release workflow. Docs, ADRs and runbooks live beside the
code. Costs are longer builds, noisy CI and hot-file merge conflicts, mitigated by per-package builds,
path filters, small PRs and CODEOWNERS. Revisit a split only if contributors exceed 5 with distinct
sub-teams, a component needs its own release cadence (e.g. `pkg/client` security patches), or unit
tests take over 5 minutes.

## Evidence

Root `go.mod`; `pkg/client/types.go`; `release.yml` ships cross-compiled binaries.
