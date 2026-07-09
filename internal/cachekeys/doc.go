// Package cachekeys is the single source of truth for Stellar Index's
// Redis key grammar, per ADR-0007.
//
// Rationale: every caller that writes to or reads from Redis must use
// these helpers rather than constructing key strings directly. That
// way a typo like `"prce:XLM"` is a compile error (the helper
// doesn't exist) rather than a silent cache-miss-forever.
//
// Design:
//
//   - Zero external dependencies. Pure string manipulation.
//     `import "github.com/redis/go-redis/v9"` is a decision we keep
//     out of this package so it can be imported by any layer
//     including code with no Redis knowledge (e.g. config loaders
//     that validate key prefixes at startup).
//
//   - One builder function per key class — never a generic
//     `Key(parts ...string) string`. The types of the parts matter:
//     `Price(asset canonical.Asset)` takes a typed asset, not a raw
//     string. Callers who pass a raw string get a compile error,
//     which is the point.
//
//   - Each builder returns a DISTINCT named string type, not a bare
//     `string` (`Price` returns [PriceKey], `VWAP` returns [VWAPKey],
//     etc — see keys.go's per-family doc comments). This closes the
//     other half of the typo-safety story: a bare `string` return
//     type means every key class is the SAME Go type, so nothing
//     stops a caller from handing a `VWAPKey` to a reader that wants
//     a `PriceKey`, or a hand-rolled `fmt.Sprintf("price:%s", ...)`
//     to anything that "just wants a string" — both type-check
//     silently. Named types make both a compile error: Go does not
//     implicitly convert between two distinct named string types (or
//     between a named type and plain `string`) even when their
//     underlying type is identical
//     (https://go.dev/ref/spec#Assignability). Crossing into the
//     untyped Redis wire protocol — `redis.Cmdable`'s methods only
//     know `string` — requires an explicit `.String()` call, which is
//     the one place per call site where "this key left the typed
//     world" is visible in a diff.
//
//   - One TTL helper per key class. Most return a constant; a few
//     (VWAP, OHLC-open-candle) take a parameter. Caller code reads
//     clearly: `ttl := cachekeys.PriceTTL`.
//
// Cross-reference:
//
//   - ADR-0007 — defines the 10 key classes, their semantics, and
//     TTL rules.
//   - internal/ratelimit — implements the `rl:*` family and does NOT
//     import this package (it has its own narrowly-scoped key shape).
//     The Test for `RateLimit` here just asserts the prefix matches
//     what internal/ratelimit writes, preventing drift.
//
// Non-goals:
//
//   - This package does NOT talk to Redis. Callers pass the returned
//     strings to `redis.Client` themselves. The
//     `internal/cachekeys/cache` package (future) will be the thin
//     convenience layer that couples both.
//
//   - This package does NOT serialise values. That belongs to the
//     caller who knows the domain type (canonical.Price, Asset etc.).
package cachekeys
