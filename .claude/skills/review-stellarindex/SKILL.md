---
name: review-stellarindex
description: Adversarial code-review checklists for Stellar Index diffs, distilled from the repo's real incident corpus (F-####/CS-### findings) — per-subsystem failure modes a generic review misses. Use when reviewing a diff/PR touching pricing SQL, ingest/decoders, API handlers, the explorer, or ops/config.
---

# /review-stellarindex

Generic review finds generic bugs. Each check below is a failure
mode THIS repo has actually shipped — review the diff against the
sections its files touch. Every check cites its incident so you can
read the original when unsure.

## Pricing / SQL (`internal/storage/timescale`, `internal/aggregate`, migrations)

- **Sargability:** any function-of-an-indexed-column in WHERE
  (`bucket + INTERVAL <= now()`) forces a full per-chunk scan —
  rewrite as `col <= now() - INTERVAL` (p95 50→400ms incident,
  446ms→7ms fix). `ORDER BY … LIMIT` siblings are fine.
- **Closed-bucket contract (ADR-0015):** serving paths must never
  read the in-progress bucket; "tip" surfaces are the deliberate
  exception with their own URL.
- **Per-source decimals:** on-chain = per-asset (XLM 7), CEX/agg =
  1e8, FX = 1e6. Any hardcoded exponent is a bug candidate (CS-040
  was ~100× off).
- **Exact math:** money is `big.Int`/`big.Rat`/NUMERIC/string —
  a float or int64 touching an amount is a stop-the-review finding
  (ADR-0003). New CAGGs: ratio = single division
  `sum(q)/sum(b)` (migrations/README rule 8).
- **Retention claims:** trades/prices retention was REMOVED
  (migration 0031) — any code reasoning about a 90d window is drift.
- **Migrations:** NUMERIC not BIGINT for amounts; per-event PK
  discriminator (the coarse-PK class collapsed distinct sub-op
  swaps); never edit an applied migration.

## Ingest / decoders (`internal/sources`, `dispatcher`, `pipeline`, `projector`)

- **The five lockstep sites** — `go test -run TestLockstep
  ./internal/pipeline/` must be green; a new event type without a
  persist arm is silent loss (F-1316).
- **Gating:** new `Matches()` on topic bytes alone is forbidden
  (ADR-0035/0040) — demand the contract-identity gate + a
  foreign-contract reject test.
- **Schema-evolution safety:** decode Map-by-field-NAME, dispatch on
  topic[0] symbol, type-test before `MustI128()` (SEP-41 transfer
  data is i128 OR map). Fixtures per WASM hash.
- **Multi-event ops:** identity must include event index / fanout
  stride (Phoenix 8-per-swap collapse; reflector's
  `opIndexFanoutStride`).
- **RPC:** any `rpc.GetEvents`/`stellarrpc` import in production
  ingest is wrong, full stop (removed from r1 2026-04-23).
- **Cursor/durability claims:** the cursor is a resume hint; any
  diff that weakens the verdict's ability to see a hole violates
  ADR-0041.

## API (`internal/api/v1`, `pkg/client`, openapi)

- **Cacheability of denials:** every problem writer sets `no-store`
  BEFORE WriteHeader (the 401-cacheable-as-public class, 2026-07-02);
  cachecontrol.go's invariant doc lists them all.
- **Bounded fan-out:** per-row DB goroutines use `forEachBounded` —
  a bare WaitGroup-per-row is a pool-exhaustion vector.
- **Contract sync:** spec touched → all three generators re-run;
  pkg/client contract test green; explorer tsc green. A response
  field added in Go but not the spec now fails machines, not
  reviewers.
- **Prewarm arg identity:** prewarm must call the cached reader with
  byte-identical args to the handler (three real cache-miss bugs).
- **Consistency tier by URL only** (ADR-0018); error triage ladder
  (`clientAborted → handlerTimedOut → transientStorageErr → 500`)
  preserved in new handlers.

## Explorer (`web/explorer`)

- **JSON-LD:** script-tag injection uses `serializeJsonLd`, never
  raw `JSON.stringify` (stored-XSS via SEP-1 ORG_NAME, cc9fe451).
- **Types:** wire shapes derive from `src/api/types.ts` aliases —
  a new hand-written interface for an API payload is regression;
  `// SPEC-GAP` intersections need a spec-side follow-up.
- **Null honesty:** narrow with `??`/optional chaining, no `!` or
  `as` casts on wire data; design-system `Table`/`Callout` over
  hand-rolled markup.
- **Build-time fetches:** go through the fail-hard build fetch
  layer — a page-local fetch fallback that emits placeholder HTML
  reintroduces the baked-"Asset not found" incident class.

## Ops / config / CI (`configs/`, `deploy/`, `.github/`, `scripts/`)

- **Both rule trees or neither** — the equivalence differ enforces;
  baseline growth needs a `Baseline-Growth:` trailer (CS-098).
- **Jinja string-truthiness:** any `when: <var>` on a value that
  arrives as a STRING needs `| bool` (the migrations_skip class,
  F-1220).
- **Config fields:** tag default ↔ `Default()` lockstep (F-1327
  test), `make docs-config` regenerated, KnownSources updated for
  new sources.
- **Gates that can't fail are decorative:** a new lint/alert/guard
  in the diff should come with evidence it was probe-tested.
- **Shell in CI:** exit codes checked directly — never through a
  pipe (`verify.sh | tail` reports tail's exit).

## Verdict style

Report findings with file:line + the concrete failing scenario, most
severe first; name what you verified as GOOD (prevents
re-litigation) — the audit corpus format in docs/audit-2026-06-30 is
the model.
