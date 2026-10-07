// Package cachekeys is the single source of the Redis key grammar
// (ADR-0007): one builder and one TTL per key class, so a mistyped key is a
// compile error rather than a cache miss forever.
//
// Builders take typed parts (Price takes a canonical.Asset, not a string)
// and return a distinct named string type per class ([PriceKey],
// [VWAPKey], …), so one class's key cannot be passed where another's is
// wanted and a hand-built string does not type-check; leaving for the
// Redis wire takes an explicit .String(). The package imports no Redis
// client and neither talks to Redis nor serialises values, so any layer
// can use it. internal/ratelimit owns the `rl:*` keys and does not import
// this package; the RateLimit test here pins its prefix against drift.
package cachekeys
