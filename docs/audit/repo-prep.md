# Repo-prep: stellarindex — cached facts for audit/remediation agents

Derived 2026-07-16 against commit `f84e2d0b` (main == origin/main, tree clean).
Read this BEFORE fixing, verifying, or integrating anything. It exists so
remediation PRs pass CI on the first push.

## What this repo is (stakes)

Go monorepo: Stellar protocol explorer + **public pricing API**
(stellarindex.io). ClickHouse raw lake + TimescaleDB served tier + Redis.
Money-adjacent: served prices/amounts are the product — i128/NUMERIC
correctness, VWAP/aggregation math, and completeness verdicts are the
"money" surface. Treat `internal/aggregate`, `internal/canonical`,
`internal/supply`, `internal/auth`, `internal/ratelimit`, `migrations/`,
`internal/storage` as protected change-classes (strongest verifier panel).

## ⚠️ Live-ops constraints for THIS campaign

- **DEPLOY FREEZE.** R1 is running the ADR-0047 Phase-0 backfill
  (`ledger_entry_changes`, ledgers 38,115,806 → 61,999,000). A deploy
  restarts services and swaps the ops binary Phase 0 is using.
  **Never**: cut a tag/release, run `deploy.yml`, `cut-release.sh`, any
  ansible playbook against r1, or any ssh/ops command on r1. Code-only;
  merge to main is allowed. 11 commits already sit undeployed since v0.16.3.
- api-audit.yml fires on push-to-main when `openapi/**` or `internal/api/**`
  change and audits the **live deployed** API against the merged spec. Any
  spec change describing not-yet-deployed behavior will red it until the
  post-Phase-0 deploy. Avoid spec/example changes that diverge from the
  deployed API; if unavoidable, expect and annotate the red.

## Worktrees + dependency seeding

- Worktrees go INSIDE this repo: `/Users/ash/code/stellarindex/.claude/worktrees/<name>`
  (dir exists). Base them on synced main (`f84e2d0b`) — already synced.
- **Go:** single module; the module cache is machine-global — worktrees need
  NO dep seeding for Go. Tools live in `$(go env GOPATH)/bin`
  (gofumpt, goimports, golangci-lint, govulncheck all installed); the
  Makefile calls them via `$(GOBIN)/…` so PATH doesn't matter for `make`.
- **Web (pnpm v10, node 20; Next 16.2.10 / React 19 — NOT Next 15 as some docs say):** `web/explorer` and `web/status` each have
  `node_modules` present in the main checkout. In a worktree, symlink before
  any web gate:
  `ln -s /Users/ash/code/stellarindex/web/explorer/node_modules <wt>/web/explorer/node_modules`
  and the same for `web/status`. (`pnpm install --frozen-lockfile` also works
  but is slower.)

## Gate / verify commands

- **Canonical full gate:** `make verify` = govulncheck + `scripts/dev/verify.sh`
  (fmt → vet → lint → lint-docs → lint-imports → protocol-registry-sync →
  lexicon → i128 → migrations-money → openapi-urls → pk-discriminators →
  rule-structure → monitoring-check (promtool ✓ installed) → gitleaks →
  unit tests `-race` → integration BUILD → explorer+status typecheck/lint/build).
  Note: it runs `make fmt` first, which MUTATES files — run on a committed tree.
  Note: verify.sh gates govulncheck on `command -v govulncheck` (PATH), which
  is NOT on PATH here — run `"$(go env GOPATH)/bin/govulncheck" ./...`
  explicitly (CI always enforces it).
- **Unit:** `make test` (`go test -race -timeout 2m ./...`). CI uses
  `-timeout 4m` because `TestI128TruncationGuard` walks the whole repo — if
  the 2m target flaps locally, use 4m; don't delete the guard.
- **Money/DB gate (the real one):** `make test-integration` — testcontainers-go
  spins its own TimescaleDB/Redis/MinIO. Prereq: Docker only (✓ installed).
  No env vars, no external services, no manual test DB. This is a **blocking
  CI job**. Proven-red DB-backed tests for money/auth/crypto/migration fixes
  go in `test/integration/` (build tag `integration`) or
  `cmd/stellarindex-ops` — remember `INT_TEST_PKGS` covers both.
- **Web only:** `make web-typecheck web-lint`, build with
  `NEXT_PUBLIC_API_BASE_URL=http://api.local-stub.invalid make web-build`
  (same for `status-*`).
- Local tool inventory: docker ✓ promtool ✓ gitleaks ✓ pnpm ✓ shellcheck ✓;
  k6 ✗ (load suite out of scope). GOBIN has the Go toolchain lints.

## Commit convention (commitlint.config.js — honor even though unenforced)

- Conventional commits, `type(scope): subject`, subject starts lowercase,
  subject ≤ 72 chars, body lines ≤ 100.
- type ∈ feat fix refactor perf test docs chore ci build revert.
- scope ∈ indexer aggregator api ops migrate canonical consumer extract
  storage auth ratelimit metadata supply obs divergence sources sdex soroswap
  aquarius phoenix comet blend reflector redstone band chainlink cex fx
  deploy ci docs deps adr infra security. **Scope is required by the enum
  style used throughout history — pick from this list.**
- Growing any `scripts/ci/*.baseline` or the `KNOWN_INERT` allowlist requires
  a `Baseline-Growth: <reason>` commit trailer (CI lint-baseline-growth reds
  otherwise). Baselines are shrink-only by default.
- FYI (audit lead): commitlint is in NO workflow, and the repo's local
  `core.hooksPath` points at `/Users/ash/code/ratesengine/.git/hooks` which
  does not exist → git runs no hooks at all locally. The config header
  claiming "runs in CI via a small action and local hook" is drift.

## CI-parity checklist — what PR CI runs that `make verify` does NOT

Run/heed these locally before pushing so PRs are green on first push:

1. **`make test-integration`** — full Docker run (verify.sh only compiles it).
2. **lint-baseline-growth** — needs a base SHA:
   `BASE_SHA=$(git merge-base origin/main HEAD) ./scripts/ci/lint-baseline-growth.sh`
3. **OpenAPI changed? Regenerate ALL THREE and commit the diffs:**
   `make docs-api && make docs-postman && make web-generate-api`
   CI exact-diffs `docs/reference/api/*`, `examples/postman/*`, and
   `web/explorer/src/api/types.ts`. (Only docs-api is covered by lint-docs
   locally; the other two have silently drifted onto main before.)
4. **Spectral lint** on `openapi/*.yaml` (CI-only; keep spec lint-clean).
5. **gitleaks history vs worktree:** CI scans full git history
   (`gitleaks detect`, fetch-depth 0); local verify scans only the working
   tree (`--no-git`). A committed-then-removed secret/fixture still reds CI —
   allowlist via `.gitleaks.toml` in the same PR that adds a hot fixture.
6. **actions-pinning lint** — only when touching `.github/workflows/`: any
   NEW third-party action must be SHA-pinned, not tag-pinned.
7. **ansible syntax + ansible-lint** — when touching `configs/ansible/`.
8. **pnpm audit --audit-level high** on both web apps (advisory;
   `ERR_PNPM_AUDIT_BAD_RESPONSE` from the registry is tolerated).
9. **release-validate.yml** fires if the PR touches `docker/**`, `Makefile`,
   `go.mod`/`go.sum`, release/deploy workflows, or `cut-release.sh`:
   cross-compiles every binary + builds every Dockerfile. A dep bump pays
   this cost and must pass it.
10. **Docs-only PRs skip ci.yml** (paths-ignore on `**.md`, `docs/**`,
    CHANGELOG). Required checks then never report — merge such PRs with the
    admin path or bundle doc changes with their code PR (preferred).
11. **Post-merge tripwires on main:** push-to-main re-runs full CI (a red run
    on main is the alarm for the admin-bypass path), and api-audit hits the
    live API when `openapi/**` / `internal/api/**` changed (see deploy-freeze
    note above).

## Migrations

- `migrations/` — golang-migrate, sequential numbering; runner is
  `cmd/stellarindex-migrate` (`make db-migrate-up/down/status`).
- Money columns must be NUMERIC (lint-migrations; escape hatch
  `-- lint-money:ok <reason>` — avoid).
- **One migration-owner per wave.** Serialize migration-generating fixes; a
  dependent migration stacks on its predecessor's branch and
  `rebase --onto` after merge. Committing a migration file ≠ running it —
  running against prod is a deploy-freeze-scoped operator action, NOT ours.

## Other repo law that reds PRs if ignored

- CHANGELOG entry under `[Unreleased]` for user-visible changes (release
  process depends on it; cite the PR).
- Every new Prometheus metric/alert: rule in BOTH `deploy/monitoring/rules/`
  and `configs/prometheus/rules.r1/` + runbook at
  `docs/operations/runbooks/<alert>.md` + entry in
  `docs/reference/metrics/README.md` (five-lint guard chain).
- Register metrics in `registerAppMetrics()`/`...Tail()`, not `init()`.
- lint-docs enforces doc freshness (180-day fail on frontmatter-dated docs),
  TODO-issue-link discipline, ADR status integrity, generated-file banners.
- Import boundaries: no Horizon anywhere; no stellar-rpc in prod ingest;
  `go-stellar-sdk/xdr` only inside `internal/scval`. i128 → `*big.Int` /
  NUMERIC / JSON string, never `int64(parts.Lo)`.
- Lexicon ratchet: no `Coin*` vocabulary, no Fetch/Make verb constructors,
  slog is the only logger.
- One writer per data domain: Soroban-derived tables are written ONLY by
  `internal/projector`; catch-up is `projector-replay`, never a bespoke
  backfill subcommand.
- PR cadence: one PR → squash-merge on green → pull main → next. Never stack
  parallel narrative PRs that share `cmd/*/main.go`, `internal/config`,
  `CHANGELOG.md`, `CLAUDE.md`.
- The repo has its own project skills in `.claude/skills/`
  (`/review-stellarindex`, `/verify-done`, `/add-metric`, …) — fixers should
  read the relevant skill file for procedure-shaped tasks.
