package v1

import (
	"sort"
	"strconv"
	"strings"
)

// cacheKey is the typed builder for the in-process read caches' map keys (CachedMarketsReader,
// CachedIssuersReader and sibling listing caches). It prevents prewarm-vs-handler key drift: every
// result-changing dimension goes through one typed method (str / int / order / strSet), and
// set-valued dimensions are ORDER-NORMALISED by [cacheKey.strSet].
//
// Grammar: length-prefixed fields, `<len>:<bytes>`, concatenated (netstring-style), so content
// cannot forge a separator. A `|`-delimited grammar rendered str("a|b").str("c") and
// str("a").str("b|c") identically, serving one caller another caller's cached page.
//
// In-process analogue of internal/cachekeys (ADR-0007); local to package v1 because cachekeys must
// not depend on the storage layer.
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

// field appends s as one length-prefixed segment: "<len(s)>:<s>". The length prefix is the sole
// boundary marker and s is written verbatim, so no character in s can shift a field boundary.
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

// order appends a sort-order enum as the integer discriminator the SQL layer switches on, so the
// key partitions exactly where the result set does. Callers pass int(order) to keep the builder
// free of storage-layer enum types.
func (k *cacheKey) order(o int) *cacheKey {
	return k.int(o)
}

// strSet appends a SET-valued dimension (a Sources filter, an asset_id batch). The slice is copied
// and SORTED so element order cannot drift between call sites; nil and empty both mean "no
// filter". Only the key is normalised (the upstream query gets the original slice), which is sound
// because every consumer treats these sets as order-independent.
//
// Each member is its own length-prefixed field after the member count, not a ','-joined string,
// so {"a,b"} and {"a","b"} cannot collide.
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
