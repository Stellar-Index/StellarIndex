---
title: D6 — Style / idiom consistency
---

# D6 — Style / idiom consistency

**Headline: the codebase is unusually self-consistent on idioms.** One M1, the rest M2, **no
M0**. gofumpt/golangci enforce format; these are the *idiom* findings.

## M1 — the one real friction
**Constructor / logger-injection has 3 competing shapes:** purely positional (majority), positional
deps + trailing `opts Options` struct (24), positional deps + variadic `...Option` functional options
(12). Worse, **logger injection splits mature-vs-recent**: 11 constructors take `logger *slog.Logger`
positionally (mature), 18 carry `Logger` in an Options/Config struct (recent) — and some mix a
*positional* logger WITH an options bag in one signature. **Canonical (already the plurality for new
code):** `New(requiredDeps…, opts Options) (*T, error)` with `Options` holding all optional knobs
**including `Logger`**, nil-guarded to `slog.Default()`. Retire positional loggers + stop adding new
`...Option` variants. Codify in `docs/engineering-standards.md`.

## M2 — polish
`childgate`/`forex`/`frankfurter` use an inline package-comment instead of doc.go (give them doc.go);
slog uses variadic-kv everywhere (769 calls, 0 typed attrs — forgoes compile-time key safety, note
only); logger struct field `logger`(39) vs `log`(3) — standardize; `MarketSourcesResp` lone abbrev;
awkward `-er` coinages (`SupplyLooker`).

## Already CONSISTENT (the good bones — leave alone)
`%w` error wrapping (1934 vs 34 `%v`, and all 34 `%v` are legit value-formatting) + sentinel `Err…`
vars ~100% prefixed `"pkg: message"` + `errors.Is/As`; **decoders stay pure + propagate** (dispatcher
return-and-metric, never swallow; the 67 `_ =` are idiomatic cleanup); **`ctx` always first + threaded,
never stored**; **slog is the ONLY logger** (81 files, 0 std-log/zap/zerolog) with identical
`if logger==nil { logger=slog.Default() }` fallback + stable field-keys (err/source/ledger/contract/
pair/tx_hash); **consumer-side narrow `-er` interfaces** implemented by the fat `*timescale.Store`
("accept interfaces, return structs"); the doc.go(observers)/README(five-file-sources) convention is
real + followed (3 stragglers).

**Net:** one short "constructor conventions" section to write; the rest is a codify-what's-already-true
exercise. Style is a strength, not a liability.
