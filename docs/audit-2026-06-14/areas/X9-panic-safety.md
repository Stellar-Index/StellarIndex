# X9 — Panic-safety in request & ingest paths — read-only audit

Scope (cross-cutting seam): every code path where a Go `panic` could
crash a goroutine or the whole process **unrecovered**. Three sub-areas:

1. **HTTP request paths** — is `middleware.Recoverer`
   (`internal/api/v1/middleware/recoverer.go`) on every mounted route
   group (v1 pricing, explorer, dashboard auth/keys/webhooks, SEP-10,
   signup, SSE)? Special-case the **SSE producer goroutines** — a panic
   in a streaming goroutine is NOT caught by request-scoped middleware.
2. **Ingest paths** — dispatcher + decoders + projector + supply
   observers. `Must*()` accessors, `.Address()` (panics on unknown
   key-type), literal-index access on topics/args/Vec, non-comma-ok type
   assertions, nil-map writes — anything that panics on malformed /
   adversarial on-chain data. Is each ingest goroutine wrapped in a
   recover, or does one bad ledger crash the indexer?
3. **Background workers** — aggregator refresh, projector loop,
   customerwebhook drain, divergence refresh, completeness, external
   CEX/FX connectors. Does a panic take down the whole binary?

Dimension codes: **X9** (panic crashes a goroutine/process) and **D9**
(degrade-not-panic: contained to a 500 but defeats an intended
graceful-degrade path).

Date: 2026-06-15. Method: full read of the middleware stack + mux
wiring (`server.go`), all three SSE handlers + the `streaming` package,
the dispatcher decode seam (`internal/dispatcher`), the
recover-boundary wrappers (`internal/pipeline`), the projector loop
(`internal/projector`), the worker-spawn sites in all three binaries,
plus targeted reads of `internal/xdrjson`, `internal/scval`, and a
sweep of every `sources/*/decode.go`. SDK panic behaviour cross-checked
against go-stellar-sdk@v0.5.0. No source edited, no git run.

The single load-bearing fact: **`recover()` exists in exactly three
non-test production locations** —
`internal/api/v1/middleware/recoverer.go` (HTTP request goroutine),
`internal/pipeline/processor.go` + `sink.go` (per-ledger + per-event
ingest boundary), and `internal/divergence/compare.go` (per-reference
fan-out). Everything else — the projector loop, all SSE producer
goroutines, every background worker — runs in a **bare goroutine with
no recover**, so a panic there crashes the whole binary.

---

## Findings

| severity | file:line | dim | issue | why it matters | fix | confidence |
|---|---|---|---|---|---|---|
| **High** | internal/projector/projector.go:285 (Decode) + 186-189 (bare goroutine) | X9 | The projector spawns one **un-recovered** goroutine per source (`go func(src){ p.runOneSource(...) }`, no `defer recover`), and `cycleOneSource` calls `src.Decoder.Decode(ev)` directly on raw `soroban_events` / CH `contract_events` rows. The projector runs **inside the live `stellarindex-indexer` binary** (`cmd/stellarindex-indexer/main.go:479-497`). The SAME decoders run **defended** in the dispatcher path (wrapped by `pipeline.ProcessLedger`'s recover, processor.go:41) — but here there is no recover anywhere between the decoder and the process. A decoder panic on one stored row crashes the entire indexer. Worse: the cursor does NOT advance on a panicking row, so on restart the projector re-reads the **same** row → crash-loop. (`p.sink` itself IS recover-protected — it's `pipeline.HandleEvent` — so only the Decode call is the live gap.) | A malformed/upgraded-contract event already persisted in the lake (Soroban contracts upgrade in place; backfill/replay sees every prior WASM) that panics any decoder takes the live indexer down and keeps it down. The dispatcher hardened this exact seam; the projector — the ADR-0031/0032 sole-writer for Soroban-derived rows — did not inherit the guard. | Wrap the per-source goroutine body in a `defer recover()` that logs + counts (`obs.ProjectorRunsTotal{..,"panic"}`) and continues, OR wrap the `process(ev)` closure / the `Decode` call in a per-row recover so one poison row is skipped (count it; leave cursor at the prior row only if you also skip-forward, else it crash-loops). Mirror `pipeline.HandleEvent`'s recover. | High |
| Med | internal/api/v1/ledger_stream.go:87; price_tip_stream.go:108; observations_stream.go:122 (all `go s.run…Producer`) | X9 | All three per-connection SSE producer goroutines are spawned **bare** (no `defer recover`). They run OUTSIDE the request goroutine, so `middleware.Recoverer` (which wraps `next.ServeHTTP` on the request goroutine) cannot catch them. The producers call into reader code (`s.ledgerTip`, the price/tip readers, the observations reader) + `json.Marshal`. A panic in any of those (e.g. a reader that itself isn't panic-safe, a nil-pointer in a view struct) crashes the whole `stellarindex-api` process — one client connection can take the API down. `streaming.StreamFromChannel` runs on the request goroutine (recover-covered) and the Hub-based `/v1/price/stream` uses `streaming.Stream` on the request goroutine (also covered), so those two are fine — only the three producer goroutines are exposed. | The streaming surface is public + unauthenticated-reachable; a single crafted or unlucky request that drives a producer into a panicking read path is a remote DoS on the API binary. Today the producers mostly read controlled cursor/price data so the trigger is narrow, but the structural gap (goroutine with no recover) is the exact case the prompt flagged. | Add `defer func(){ if r:=recover(); r!=nil { s.logger.Error("sse producer panic", …); close(ch) } }()` at the top of each `run…Producer`, or a small shared `go safeProducer(…)` wrapper. Closing `ch` lets `StreamFromChannel` return cleanly (client reconnects) instead of crashing the process. | Med |
| Med | cmd/stellarindex-aggregator/main.go:471-546; cmd/stellarindex-api/main.go:629-1039; cmd/stellarindex-indexer/main.go:295-1424 (every `go func(){ …Run(rootCtx) }`) | X9 | Every background worker — aggregator refresh / change-summary / supply-refresher / freeze-recovery / orchestrator, the API forex worker / market-cap refresher / redis pub+sub / customerwebhook worker, the indexer decoder-stats flusher / external runner / completeness — is launched in a **bare goroutine** with `_ = worker.Run(rootCtx)` and **no recover**. None of `internal/{aggregate,customerwebhook,completeness,supply,sources/external}` contains a single `recover()` (verified). A panic in any worker loop (`tick`, refresh cycle, poll) crashes the entire binary rather than being isolated + restarted. The external CEX/FX connectors parse adversarial vendor JSON — the highest-risk reachable input among the workers — and the runner (`internal/sources/external/runner.go:103-126`) spawns `forwardTrades`/`runPoller` bare too. | A misbehaving upstream (vendor sends a shape the parser doesn't `comma-ok`, a nil deref in a refresh) becomes a full-binary crash, not a logged + counted single-cycle failure. The codebase already HAS the right pattern (`divergence/compare.go` recovers per-reference so "a misbehaving reference MUST NOT take the whole comparison run down") — it just isn't applied to the long-lived worker goroutines. | Introduce one `obs.GoSafe(name, fn)` / supervised-goroutine helper that recovers, logs with the worker name + stack, increments a `worker_panics_total{worker}` counter, and (for the long-lived loops) restarts with backoff; route every `go func(){ …Run() }` through it. At minimum recover-and-log so one worker can't kill its siblings. | Med |
| Med | internal/xdrjson/operation.go:95 (createAccount dest), 144 & 148 (trustor), 165 (clawback from); internal/xdrjson/helpers.go:32,35,63,66 (asset issuer) | D9 | Same panic CLASS as the already-fixed muxed-destination bug, in other arms: `AccountId.Address()` calls `GetAddress()` and `panic(err)` on any PublicKey discriminant ≠ Ed25519 (SDK account_id.go:12-19, confirmed). These run on raw decoded XDR in the explorer's per-op field decoder with no per-op recover. The muxed cases were correctly migrated to the non-panicking `muxedAddr`→`GetAddress` helper (helpers.go:16-21), but `op.Destination.Address()` (create_account), both `op.Trustor.Address()` (allow_trust / set_trustline_flags), `op.From.Address()` (clawback), and `Issuer.Address()` in `assetID`/`assetCode` (every credit-asset field on every op) were NOT — they still call the panicking form. | Contradicts the explorer's stated "one malformed op never fails the response" degrade contract (A11 logged the muxed instance as a Med; this is its surviving siblings). It is CONTAINED to a clean 500 by `middleware.Recoverer` (server.go:866) — NOT a crash — but it 500s the WHOLE tx/ops response instead of degrading that one op to RawXDR. Trigger requires an out-of-range PublicKey type discriminant in a body that otherwise unmarshals; rare on pubnet but exactly the adversarial-XDR case in scope. | Replace each `X.Address()` on an `xdr.AccountId` in xdrjson with the error-returning `GetAddress()` (the dispatcher already uses the safe `accountIDToStrkey` analogue at dispatcher.go:1256), falling through to RawXDR / `"unknown_address"` on error; or wrap `fillOpFields` in a recover that demotes to RawXDR. | High |
| Low | internal/customerwebhook/worker.go:178-188 (`tick`/`deliverOne`) | X9 | `Worker.Run`'s loop calls `w.tick` → `w.deliverOne` (HMAC-signs + POSTs each pending row) with no recover. The package has zero `recover()`. A panic in signing or response handling crashes the API binary (where the worker runs, api/main.go:1029-1030). Lower than the general worker finding because inputs are DB-sourced (operator-controlled webhook config) not chain/attacker data, but it shares the structural gap. | One poison delivery row panicking takes the whole API down rather than failing that one delivery (the loop is explicitly designed so "one bad row doesn't stall the queue" — but a panic, unlike an error, isn't caught). | Covered by the Med `GoSafe` worker-wrapper fix; or add a per-`deliverOne` recover so a single delivery's panic is counted as a failed attempt. | High |
| Info | internal/sources/external/chainlink/decode.go:197 | D9 | `bigEndianUint64` does `panic(fmt.Sprintf("…expects 8 bytes, got %d", len(b)))` instead of returning an error, inconsistent with every other decoder in the tree (which return errors). Reachable from off-chain chainlink-HTTP payload parsing; the surrounding external runner has no recover, so this would crash the binary if the length invariant is ever violated by upstream. | Stylistic divergence with a real (if narrow) crash path; everything else in the decode tree degrades via error return. | Return an error and let the connector's normal error path count it. | High |

---

## CORRECT — verified, no issue

- **Live-indexer ingest seam IS panic-defended (the load-bearing
  guard).** `pipeline.ProcessLedger` (processor.go:39-48) has a
  `defer recover()` that converts ANY decoder panic into a per-ledger
  error: the cursor is NOT advanced, the ledger is logged at WARN +
  refused, the process keeps running. The production indexer
  (`cmd/stellarindex-indexer/main.go:1340`) and `stellarindex-ops
  backfill` (backfill.go:385) both route through it. So one malformed
  ledger crashing the live indexer is **not** possible via the
  dispatcher path — the panic is contained to a single refused ledger.
- **Per-event sink is panic-defended.** `pipeline.HandleEvent`
  (sink.go:413-422) recovers per event, logs, and increments
  `SourceInsertErrorsTotal{source,"panic"}` — "a single malformed
  Amount can't take the whole sink down." This is also the projector's
  sink, so the sink half of the projector path is safe (only the
  Decode half is the High finding).
- **Decoder topic/arg indexing is length-guarded.** Every
  `sources/*/decode.go` checks `len(Topic) < N` (or uses
  `scval.AsTupleN`) before literal-index access. Verified directly on
  the two the broad sweep flagged as suspect: soroswap/decode.go:52
  guards `len(e.Topic) < 2` before `Topic[0]`/`[1]`; sep41_supply
  /decode.go:96 guards `len(ev.Topic) >= 3` before `Topic[2]`. Both
  were false positives — the decoders are robust. cctp / aquarius /
  band / reflector / blend / comet / defindex / rozo / redstone all
  follow the same guard-then-index discipline.
- **Decoder `Must*` accessors run only after a variant guard.** Every
  `MustSuccess` / `MustV3/V4` / `MustOrderBook` / `MustContractId` /
  `MustAlphaNum*` in `internal/dispatcher` (census.go, dispatcher.go)
  and `internal/sources/sdex/decode.go` is preceded by a `switch` on
  the union type or an explicit code check — so the panicking branch is
  unreachable. `internal/scval/scval.go:370-391` gates each `Must*` on
  a `switch addr.Type`. These are safe by construction, and even if one
  regressed it would be caught by the ProcessLedger recover.
- **`scval` exposes non-panicking parsers for raw input.** Decoders
  parse adversarial SCVal via `scval.Parse` / `AsAddressStrkey` /
  `AsAmountFromI128` (all return errors), never the `Must*` form, on
  network-sourced bytes. `AsAddressStrkey` handles all five ScAddress
  variants with an error return — no `.Address()` panic in the ingest
  path.
- **Dispatcher uses the safe AccountId path.**
  `accountIDToStrkey` (dispatcher.go:1256) returns an error on a
  non-Ed25519 PublicKey type instead of panicking — the correct
  analogue of the `AccountId.Address()` that xdrjson still uses (Med
  finding). The ingest path got this right; the explorer decoder
  didn't.
- **`mustParseRFC3339` (dispatcher.go:1267) is self-generated-data
  only.** The string it parses was produced two lines earlier by
  `lcm.ClosedAt().UTC().Format(time.RFC3339)` (dispatcher.go:522), so a
  failure is a pure programming bug, and it runs inside the
  ProcessLedger recover anyway. Defended.
- **`scval.MustEncode*` panics are package-init / compile-time
  constants.** Every `MustEncodeSymbol`/`MustEncodeString` in every
  `sources/*/events.go` is a package-level `var` over a literal string
  constant — it fires at program start on a programmer typo, never on
  network data. Same for `redstone/feeds.go` init-time feed registry
  and `reflector`'s init-time `NewFiatAsset("USD")`.
- **`xdrjson` muxed-account fix confirmed.** The MuxedAccount.Address
  panic A11 flagged was fixed: `muxedAddr` (helpers.go:16-21) now uses
  `m.GetAddress()` (error-swallowed, no panic) and operation.go:99/104/
  112/153 route through it. (The surviving `AccountId.Address()`
  siblings are the Med finding above — same class, different type.)
- **`xdr.SafeUnmarshalBase64` everywhere.** xdrjson uses the
  error-returning base64/XDR unmarshal, not the panicking variant
  (A11-verified); the explorer `opView` falls back to RawXDR on a
  decode error rather than panicking.
- **HTTP request goroutine is fully covered.** `middleware.Recoverer`
  (recoverer.go:20-51) is in the single middleware stack `Handler()`
  (server.go:866) builds, and **every** route is registered on the
  **same** `s.mux` — including the dashboard mounts
  (`dashboardAuth/Keys/Webhooks.Mount(s.mux)`, server.go:1208-1220),
  SEP-10 (1228-1229), signup (1194-1199), the Stripe webhook (1200),
  and all explorer routes (1025-1041). `Handler()` wraps the whole mux,
  so there is NO route group that bypasses the Recoverer for the
  request goroutine. Recoverer also correctly re-panics
  `http.ErrAbortHandler` (the one panic the stdlib must keep handling).
- **divergence is the model of per-task isolation.**
  `compare.go:141-150` recovers each reference's `LookupPrice` in its
  own goroutine and records the panic as a normal failure outcome —
  "a misbehaving reference MUST NOT take the whole comparison run
  down" — and `safeName(r)` (compare.go:133) even guards the panic in
  `r.Name()` itself. This is the pattern the worker goroutines (Med
  finding) should adopt.
- **`extractInvokeContractCallTrees` / `ExtractContractCallTree`
  index-safe.** contract_call_export.go:33 guards
  `len(trees)==0 || trees[0]==nil` before indexing; the dispatcher's
  per-op `invokeCalls[opIdx]` access is bounds-checked
  (`opIdx < len(invokeCalls)`, dispatcher.go:583).
- **Dispatcher stats map writes are mutex-guarded** (statsMu,
  dispatcher.go:536-538/577-579) — no nil-map / concurrent-map-write
  panic on the count path (the F-1317 class is closed).
- **Hub / redispub** contain no bare goroutines that decode
  attacker data into a panic; the Hub fan-out runs on the request
  goroutine via `streaming.Stream`.

---

## Files read

Request-path / middleware / SSE:
- internal/api/v1/middleware/recoverer.go
- internal/api/v1/server.go (middleware stack + full mux wiring)
- internal/api/v1/ledger_stream.go
- internal/api/v1/price_tip_stream.go (header/spawn)
- internal/api/v1/observations_stream.go (spawn)
- internal/api/v1/price_stream.go (Hub path)
- internal/api/streaming/handler.go
- internal/api/streaming/hub.go (goroutine sweep)

Ingest / decode:
- internal/pipeline/processor.go (ProcessLedger recover boundary)
- internal/pipeline/sink.go (HandleEvent recover)
- internal/dispatcher/dispatcher.go (ProcessLedger decode seam,
  accountIDToStrkey, mustParseRFC3339, entry-change dispatch)
- internal/dispatcher/census.go (Must* guards)
- internal/dispatcher/contract_call_export.go
- internal/projector/projector.go (Run + runOneSource + cycleOneSource)
- internal/scval/scval.go (Must* variant guards, Parse/As* error forms)
- internal/sources/soroswap/decode.go
- internal/sources/sep41_supply/decode.go
- internal/sources/external/runner.go
- (swept) internal/sources/{band,cctp,aquarius,reflector,blend,comet,
  defindex,rozo,redstone,sdex}/decode.go — guard-then-index confirmed
- internal/sources/external/chainlink/decode.go (Info panic)

Workers / binaries:
- internal/customerwebhook/worker.go
- internal/divergence/compare.go (per-reference recover model)
- cmd/stellarindex-indexer/main.go (projector + worker spawns)
- cmd/stellarindex-aggregator/main.go (worker spawns)
- cmd/stellarindex-api/main.go (worker + SSE-producer-feeding spawns)

Explorer decoder (panic class):
- internal/xdrjson/operation.go
- internal/xdrjson/helpers.go

Cross-reference:
- go-stellar-sdk@v0.5.0 xdr/account_id.go + xdr/muxed_account.go
  (Address() → GetAddress() → panic(err) on non-Ed25519 / unknown
  type)
- docs/audit-2026-06-14/areas/A11-api-explorer.md (muxed finding +
  Recoverer containment, prior art)

---

## Severity counts

- Critical: 0
- High: 1
- Medium: 3
- Low: 1
- Info: 1
