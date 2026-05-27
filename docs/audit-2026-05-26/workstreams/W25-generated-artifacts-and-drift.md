# W25 — Generated artifacts + drift

## Scope

Every artifact produced by a script or build step rather than
hand-edited. Drift between generator + checked-in output is its
own failure mode.

## Inputs

- `openapi/rates-engine.v1.yaml`
- `docs/reference/{api,config,metrics}/`
- `examples/postman/*.json`
- `examples/curl/*.sh`
- `pkg/client/{types,endpoints}.go`
- `web/*/out/` static exports
- `go.sum`, `pnpm-lock.yaml` (per frontend)
- `docs/audit-2026-05-26/inventory/*` (this audit's own
  generated files)

## Checks

| # | Check | Method |
| --- | --- | --- |
| W25.1 | OpenAPI regeneration drift: handlers add/remove routes; openapi reflects | `make docs-api` then diff |
| W25.2 | docs/reference/{api,config,metrics}/ regenerated | `make docs-all` then diff |
| W25.3 | Postman + curl examples drift vs current routes | per-file |
| W25.4 | pkg/client drift vs OpenAPI | per-file |
| W25.5 | web/*/out/ static-export shape vs API contract | inspection |
| W25.6 | go.sum integrity | `go mod verify` |
| W25.7 | pnpm-lock per frontend integrity | shell |
| W25.8 | This audit's inventory/ regenerable by `inventory/generate.sh` | shell |
| W25.9 | NEW: every per-source backfill subcommand has matching CHANGELOG entry | grep |

## Closure criteria

Every artifact has terminal status. Findings on drift.
