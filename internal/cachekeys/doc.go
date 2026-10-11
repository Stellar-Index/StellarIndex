// Package cachekeys is the single source of the Redis key grammar
// (ADR-0007): one builder and one TTL per key class, so a mistyped key is a
// compile error rather than a cache miss forever.
//
// Builders take typed parts (Price takes a canonical.Asset) and return a distinct named string
// type per class, so one class's key cannot stand in for another's and a hand-built string does
// not type-check; reaching the Redis wire takes an explicit .String(). The package imports no
// Redis client, so any layer can use it. internal/ratelimit owns `rl:*` without importing this
// package; the RateLimit test here pins its prefix against drift.
package cachekeys
