---
name: add-endpoint
description: Add or change a Stellar Index API endpoint — handler + Options wiring + OpenAPI + the three generated artifacts + SDK triage + caching/consistency contracts. Use when adding a route, changing a response shape, or when the spec/SDK/explorer drift after an API change.
---

# /add-endpoint

Canonical checklist: `docs/contributing/add-api-endpoint.md`. This
skill adds the contract-gate ecosystem that now surrounds an
endpoint change.

## 1. Design decisions FIRST

- **Consistency surface (ADR-0018):** closed-bucket, tip, or
  observations — the URL is the contract; query params must never
  select a tier (`lint-openapi-urls` rejects it).
- **Cache policy:** add the route to
  `middleware/cachecontrol.go::policyForPath` deliberately (more-
  specific prefix wins; `/v1/price/tip` vs `/v1/price` ordering).
  Problem responses MUST set `no-store` — use the existing writers;
  a new problem-writer must follow cachecontrol.go's invariant doc.
- **Wire types:** decimal STRINGS for money (ADR-0003), envelope
  `{data, as_of, sources?, flags, pagination?}`, RFC 9457 errors.
- **Fan-out:** per-row DB reads use `forEachBounded` (cap 16) —
  never a bare WaitGroup-per-row.

## 2. The change set (all in ONE commit)

1. Handler in `internal/api/v1/` + reader interface on `Options`
   (nil-safe degradation documented on the field) + wiring in
   `cmd/stellarindex-api/main.go`.
2. `openapi/stellar-index.v1.yaml` path + schemas.
3. Regenerate ALL THREE artifacts (two have silently drifted before):
   `make docs-api && make docs-postman && make web-generate-api`.
4. SDK triage — the contract test FORCES this: either add the
   `pkg/client` method + a `coveredOperations` row, or add the
   route to `uncoveredOperations` with a reason. If covered, the
   payload struct's json tags must match the spec's data schema
   bidirectionally.
5. Prewarm: if the endpoint gets a prewarm goroutine, it must call
   the cached reader with BYTE-IDENTICAL args to the handler (three
   real bugs from drifted Order/Sources/Limit dimensions).
6. CHANGELOG under [Unreleased]; API minor bump if additive.

## 3. Checks

```sh
go test ./internal/api/v1/ ./pkg/client/
bash scripts/ci/lint-docs.sh          # route↔spec bidirectional
go run ./scripts/ci/lint-openapi-urls openapi/stellar-index.v1.yaml
npx --yes @stoplight/spectral-cli lint openapi/stellar-index.v1.yaml
cd web/explorer && pnpm typecheck     # generated types feed the UI now
```

Then curl the endpoint (local stack or read-only prod) and READ the
payload — including one error case (bad param → problem+json with
`no-store`).

Finish with **/verify-done**.
