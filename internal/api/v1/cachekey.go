package v1

import (
	"sort"
	"strconv"
	"strings"
)

// cacheKey is the typed builder for the in-process read caches'
// (CachedMarketsReader, CachedIssuersReader, and their sibling
// listing caches) map keys. It exists to kill the "prewarm-vs-handler
// key drift" bug
// class — three shipped bugs (memory: feedback_prewarm_handler_drift)
// came from the prewarm goroutine and the handler stringifying the
// same Order / Sources / Limit dimensions into subtly different raw
// keys, so the warmed entry never matched the user request.
//
// Two structural guarantees remove that class:
//
//  1. Every dimension that changes the result set is appended through
//     an explicit typed method (str / int / order / strSet). The
//     grammar for a given method is defined once, so the prewarm and
//     handler paths that both call the wrapped reader method are
//     physically incapable of producing different key formats.
//  2. Set-valued dimensions (the Sources filter, an asset_id batch)
//     go through [cacheKey.strSet], which ORDER-NORMALISES the slice.
//     Two call sites passing the same set in a different order — the
//     exact Sources-order footgun the AllPools prewarm relied on a
//     convention to avoid — can no longer land on different slots.
//
// Grammar: each field is length-prefixed — `<len>:<bytes>` — and
// fields are simply concatenated, netstring-/Bencode-style. Because
// every field carries its own length, two different sequences of
// fields can never serialize to the same string no matter what bytes
// a field contains: there is no separator byte for content to forge.
//
// (Earlier revisions used a `|`-delimited grammar on the assumption
// that '|' "cannot appear in an asset_id, source name, cursor, or
// code" — an assumption a hostile filter value or cursor could
// violate: str("a|b").str("c") and str("a").str("b|c") both rendered
// as ".../a|b|c...", so one caller's request could be served another
// caller's cached page. API-05 / COR-14. Length-prefixing removes the
// assumption instead of trying to escape around it.)
//
// This is the in-process analogue of internal/cachekeys (the Redis
// key grammar mandated by ADR-0007). It is kept local to package v1
// rather than folded into cachekeys because the keys reference the
// timescale sort-order enums, and cachekeys is a foundational package
// that must not depend on the storage layer.
type cacheKey struct {
	b strings.Builder
}

// newCacheKey starts a key for operation op (the wrapped method name,
// which also namespaces the key so two methods can't collide).
func newCacheKey(op string) *cacheKey {
	k := &cacheKey{}
	k.field(op)
	return k
}

// field appends s as one length-prefixed segment: "<len(s)>:<s>". The
// length prefix is the sole boundary marker — s itself is written
// verbatim, so no character in s (including ':' or a digit sequence
// that could be mistaken for a length) can ever shift a field
// boundary: the decoder (if there were one) always reads exactly
// len(s) bytes after the ':', full stop.
func (k *cacheKey) field(s string) *cacheKey {
	k.b.WriteString(strconv.Itoa(len(s)))
	k.b.WriteByte(':')
	k.b.WriteString(s)
	return k
}

// str appends a scalar string dimension (cursor, asset_id, source,
// issuer, code, free-text query, …).
func (k *cacheKey) str(s string) *cacheKey {
	return k.field(s)
}

// int appends a scalar integer dimension (limit, …).
func (k *cacheKey) int(n int) *cacheKey {
	return k.field(strconv.Itoa(n))
}

// order appends a sort-order enum (a markets or asset-listing sort
// order) as its integer discriminator — the same value
// the SQL layer switches on, so the key partitions exactly where the
// result set does. Callers pass int(order); the builder stays free of
// the storage-layer enum types.
func (k *cacheKey) order(o int) *cacheKey {
	return k.int(o)
}

// strSet appends a SET-valued dimension (a Sources filter, an
// asset_id batch). The slice is defensively copied and SORTED so
// element order can never drift between call sites — the structural
// half of the Sources-order fix. nil and empty both render as a
// zero-length set (both mean "no filter"). Callers pass the original
// unsorted slice to the upstream query; only the key is normalised,
// which is sound because every consumer of these sets treats them as
// order-independent (an IN-filter / a map keyed by asset_id).
//
// Each member is its own length-prefixed field (preceded by the
// member count), rather than a ','-joined string — the same
// content-cannot-forge-a-boundary property [field] gives str/int, now
// applied to set members too (a member containing ',' could otherwise
// collide two different sets, e.g. {"a,b"} vs {"a","b"}).
func (k *cacheKey) strSet(ss []string) *cacheKey {
	sorted := append([]string(nil), ss...)
	sort.Strings(sorted)
	k.field(strconv.Itoa(len(sorted)))
	for _, s := range sorted {
		k.field(s)
	}
	return k
}

// build returns the finished key.
func (k *cacheKey) build() string { return k.b.String() }
