# W04 — Dependency, provenance, supply chain

## Scope

`go.mod` direct deps + transitive surface; `VERSIONS.md` pins;
GitHub Actions deps; web frontends' pnpm lockfiles; Dockerfile
base-image pins.

## Inputs

- `go.mod`, `go.sum`, `VERSIONS.md`
- `go list -m all`
- `.github/workflows/*.yml` action refs
- `web/{explorer,dashboard,status}/pnpm-lock.yaml`
- `docker/*.Dockerfile`
- `.discovery-repos/` (verify NEVER imported in product code)

## Checks

| # | Check | Method |
| --- | --- | --- |
| W04.1 | Every direct dep in `go.mod`: licence, maintenance, last release, known CVEs | dep-by-dep |
| W04.2 | Transitive surface: `go list -m all` (any concerning libs?) | inspection |
| W04.3 | `VERSIONS.md` pins are real SHAs that resolve | per-line check |
| W04.4 | `go.sum` integrity: `go mod verify` clean | shell |
| W04.5 | `govulncheck` runs in CI | workflow check |
| W04.6 | `gitleaks` runs in CI; `.gitleaks.toml` rule coverage | scan |
| W04.7 | `.discovery-repos/` is gitignored AND not imported in `internal/`, `pkg/`, `cmd/`, `web/` | grep |
| W04.8 | web pnpm lockfiles: per-package advisories | `pnpm audit` |
| W04.9 | Dockerfile base images pinned by SHA, not by tag | grep `FROM` |
| W04.10 | GitHub Actions refs: every action pinned by SHA (per `lint-actions-pinning.sh`) | grep workflows |
| W04.11 | NEW chainlink + sorobanevents deps don't introduce concerning transitives | go.sum diff |
| W04.12 | dependabot config: opens PRs for security updates promptly | `.github/dependabot.yml` |

## Closure criteria

Every dep tier audited. Findings on unmaintained deps, missing
pins, CVE matches.
