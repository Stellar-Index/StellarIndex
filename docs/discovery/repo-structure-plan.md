# Repo structure plan

**Status:** ✅ Design-locked plan. This is the blueprint for the
implementation repository that will replace `~/code/rates` and house
all of Phase 2+ execution. **We initialise this structure before any
Phase-2 production code lands.**

**Guiding user directives** (from the /loop instruction):

> "I want a plan around the repo structure to ensure we enforce good
> practice and don't have stale docs and technical debt. This needs
> to follow all best practices and be incredibly clearly structured
> and well thought through. This is a serious infrastructure project
> and needs to be designed to an enterprise grade."

Every design choice below is justified against those four criteria.

---

## 1. Decision: single repo (monorepo)

One repository: **`ctx/ratesengine`** (exact org/name TBD; use that as
the working placeholder).

### Why monorepo

1. **Shared types.** Our `CanonicalTrade`, `CanonicalPrice`, `Asset`,
   and `Pair` types are touched by indexer, aggregator, API, and
   client SDK. A multi-repo setup forces us to extract them into a
   shared `types` module and version it independently — a coordination
   tax we pay on every PR. Monorepo keeps one version of truth.
2. **Atomic cross-cutting changes.** Adding a new asset source (e.g. a
   new oracle) touches the consumer, the aggregator, the API response
   shape, and the client SDK. One PR, one review, one merge.
3. **Open-source contribution friction.** An external contributor
   wanting to add a CEX connector should do it in one PR against one
   repo with one CI run. Multi-repo "please also update ctx-types to
   v1.8.2 in ctx-api and ctx-indexer" is a well-documented path to
   contributor churn.
4. **One version, one release.** SemVer + CalVer on a single artifact
   family. No "ctx-api v2.4 requires ctx-indexer v2.3+" compatibility
   matrices for operators to track.
5. **RFP requires open-source.** Our transparency story is stronger if
   every line of production code lives in one auditable place.

### Known trade-offs (and how we mitigate)

| Trade-off | Mitigation |
| --------- | ---------- |
| Build time grows with repo | Go's per-package build; use `go build ./cmd/<name>` not `./...` in CI fast-path |
| CI fanout on unrelated changes | Path filters in GitHub Actions — a `docs/` change doesn't run full test suite |
| Merge conflicts on hot files | Small PRs policy; CODEOWNERS routing reviews to domain experts |
| Monorepos tempt "one-off" tooling | Explicit rule: everything under `internal/` must be used by `cmd/` or `test/`; no one-off scripts outside `scripts/` |

### Rejected alternative: split repos

We considered **ctx-types + ctx-indexer + ctx-api + ctx-deploy-kit**.
Rejected because the versioning overhead outweighs every benefit for
a 10-week delivery. Revisit if the team grows past 5 contributors or
if we ship a stable v1.x and want to move API development independently.

---

## 2. Top-level layout

```
ctx/ratesengine/
├── README.md
├── LICENSE                    (Apache-2.0)
├── CHANGELOG.md               (kept hand-edited; see §10)
├── CONTRIBUTING.md
├── CODE_OF_CONDUCT.md
├── CODEOWNERS                 (GitHub review routing)
├── SECURITY.md                (how to report vulns)
├── VERSIONS.md                (pinned SHAs of every upstream dep we audit)
├── Makefile
├── go.mod
├── go.sum
├── go.work                    (if we split internal into multiple modules — see §3)
├── .github/
│   ├── workflows/
│   │   ├── ci.yml
│   │   ├── release.yml
│   │   ├── security.yml
│   │   └── docs.yml
│   ├── ISSUE_TEMPLATE/
│   │   ├── bug_report.md
│   │   ├── feature_request.md
│   │   └── config.yml
│   ├── PULL_REQUEST_TEMPLATE.md
│   └── dependabot.yml
├── .golangci.yml              (lint config)
├── .goreleaser.yaml           (multi-arch binary + deb + docker release)
├── cmd/
│   ├── ctx-indexer/
│   ├── ctx-aggregator/
│   ├── ctx-api/
│   ├── ctx-ops/               (admin CLI: backfill, gap-detect, cache-prime)
│   └── ctx-migrate/           (db-migration runner)
├── internal/                  (not importable externally — Go's rule)
│   ├── canonical/             (Trade, Price, Asset, Pair, Amount-as-big.Int)
│   ├── config/
│   ├── consumer/
│   ├── extract/               (wrapper over stellar-extract)
│   ├── sources/
│   │   ├── soroswap/
│   │   ├── aquarius/
│   │   ├── phoenix/
│   │   ├── comet/
│   │   ├── blend/
│   │   ├── sdex/
│   │   ├── reflector/
│   │   ├── redstone/
│   │   ├── band/
│   │   ├── chainlink/
│   │   └── external/
│   │       ├── cex/
│   │       │   ├── binance/
│   │       │   ├── coinbase/
│   │       │   ├── kraken/
│   │       │   ├── bitstamp/
│   │       │   └── …
│   │       ├── fx/
│   │       ├── coingecko/
│   │       └── coinmarketcap/
│   ├── aggregate/             (VWAP, TWAP, outlier, triangulation)
│   ├── supply/                (circulating/total/max derivation)
│   ├── storage/
│   │   ├── timescale/
│   │   ├── redis/
│   │   └── minio/
│   ├── api/
│   │   ├── v1/                (handlers + request/response types)
│   │   ├── middleware/
│   │   └── streaming/
│   ├── auth/
│   ├── ratelimit/
│   ├── metadata/              (SEP-1 / home-domain resolution)
│   ├── divergence/            (cross-check against CoinGecko/CMC/Chainlink)
│   ├── health/
│   ├── obs/                   (metrics, tracing, logging)
│   └── version/               (build-time version injection)
├── pkg/                       (public, importable)
│   ├── client/                (Go SDK for our API)
│   └── types/                 (stable types our API consumers depend on)
├── migrations/                (golang-migrate SQL migrations)
├── configs/
│   ├── defaults.yaml
│   ├── dev.yaml
│   ├── prod.yaml.example
│   └── asset_supply_policy.yaml.example
├── openapi/
│   ├── ctx-rates.v1.yaml      (source of truth for the API)
│   └── README.md              (how to regenerate clients from here)
├── deploy/
│   ├── docker-compose/        (full-stack single-host)
│   ├── k8s/
│   │   ├── base/
│   │   └── overlays/
│   ├── nomad/                 (optional)
│   ├── baremetal/             (systemd + setup scripts for our colo)
│   └── stellar-toml/          (SEP-20 validator self-verification)
├── docker/                    (Dockerfiles per component)
├── scripts/
│   ├── dev/                   (`dev/up`, `dev/teardown`, `dev/seed`)
│   ├── ops/                   (runbook helpers)
│   └── ci/
├── test/
│   ├── fixtures/              (golden-file LedgerCloseMeta samples)
│   ├── integration/           (Go integration tests behind build tag)
│   ├── load/                  (k6 / vegeta scripts)
│   └── chaos/                 (pre-release chaos scenarios)
├── tools/                     (Go tools pinned via go.mod; not shipped)
│   └── tools.go
└── docs/
    ├── README.md              (docs index)
    ├── architecture/
    │   ├── overview.md
    │   ├── data-flow.md
    │   ├── threat-model.md
    │   └── protocol-versions.md   (ported)
    ├── adr/
    │   ├── README.md              (template + index)
    │   ├── 0001-horizon-deprecated.md
    │   ├── 0002-minio-s3-compat-storage.md
    │   ├── 0003-i128-no-truncation.md
    │   ├── 0004-tier1-validator-aspiration.md
    │   └── 0005-monorepo.md
    ├── reference/
    │   ├── api/                    (auto-generated from openapi/)
    │   ├── metrics/                (auto-generated from Prometheus scrape)
    │   └── config/                 (auto-generated from config struct tags)
    ├── operations/
    │   ├── runbooks/
    │   ├── sev-playbook.md
    │   ├── backup-dr.md
    │   └── onboarding-operator.md  (for self-hosted operators)
    ├── development/
    │   ├── getting-started.md
    │   ├── testing.md
    │   └── contributing-a-source.md
    └── _archive/
        └── discovery/              (ported from docs/discovery/, frozen)
```

---

## 3. Go module layout — single module, `internal/` boundaries

### Single `go.mod`

Despite being a monorepo we use **one Go module**. Reasoning:

- Multi-module monorepos require `go.work` orchestration and version-pin
  every internal cross-reference. Complex for negligible benefit at
  our scale.
- A single module lets us refactor across package boundaries without
  version dance.
- External consumers only import from `pkg/` (public surface).

### `internal/` vs `pkg/`

- **`internal/`** — everything private to our server. Go enforces
  non-importability. This is ~95 % of our code.
- **`pkg/`** — two explicit public surfaces:
  - `pkg/client/` — a Go client library for our API.
  - `pkg/types/` — the type definitions our API-client consumers
    depend on (`Trade`, `Price`, `Asset`, `Pair` — the minimum set
    to write an integration).

We **commit to backwards compatibility on `pkg/*`** via SemVer. We
have **no such commitment on `internal/*`** — any internal package
can be refactored in any PR.

### Package naming conventions

- Package name = final path segment, always lowercase, single word.
- No stuttering: `canonical.Trade` not `canonical.CanonicalTrade`.
- Interfaces named for the behaviour, not prefixed with `I`.
- Test helpers in a separate `fooutil` or `footest` package when they
  need to be importable by sibling packages.

### Dependency rules (enforced via `go-arch-lint` or similar in CI)

```
cmd/*           → internal/*, pkg/*
internal/api    → internal/canonical, internal/auth, internal/ratelimit,
                  internal/storage (via interface), internal/obs
internal/sources/* → internal/canonical, internal/extract,
                     internal/consumer, internal/obs
internal/aggregate → internal/canonical, internal/storage (via interface),
                     internal/obs
internal/storage/* → internal/canonical, internal/obs
pkg/client      → pkg/types   (and nothing else internal)
pkg/types       → (no internal deps — it's the public surface)
```

No package imports `cmd/` or `test/`. No `internal/` package imports
`pkg/` (public depends on private, never the other way round).

---

## 4. Per-component organisation

Every source implementation (`internal/sources/<protocol>/`) follows
the same five-file pattern (stolen from stellar-etl, simplified):

```
internal/sources/soroswap/
├── README.md              (what this source indexes, open items)
├── events.go              (topic filters + event decoding)
├── decode.go              (XDR → CanonicalTrade for this source)
├── factory.go             (enumerate pair contracts)
├── consumer.go            (subscribe + backfill orchestration)
└── source_test.go         (unit tests)
```

Corresponding fixtures live in `test/fixtures/soroswap/`. Integration
tests in `test/integration/sources/soroswap_test.go`.

Every source package must export exactly three things:

1. `Name() string` — for metrics / logging / config.
2. `New(deps Deps) Source` — constructor with explicit dependencies.
3. The `Source` interface it implements.

The `Source` interface lives in `internal/consumer/` and is uniform:

```go
type Source interface {
    Name() string
    BackfillRange(ctx context.Context, from, to uint32, out chan<- canonical.Trade) error
    StreamLive(ctx context.Context, out chan<- canonical.Trade) error
    Health() HealthStatus
}
```

This is the internal mirror of nebu's Origin/Transform/Sink pattern
(see [data-sources/withobsrvr-nebu.md](data-sources/withobsrvr-nebu.md))
but scoped to our specific canonical type.

---

## 5. Documentation structure

Three docs trees, with deliberately different freshness guarantees.

### `docs/architecture/` — narrative designs

- **Edited freely; reviewed like code.**
- Each file has YAML frontmatter:

  ```yaml
  ---
  title: Data flow across the CTX Rates pipeline
  last_verified: 2026-04-22
  verified_by: ash
  owners: ['@ash', '@alex']
  supersedes: []
  ---
  ```

- CI fails if `last_verified` is older than **90 days** and the file
  has been touched (git blame) with content changes since then.

### `docs/adr/` — Architecture Decision Records

Immutable, append-only. Each ADR is numbered, dated, and one of three
statuses: **Proposed**, **Accepted**, **Superseded**.

Template (bundled in `docs/adr/README.md`):

```markdown
# ADR-NNNN: <decision title>

**Status:** Accepted (2026-MM-DD)
**Decision makers:** @ash, @alex
**Supersedes:** (link or "none")
**Superseded by:** (filled when superseded; otherwise leave blank)

## Context

(What problem are we solving? What constraints matter?)

## Decision

(What we decided, in one paragraph.)

## Consequences

(What does this unlock? What does it cost? What future decisions
does it constrain?)

## Alternatives considered

(What we rejected, briefly, with why.)
```

**Rule:** an ADR is never edited for content. If a decision is
revisited, a new ADR supersedes the old one. The old ADR gets
exactly one edit — `Superseded by: ADR-NNNN` in its metadata.

### `docs/reference/` — auto-generated

- **`reference/api/`** from `openapi/ctx-rates.v1.yaml`. Build step:
  `make docs-api`.
- **`reference/metrics/`** by scraping Prometheus' `/metrics`
  endpoint of a running instance into a machine-readable table.
- **`reference/config/`** from Go struct tags on our `config.Config`
  type. Build step: `make docs-config`.

These directories have a banner at the top saying **"Generated file
— do not edit. Regenerate with `make docs-<name>`."** The CI
workflow regenerates them on every release to catch drift.

### `docs/operations/` — runbooks

Same frontmatter as `docs/architecture/`. Additional rule: every
runbook references a specific alert or SLO. Alerts in our
Prometheus config link back to the runbook URL. Reverse link is
checked in CI (if an alert mentions `runbooks/xyz.md`, that file
must exist).

### `docs/development/`

- `getting-started.md` — `make dev` and you're running.
- `testing.md` — how to run unit / integration / load tests.
- `contributing-a-source.md` — step-by-step for new connectors
  (concrete example: how to add Phoenix if it didn't exist).

### `docs/_archive/` — superseded docs

When a doc is outdated but has historical value, **move** (not
delete) it under `docs/_archive/`. Filename prefix: `YYYY-MM-DD-`.
CI does not check staleness for archive files.

**`docs/_archive/discovery/`** receives a snapshot of
`docs/discovery/` (this directory) when we cut over to Phase 2.
Frozen; not expected to track current state. Value is historical —
future team members can see the reasoning from Phase 1.

### `README.md` per package

Every `internal/*/` and every `cmd/*/` package has a `README.md`
describing:

1. What it does (one paragraph).
2. Its public types / functions.
3. Dependencies it assumes.
4. Known limitations / open items.

CI check: for every `internal/*/*.go` file modified in a PR, the
sibling `README.md`'s `last_verified` must be current or the PR
gets a "docs drift" label (warn, not block — we don't want to
require a README touch on every line change).

---

## 6. Doc staleness — concrete enforcement

This section is the direct answer to the user's ask. Five mechanisms:

### 6.1. Frontmatter with verification date

Every edited `docs/*.md` has:

```yaml
---
last_verified: 2026-04-22
verified_by: <handle>
---
```

CI script (`scripts/ci/check-doc-freshness.sh`) scans all docs,
computes the delta against today, and reports anything > 90 days
as a warning in the PR comment.

### 6.2. Git-hook `doc-code-link` check

Audit docs that cite code paths/lines (e.g.
`[trades.go:55-57](…)`) run through a linter that verifies the
file still exists at that path and the cited lines still contain
what the doc claims. Uses a simple `git grep` pattern + line-range
verification. We inherit this pattern from
[adversarial-audit.md §11](adversarial-audit.md) — stability of
citations is what made that section believable.

### 6.3. Release-gate doc regeneration

On every tagged release, `make docs-all` regenerates
`docs/reference/` from code. If the output differs from what's
committed, the release fails until someone commits the regenerated
output. This catches "I added a config field but didn't update the
config reference" silently forever.

### 6.4. Explicit archival

Moving a doc to `docs/_archive/` is a **deliberate act** with a
PR, not a delete. Discovery docs that stop being current get
archived with a banner explaining why:

```markdown
> **Archived 2026-05-15.** This doc described the pre-Phase-2
> data-flow sketch. Superseded by `docs/architecture/data-flow.md`.
> Kept for historical context.
```

### 6.5. Quarterly doc review

One calendar-day-per-quarter: someone walks every non-generated
`docs/` file. For each, they either:

1. Refresh it + bump `last_verified`.
2. Archive it.
3. Delete it (rare — only if genuinely useless).

Result is committed as one PR per quarter. This is the SRE
"doc hygiene" sweep — explicit, calendared, non-optional.

---

## 7. CI/CD pipeline

Five GitHub Actions workflows. All concurrent-safe.

### 7.1. `ci.yml` — runs on every PR + every push to main

```
┌─────────────────────────────┐
│  go fmt check               │
│  go vet                     │
│  golangci-lint run          │   (strict config, see §8)
│  go test ./... -race        │   (unit tests only, < 2 min)
│  goarchlint                 │   (dependency-layer enforcement)
│  check-doc-freshness.sh     │
│  doc-code-link check        │
│  openapi-lint (spectral)    │
└─────────────────────────────┘
```

Path filters: a PR touching only `docs/_archive/` or `CHANGELOG.md`
skips lint/test and runs only the doc checks.

### 7.2. `integration.yml` — PRs labeled `ready-for-integration` + nightly

Spins up Postgres + TimescaleDB + Redis + MinIO in containers
(Docker Compose), runs `go test -tags=integration ./test/integration/...`.
Includes captive-core-based fixture replays via our golden-file
corpus.

### 7.3. `security.yml` — weekly + on-dependency-bump

- `govulncheck`
- `gosec`
- `trivy` scan of Docker images
- `syft` SBOM generation (uploaded as artifact)

Dependabot auto-opens PRs for Go + Docker base image updates.

### 7.4. `release.yml` — on tag push `v*`

- GoReleaser builds multi-arch binaries (linux amd64/arm64, darwin
  amd64/arm64).
- Docker multi-arch images pushed to `ghcr.io/ctx/ratesengine`.
- Debian packages for Ubuntu LTS published.
- Signs artifacts with Sigstore/cosign.
- Creates GitHub release with changelog extract.
- Updates `docs/reference/` via `make docs-all`, commits if delta.

### 7.5. `docs.yml` — on merge to main if `docs/**` changed

Builds the static site (Hugo) and deploys to
`docs.ratesengine.ctx.io`. OpenAPI spec rendered via Redoc.

---

## 8. Lint + style

`.golangci.yml` with:

```yaml
linters:
  enable:
    - govet
    - gofmt
    - goimports
    - errcheck
    - staticcheck
    - gosec
    - gosimple
    - ineffassign
    - unparam
    - unused
    - bodyclose
    - contextcheck
    - errorlint
    - nilerr
    - noctx
    - rowserrcheck
    - sqlclosecheck
    - wastedassign
```

No `gochecknoglobals` — we have legitimate package-level
registries (sources, metrics). No `funlen` — enforced via code
review.

**Formatting:** `gofumpt` (stricter than gofmt, prevents bikeshedding).

**Import order:** `goimports -local github.com/ctx/ratesengine` —
stdlib → third-party → our packages.

**Error wrapping:** always `fmt.Errorf("%w", err)`, never `%s` or
`%v` on errors that cross a package boundary. `errorlint` enforces.

**No `interface{}` / `any` in public APIs.** If it's ambiguous
enough to need one, it's not ready for `pkg/`.

**No global `init()` state** beyond pure constant setup. Tests
break on hidden global state.

---

## 9. Testing

### Unit — co-located, fast

`internal/canonical/trade_test.go`, etc. Run on every PR.

Target coverage per package:

- `internal/canonical/` — 95 %
- `internal/aggregate/` — 90 %
- `internal/supply/` — 90 %
- `internal/sources/*` — 80 %
- `internal/api/` — 80 %
- `internal/storage/*` — 70 %
- `cmd/*` — exempt (integration-tested instead)

Enforced via codecov threshold in CI.

### Integration — `test/integration/`, behind `// +build integration`

- Testcontainers-Go for real Postgres + Redis + MinIO.
- Golden-file replays using `test/fixtures/` (ported subset from
  `stellar-etl/testdata/`).
- Fault injection: broken source, stalled source, diverged source.

### Property-based tests for i128 math

`internal/canonical/amount_test.go` uses `gopter` or `testing/quick`
with generator over `big.Int` — round-trip via JSON, Postgres
NUMERIC, XDR, CanonicalAmount → CanonicalAmount must be identity.

Mandatory fixtures for the i128 regression (per
[decisions.md](decisions.md)):

- Amount = i64 max (`9_223_372_036_854_775_807`).
- Amount = i64 max + 1.
- Amount = sign-bit-set negative i128.
- Amount = exactly 2^127 - 1 (u128 max for positive).
- The KALIEN-incident amount (`40_000_005_972_900_000_000`).

### Load — `test/load/` with `k6`

Scenarios:

- `api_steady_state.js` — 1000 req/min per key, 100 keys, 30 min.
- `api_ramp_to_saturation.js` — linear ramp until 5xx > 0.5 %.
- `api_spike.js` — 10× burst for 30 s, recover < 60 s.
- `ingest_peak_ledger.js` — 5× normal event rate for 1 h.

Pass criteria: p95 ≤ 200 ms, p99 ≤ 500 ms, error < 0.1 % across all
four.

Run pre-release, pre-SLA-validation (proposal Phase 6).

### Chaos — `test/chaos/`

Pumba / Chaos Mesh scenarios:

- Kill primary Postgres; verify replica promotion within 30 s.
- Network-partition Redis; verify API degrades (fallback to
  Postgres for hot reads).
- MinIO node failure; verify erasure-coding continues serving.
- stellar-core peer disconnect; verify failover to secondary.
- Kill API pod mid-stream; verify reconnect-with-cursor works.

Run pre-release, quarterly thereafter.

---

## 10. Versioning + changelog + release

### Two version schemes

1. **`pkg/*` SemVer** — on `pkg/types` and `pkg/client`. Promised
   API-compat within a major. Breaking change = new major.
2. **Binary CalVer** — `ctx-rates 2026.06.15.1` etc. Easier for
   operators to reason about "what we shipped when."

### Changelog

`CHANGELOG.md` kept manually, [Keep a Changelog](https://keepachangelog.com)
format:

```markdown
# Changelog

## [Unreleased]

### Added
- New Phoenix DEX indexer (#123)

### Changed
- Reflector integration now reads event-stream instead of polling (#125)

### Fixed
- i128 truncation edge case on negative values (#118)

## [2026.06.15.1] - 2026-06-15
...
```

Every PR updates `[Unreleased]`. Release workflow moves the
`[Unreleased]` block under the new version header.

### ADRs and changelog

The **changelog is for operators** — "what changed." The **ADR log
is for architects** — "why." Don't conflate. A release-note entry
may link to the relevant ADR for depth.

### Release cadence

- **Patch** (bugfix) — as needed.
- **Minor** — every 2–4 weeks once we ship v1.
- **Major** — only on breaking change to `pkg/types` or `/v1` API.
- **Pre-v1** — `0.x.y`, breaking changes allowed on minor bumps, no
  SLA commitment.

### Stellar-protocol compatibility notes

Every release's changelog includes:

```
**Tested against:** Stellar protocol 25.x (network passphrase
  "Public Global Stellar Network ; September 2015"),
  stellar-core v26.0.1, stellar-rpc v26.0.0, stellar-galexie v26.0.0.
```

When a new Stellar protocol lands, we test before advertising
compat. Minor release of ours bumps the tested-against line.

---

## 11. Security

### Dependency management

- Dependabot on Go modules, GitHub Actions, Docker base images.
- Weekly `govulncheck` in `security.yml`.
- No direct dependency on a module with zero stars + <1 yr history
  unless justified in an ADR.
- `go.sum` checked in; module checksum verified.

### Secret handling

- **No secrets in the repo.** Ever. `.gitignore` blocks `*.env`,
  `credentials*.json`, `*.key`, `*.pem`.
- `.github/workflows/` uses GitHub encrypted secrets only.
- Pre-commit hook (optional dev convenience) runs `gitleaks` /
  `detect-secrets` locally.
- CI runs `gitleaks` on every PR.
- Runtime secrets delivered via env vars, injected from our secret
  manager (Vault, AWS Secrets Manager, or similar — decided in
  infrastructure doc round).

### Signing

- Commits should be signed (`git commit -S`). Branch protection
  enforces.
- Release artifacts signed with Sigstore/cosign.
- Docker images published with provenance attestations.

### Reproducible builds

- `-trimpath` + `-buildvcs=true` in `go build`.
- Goreleaser builds with locked toolchain version.
- SBOM (syft) shipped with every release.

### Vulnerability disclosure

`SECURITY.md` at repo root describes: how to report
(`security@ratesengine.ctx.io` + GPG key), 90-day embargo,
hall-of-fame policy for researchers.

---

## 12. CODEOWNERS

```
# .github/CODEOWNERS

*                        @ash
/internal/sources/       @ash @indexer-team
/internal/aggregate/     @ash
/internal/api/           @ash @api-team
/internal/storage/       @ash @infra-team
/deploy/                 @ash @infra-team
/docs/adr/               @ash
/openapi/                @ash @api-team
/pkg/                    @ash
```

Start with @ash owning everything; add team handles as we hire.
All ADRs require @ash review (nothing lands without an architect
signing off on architectural decisions).

---

## 13. Developer experience

### `make` targets (run `make help`)

```
make dev            # docker-compose up full stack
make dev-teardown   # down + volumes
make dev-seed       # load fixture data into local stack

make test           # unit tests
make test-integration  # integration tests (requires `make dev`)
make test-load      # k6 load tests
make test-all       # everything

make lint           # golangci-lint
make fmt            # gofumpt + goimports
make vet            # go vet

make build          # all binaries into bin/
make build-docker   # all images locally

make docs           # static site build
make docs-serve     # local preview on :8080
make docs-api       # regenerate openapi → docs/reference/api/
make docs-config    # regenerate config ref
make docs-all       # all generated docs

make db-migrate-up   # apply pending migrations
make db-migrate-down # revert one
make db-migrate-status

make release-dryrun # goreleaser dryrun
```

### Devcontainer

`.devcontainer/devcontainer.json` provides a VS Code / Codespace
environment with Go, soroban-cli, k6, Docker-in-Docker, plus all
Makefile deps pre-installed. Lowers "I can't reproduce your env"
friction for contributors and new hires.

### Pre-commit hook (opt-in)

`scripts/dev/install-hooks.sh` installs:

- `gofumpt -w` on staged Go files.
- `golangci-lint run --new-from-rev=HEAD`.
- `gitleaks protect --staged`.

Not enforced; devs can bypass with `--no-verify`. CI catches
anything that slips through.

---

## 14. Open-source release checklist

Before we flip the repo public:

- [ ] LICENSE (Apache-2.0) in root.
- [ ] CONTRIBUTING.md with contributor workflow.
- [ ] CODE_OF_CONDUCT.md (Contributor Covenant).
- [ ] SECURITY.md with disclosure process.
- [ ] README.md with quickstart, architecture diagram, status badges.
- [ ] No secrets ever in git history (check with `gitleaks` full scan).
- [ ] No internal-only hostnames / URLs hardcoded.
- [ ] Trademark policy if we register "CTX Rates" as a mark.
- [ ] A published Docker image at `ghcr.io/ctx/ratesengine:<ver>`.
- [ ] Self-hosted quickstart works from a clean machine
      (`docker-compose up` → query API within 5 min).
- [ ] docs.ratesengine.ctx.io is live.

---

## 15. Migration from `docs/discovery/` to implementation repo

When we initialise the production repo:

### Step 1: port `docs/discovery/` → `docs/_archive/discovery/`

Verbatim copy. No edits. Timestamp the archive with a banner
at `docs/_archive/discovery/README.md`:

```markdown
# Phase 1 discovery archive (frozen 2026-MM-DD)

This directory is a point-in-time snapshot of our Phase 1 discovery
work. It is **frozen**. Every audit doc here was verified against
the dependency versions pinned in VERSIONS.md (at the time of
archival).

For current designs, see /docs/architecture/ and /docs/adr/.
For current dep pins, see /VERSIONS.md at repo root.
```

### Step 2: extract durable decisions into ADRs

Each entry in our current `decisions.md` becomes a numbered ADR:

- `decisions.md#Horizon` → `docs/adr/0001-horizon-deprecated.md`
- `decisions.md#MinIO` → `docs/adr/0002-minio-s3-compat-storage.md`
- `decisions.md#i128` → `docs/adr/0003-i128-no-truncation.md`
- `decisions.md#Tier-1` → `docs/adr/0004-tier1-validator-aspiration.md`

Plus one new one we've accumulated:

- `docs/adr/0005-monorepo.md` — the decision captured in §1 of this
  doc.

### Step 3: promote relevant audit docs into `docs/architecture/` or `docs/reference/`

Candidates:

- `protocol-versions.md` → `docs/architecture/protocol-versions.md`.
- `rfp-requirements-matrix.md` → `docs/operations/rfp-compliance.md`
  (we track RFP compliance forever, not just Phase 1).
- `adversarial-audit.md` → `docs/_archive/` (Phase 1 artefact).
- Per-source audit docs (oracle / DEX) → **keep as reference, move
  to `docs/architecture/sources/<name>.md`** because they describe
  the on-chain surface we build against — that surface doesn't
  change just because we entered Phase 2.

### Step 4: establish `VERSIONS.md` at repo root

Move `docs/discovery/VERSIONS.md` → root `/VERSIONS.md`. Refresh
dates, re-pull SHAs, re-run the verification.

### Step 5: write the first production code

With the structure in place, the first code lands somewhere small
and concrete — probably `internal/canonical/` (the types that
everything else depends on). That PR also lands `Makefile`,
`.golangci.yml`, `.github/workflows/ci.yml`, and the first ADR
(`0005-monorepo.md`). Everything downstream depends on the
structure this PR creates.

---

## 16. What we reject

To make the decisions sharp:

- ❌ **Multi-module monorepo.** Complexity tax, no benefit.
- ❌ **`cmd/` with one binary per feature.** We have four
  binaries: indexer, aggregator, api, ops-cli. Not per-feature
  splits.
- ❌ **Hand-edited API docs.** Generated from OpenAPI only.
- ❌ **Hand-edited config docs.** Generated from struct tags only.
- ❌ **Separate docs repo.** Docs-as-code, same repo as source.
- ❌ **Copying `cdp-pipeline-workflow` patterns.** See its audit;
  we explicitly avoid their factory-over-JSON-strings pattern.
- ❌ **`interface{}` / `any` in `pkg/types`.** Strongly typed or
  it stays in `internal/`.
- ❌ **Running in Kubernetes for the colo tier.** SDF explicitly
  discourages k8s for validators (see
  [data-sources/archival-nodes.md](data-sources/archival-nodes.md));
  we follow. k8s manifests exist for self-hosted operators who
  want them.
- ❌ **`make install` that writes into `/usr/local/bin`.** Binaries
  install via Debian packages or Docker images only. `make build`
  puts them in `bin/` in the working tree.

---

## 17. Open items for infra + API rounds

Things this plan deliberately **does not** decide, left for the
next rounds:

- **Which hosting provider for cloud tier.** Infrastructure round.
- **Which secret manager.** Infrastructure round.
- **Which static-site generator for docs.** Options: Hugo, MkDocs,
  Docusaurus. Pick during infrastructure round.
- **Which observability backend.** Prometheus + Grafana is locked;
  logs backend (Loki vs ELK vs managed) is infrastructure round.
- **API versioning strategy beyond `/v1`.** API round.
- **OpenAPI vs GraphQL vs both.** API round.
- **SDK languages beyond Go.** API round. JS at minimum;
  possibly Python + Rust if there's demand.
- **Commit-signing key-management for the team.** Not blocking
  v0 but needs to land before team grows.

Each of these is a one-decision doc; they slot into the
`docs/adr/` sequence as we make them.

---

## 18. Timeline to apply this plan

- **Day 1–2:** Initialise repo with this layout, port `docs/discovery/`
  to `docs/_archive/`, write the five ADRs, land the first CI workflow.
- **Day 3:** Infrastructure design round starts; docs land in
  `docs/architecture/infrastructure/`.
- **Day 4–5:** API spec round; `openapi/ctx-rates.v1.yaml` + generated
  reference lands.
- **Day 6+:** Phase 2 code (the actual 10-week delivery clock).

This plan is ~1-day of architectural work to execute faithfully.
Worth it to land zero technical-debt-from-day-one.
