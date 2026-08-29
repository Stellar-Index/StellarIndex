# AGENTS.md — Stellar Index

> Generic AI-agent orientation file — a short digest, not a second
> map. The full repo orientation (layout, invariants, task recipes,
> footguns) lives in [CLAUDE.md](CLAUDE.md), which is the source of
> truth wherever the two overlap; the overlapping command block below
> is copied from it verbatim and CI enforces that (lint-docs §18a).
> Use whichever your agent scaffolding prefers.

## Docs index

| Doc | Contents |
| --- | -------- |
| [README.md](README.md) | Project overview, status, contact |
| [CLAUDE.md](CLAUDE.md) | Full repo orientation (layout, invariants, task recipes, footguns) |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Contribution workflow + Definition of Done |
| [SECURITY.md](SECURITY.md) | Vulnerability disclosure process |
| [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) | Contributor Covenant 2.1 |
| [CHANGELOG.md](CHANGELOG.md) | Keep-a-Changelog format; Unreleased at top |
| [VERSIONS.md](VERSIONS.md) | Pinned SHAs of every upstream dependency |
| [docs/engineering-standards.md](docs/engineering-standards.md) | The enforcement policy |
| [docs/architecture/semver-policy.md](docs/architecture/semver-policy.md) | Versioning + repo-layout rationale |
| `docs/adr/` | Architecture Decision Records (numbered, immutable) |
| `docs/architecture/` | Narrative design docs |
| `docs/protocols/` | Per-protocol verification pages |

## Invariants — never violate these

See [CLAUDE.md](CLAUDE.md) for the full list with ADR cross-refs.
Short form:

1. **i128 / u128 never truncates to int64.** ADR-0003.
2. **Horizon is not in our architecture.** ADR-0001.
3. **Self-hosted storage is S3-compatible, not local filesystem.**
   ADR-0002.
4. **Monorepo with one `go.mod`.** ADR-0005.
5. **Validator track post-launch targets Tier-1.** ADR-0004.
6. **Per-source coverage invariant.** Every per-source hypertable
   must register in `DefaultGapDetectorTargets` (same PR as the
   migration). ADR-0030.

## Quick-start commands

```sh
make help              # list all targets
make dev               # docker-compose up the local dependency stack (TimescaleDB + Redis + MinIO); the app binaries run on the host, and there is no API/ClickHouse service in the compose file
make lint              # golangci-lint (gofumpt runs as a golangci formatter; architectural import boundaries enforced by scripts/ci/lint-imports.sh)
make test              # unit tests (fast; ~2 min)
make verify            # canonical pre-push gate (fmt, vet, lint, docs, vuln, test) — run this before every push
make docs-all          # regenerate docs/reference/ from OpenAPI + struct tags (the metrics reference is hand-edited; drift-guarded by lint-docs, not generated)
```

## No orphan work — the contract

Stated once, here; CONTRIBUTING.md and CLAUDE.md point at this
section. It exists because on 2026-08-27 a three-file fix branch was
pushed with no PR and no backlog line; the next day a different
agent re-diagnosed the same alert from scratch and fixed it
differently (#254 — the real root cause), and the orphan was found
only by a manual branch audit. A postmortem sat on another orphan
branch for a day (#255).

1. **Every pushed branch gets a PR in the same working session.**
   Draft is fine. A branch with no PR is not work, it is loss —
   nobody else can see it, so it will be redone. The daily
   `orphan-branches` workflow lists any PR-less branch older than
   24h in a tracking issue; don't be on it.
2. **Prior-art check before starting on a symptom, alert or
   finding.** Run all of:
   - `gh pr list --state all --search "<alert name or symptom keywords>"`
   - `git branch -r | grep -i <keyword>`
   - grep the backlog (`docs/operations/v1-launch-plan.md`) and the
     alert's runbook (`docs/operations/runbooks/`).

   Record the result in the PR body's **Prior art** field:
   `none` or `#NNN (superseded because …)`.
3. **The PR body names the alert/finding and states root cause vs
   symptom.** "Fixes `stellarindex_assets_popular_priceless`" is a
   symptom; say what was actually wrong and why this change removes
   it rather than hides it.
4. **Superseding a prior attempt means closing it with a comment
   saying why.** Never silently — the closed PR is how the next
   reader learns which diagnosis lost and why.

## When in doubt

- Smallest-possible PR that advances one thing.
- Read the nearest `doc.go` or `README.md` before you touch code.
- Decisions go in `docs/adr/`, not architecture docs.
- Every `TODO` has a linked issue.
- Every amount is `*big.Int`, not `int64`.
