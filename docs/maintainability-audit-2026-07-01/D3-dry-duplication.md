---
title: D3 — DRY / duplication register (FLAGSHIP)
---

# D3 — Duplication register

**Headline:** the flagship maintainability debt is **~50+ near-identical helper copies
concentrated in `internal/sources/external/`, driven by an institutionalized "copy the
binance package" workflow** (the CLAUDE.md CEX recipe literally says to). Every claimed
duplicate verified by reading both copies. **M0** = will be re-duplicated / has caused rework.

| # | Cluster | Copies | Canonical | Rate |
|---|---------|--------|-----------|------|
| 1 | External scaling + synthetic-hash helpers | ~36 fn copies / 11 pkgs | **none — extract** | **M0** |
| 2 | External WS reconnect/backoff infra | 4× of 5 helpers | **none — extract** | **M0** |
| 6 | ClaimAtom COUNTING (not extraction) | 3× | export from `sdex` | **M0** |
| 8 | SSRF / private-IP dial guard (divergent!) | 2× | **none — extract** | **M0 (sec)** |
| 3/4 | pair-inversion (8×) / candle→trade (3×) | — | none | M1 |
| 7 | Redis "N-per-window" throttles | 4× | `internal/ratelimit` | M1 |
| 9 | oracle ts-overflow guard | 3× | `canonical.SafeUnixSeconds` | M1 |
| 10 | envelope/response writers (writeJSON/writeProblem) | 2 identical + ~6 | `api/v1/envelope.go` | M1 |
| 11 | `consumer.Event` coupled switches | 3 switches | `pipeline/sink.go` | M1 |
| 5/12 | http.Client{30s} (11×) / storage NULL boilerplate | — | (mostly intrinsic) | M2 |

## The M0s
- **Cluster 1 (flagship) — external scaling + tx-hash.** `decimalStringToScaledInt` **10 copies
  (9 byte-identical**; only `exchangeratesapi` adds a sci-notation branch); `floatToScaledInt` 4×;
  `pow10` 7×; synthetic tx-hash ~15 copies in 3 families (`formatTxHash`/`backfillTxHash`/
  `syntheticTxHash` — only `chainlink`'s sha256 form avoids truncation-collision risk); `externalAmountDecimals=8`
  4×, `DefaultDecimals` 7×. **`canonical.FromString` is NOT the home** (integer-only, rejects a decimal
  point). No shared home exists (only framework/registry/runner are shared). The "local & auditable"
  comment rationale is moot — the `targetDecimals` arg already localizes convention. **This gets
  re-pasted every new connector.**
- **Cluster 2 — WS reconnect/backoff.** `jitter` byte-identical 4×; `keepAliveHTTPClient` identical 4×;
  `classifyDisconnect` near-identical; `healthyConnectionThreshold` 4×; the ~50-line reconnect loop
  ~identical. A 5th streaming venue re-pastes ~150 lines.
- **Cluster 6 — ClaimAtom COUNTING forked (corrects the brief).** Extraction IS consolidated
  (`sdex/decode.go`, one copy — the "×5" was false). But `realTradeCount` is **verbatim in
  `dispatcher/census.go:226` AND `storage/clickhouse/extract.go:384`**, each with its own copy of the
  op-result arm switch → ClaimAtom knowledge expressed 3×. Drove a past coarse-PK/census incident.
- **Cluster 8 — SSRF guard duplicated with DIVERGENT coverage.** `metadata/sep1.go` blocks
  loopback/unspecified/link-local/private; `customerwebhook/ssrf.go` ALSO blocks multicast — **two
  different blocklists for the same security control.** Any future fetcher (divergence, notify) invents
  a third. Security controls must not silently diverge. **Fix: `internal/nettools.SafeDialer` (stricter union).**

## Consolidation plan (highest ROI first)
1. **`internal/sources/external/scale`** — one each of `DecimalStringToScaledInt`/`FloatToScaledInt`/
   `Pow10`/`SyntheticTxHash` (adopt chainlink's sha256). Delete ~36 copies. *(kills 1, 4, 5)*
2. **`internal/sources/external/wsclient`** — shared `ReconnectLoop`/`jitter`/`keepAliveHTTPClient`/
   `classifyDisconnect`/`InvertPairMap`; venues keep only frame-parse. *(kills 2, 3)*
3. Export a ClaimAtom counter from `sdex`; import in census + clickhouse/extract. *(kills 6)*
4. `internal/nettools.SafeDialer`/`IsBlockedIP`, reconcile to the stricter union. *(kills 8)*
5. `canonical.SafeUnixSeconds(u64)`. *(kills 9)*  6. `internal/httpx` (or promote envelope.go)
   `writeJSON`+`writeProblem`. *(kills 10)*  7. Promote `ratelimit` to a `FixedWindowCounter`
   consumed by login/signup throttles (route keys through cachekeys). *(kills 7)*
8. Lower: lint coupling `tradeFromEvent`↔`HandleEvent` (11); a `nulls` helper (12).

**Ties:** #1/#2 also fix D1's "copy the binance package" hazard and D9's misleading recipe —
extracting the shared helpers + rewriting the recipe to point at them is a single coherent fix.

## Already well-shared (don't touch)
`canonical` (284 importers — Amount/Trade/Pair, i128 once); `scval` (49); `cachekeys` (29 — the only
gap is the auth throttles bypassing it); `external/runner.go` (the fan-in runner IS shared — the
reconnect *inside* each streamer is what's duplicated); `sdex` extraction; `sink.go` one-writer.
**Dual UI components = FALSE alarm** (SEO refactor left it clean; `SourceSparkline` reused).
