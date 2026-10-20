// Package cachekeys is the single source of the Redis key grammar (ADR-0007): one builder and one
// TTL per key class, so a mistyped key is a compile error rather than a cache miss forever.
//
// Builders take typed parts (Price takes a canonical.Asset) and return a distinct named string
// type per class, so a hand-built string does not type-check; reaching the wire takes an explicit
// .String(). internal/ratelimit owns `rl:*` without importing this package; the RateLimit test
// here pins its prefix against drift.
package cachekeys
