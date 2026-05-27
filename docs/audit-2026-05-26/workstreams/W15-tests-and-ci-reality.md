# W15 — Tests, CI reality, regression confidence

## Scope

What is actually proven vs what we claim is proven.

## Inputs

- `git ls-files '*_test.go'` (387 test files)
- `test/integration/` (28 integration test files)
- `test/chaos/`
- `test/load/`
- `test/fixtures/`
- `.github/workflows/ci.yml`

## Checks

| # | Check | Method |
| --- | --- | --- |
| W15.1 | `make test` clean (`go test -race ./...`) | shell |
| W15.2 | `make test-integration` clean (testcontainers-go path) | shell + docker |
| W15.3 | per-package test density vs risk | inventory pass |
| W15.4 | Per-source: malformed-input test present (cross-ref W07 check 3) | grep |
| W15.5 | k6 weekly: cron + perf budgets vs measured | workflow + reports |
| W15.6 | Chaos scenarios: every scenario exercises one named failure mode | per-scenario |
| W15.7 | Linter parity local vs CI (gofumpt, golangci-lint versions) | shell + workflow |
| W15.8 | CI gating: `go test -race -coverprofile` is the gating job? | workflow inspection |
| W15.9 | NEW: 28 integration tests up from 19 at baseline — verify each new one is wired into CI | per-test |
| W15.10 | NEW: every new package (cctp, rozo, sorobanevents, etc.) has integration test exercising real Timescale path | per-source |
| W15.11 | NEW: `internal/sources/sorobanevents/dispatcher_adapter_test.go` regression-tests back-pressure (W28 cross-ref) | test inspection |
| W15.12 | NEW: `internal/ledgerstream/trailing_edge_*_test.go` regression-tests trailing-edge (W28) | test inspection |
| W15.13 | NEW: `internal/sources/sorobanevents/reconstruct_test.go` round-trips Capture | test inspection |

## Closure criteria

Per-package status terminal. Findings on:
- happy-path-only packages
- integration tests existing but not in CI
- chaos scenarios with no recent run
