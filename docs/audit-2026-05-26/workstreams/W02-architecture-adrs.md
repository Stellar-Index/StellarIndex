# W02 — Architecture, ADRs, negative space

## Scope

Every architectural claim vs implementation. ADR-0001..0029
(three new since baseline: 0027 LCM cache tiering, 0028 RWA
asset representation, 0029 soroban_events landing zone).

Plus all `docs/architecture/` notes. Plus the package-graph
import-boundary lint at `scripts/ci/lint-imports.sh`.

## Inputs

- `docs/adr/*.md` (29 ADRs + README + _template)
- `docs/architecture/*.md`
- `scripts/ci/lint-imports.sh` + baseline
- `go list -deps ./...`

## Checks

| # | Check | Method |
| --- | --- | --- |
| W02.1..W02.29 | Each ADR-NNNN's invariant is honoured by current code OR marked stale | per-ADR audit (see 04-reconciliation R01) |
| W02.30 | Architecture docs match `internal/` reality (R02 row-by-row) | per-doc audit |
| W02.31 | `lint-imports.sh` rules block new violations; baseline is a closed register | `scripts/ci/lint-imports.sh` execution |
| W02.32 | New packages since 2026-05-12: `internal/customerwebhook`, `internal/currency/marketcap`, supporting modules — verify wired + tested | grep + test inspection |
| W02.33 | Legacy / dead packages (`internal/consumer`, `internal/stellarrpc`) — wired anywhere that matters? Per CLAUDE.md: stellarrpc is rpc-probe + fixture-capture only | grep import-graph |
| W02.34 | ADR drift register: ADR-0027 cold-tier text matches `seamed.go`/`tiered.go` (W30); ADR-0028 RWA matches `asset_rwa.go` (W31); ADR-0029 soroban_events matches `sorobanevents` package (W27) | per-ADR cross-ref |
| W02.35 | ADR numbering: every NNNN allocated to a real ADR (no skipped numbers; or skipped numbers explained) | `ls docs/adr/` |

## Closure criteria

Every ADR has terminal status. Findings on any drift, contradiction,
or stale invariant.
