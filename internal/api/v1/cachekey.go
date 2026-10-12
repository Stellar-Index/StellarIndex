package v1

import (
	"sort"
	"strconv"
	"strings"
)

// cacheKey is the typed builder for in-process read-cache keys, preventing prewarm-vs-handler key drift: every result-changing
// dimension goes through one typed method and set-valued ones are ORDER-NORMALISED by [cacheKey.strSet]. Fields are
// length-prefixed (`<len>:<bytes>`) because a `|` grammar rendered str("a|b").str("c") and str("a").str("b|c") identically,
// serving one caller another's page (analogue of internal/cachekeys, ADR-0007).
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

// order appends a sort-order enum as the integer discriminator the SQL layer switches on, so the key partitions exactly where
// the result set does.
func (k *cacheKey) order(o int) *cacheKey {
	return k.int(o)
}

// strSet appends a SET-valued dimension; the slice is copied and SORTED (nil and empty both mean "no filter"), sound because
// consumers treat these sets as order-independent. Each member is its own length-prefixed field, so {"a,b"} and {"a","b"}
// cannot collide.
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
