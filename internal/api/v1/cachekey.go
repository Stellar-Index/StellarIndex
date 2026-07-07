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
// Grammar: `<op>|<part>|<part>…`. `|` cannot appear in an asset_id,
// source name, cursor, or code, so it is an unambiguous field
// separator; set members are joined with `,` (also absent from those
// alphabets) inside their field.
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
	k.b.WriteString(op)
	return k
}

func (k *cacheKey) sep() { k.b.WriteByte('|') }

// str appends a scalar string dimension (cursor, asset_id, source,
// issuer, code, free-text query, …).
func (k *cacheKey) str(s string) *cacheKey {
	k.sep()
	k.b.WriteString(s)
	return k
}

// int appends a scalar integer dimension (limit, …).
func (k *cacheKey) int(n int) *cacheKey {
	k.sep()
	k.b.WriteString(strconv.Itoa(n))
	return k
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
// half of the Sources-order fix. nil and empty both render as the
// empty fragment (both mean "no filter"). Callers pass the original
// unsorted slice to the upstream query; only the key is normalised,
// which is sound because every consumer of these sets treats them as
// order-independent (an IN-filter / a map keyed by asset_id).
func (k *cacheKey) strSet(ss []string) *cacheKey {
	sorted := append([]string(nil), ss...)
	sort.Strings(sorted)
	k.sep()
	k.b.WriteString(strings.Join(sorted, ","))
	return k
}

// build returns the finished key.
func (k *cacheKey) build() string { return k.b.String() }
