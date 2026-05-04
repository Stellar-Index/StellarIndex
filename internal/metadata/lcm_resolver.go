package metadata

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// AccountObservationLookup is the storage-side primitive the
// [LCMHomeDomainResolver] consumes. Production impl is
// timescale.Store.LatestAccountObservationAtOrBefore via an
// adapter; tests pass in-memory fakes.
//
// Returns ErrNoObservation (or a similar storage-side typed
// error) when the account has no observation; the resolver
// translates that into ("", false, nil) so the chained-fallback
// caller drops to the operator-static map.
type AccountObservationLookup interface {
	HomeDomainAtOrBefore(ctx context.Context, issuer string, asOfLedger uint32) (string, bool, error)
}

// LCMHomeDomainResolver replaces the operator-static
// `[metadata.issuer_home_domains]` map with live data observed by
// the AccountEntry observer (#298). Per ADR-0021 the static map
// stays in tree as a bootstrap fallback — operators that flip
// to LCM keep the static entries for issuers the observer hasn't
// backfilled yet.
//
// Wire shape matches the existing
// `MetadataConfig.HomeDomainFor(issuer string) (string, bool)`
// closure that the API binary passes to storeAssetReader. The
// resolver bakes a context-with-timeout internally so the call
// site's signature can stay sync.
type LCMHomeDomainResolver struct {
	store AccountObservationLookup
}

// NewLCMHomeDomainResolver constructs the live resolver.
func NewLCMHomeDomainResolver(store AccountObservationLookup) *LCMHomeDomainResolver {
	return &LCMHomeDomainResolver{store: store}
}

// HomeDomainFor returns the most recently observed HomeDomain for
// the issuer's G-strkey. Returns ("", false, nil) when no
// observation exists OR the most-recent observation has an empty
// HomeDomain (operator-static-map fallback applies in both
// cases — they're indistinguishable from the consumer's POV).
//
// Storage-layer errors are wrapped, not swallowed — the closure
// adapter logs them but presents ("", false) to the legacy
// signature so the asset-detail handler still serves the response.
func (r *LCMHomeDomainResolver) HomeDomainFor(ctx context.Context, issuer string) (string, bool, error) {
	// Sentinel for "no upper bound, give me the latest observation."
	// MUST fit in postgres int4 (the `account_observations.ledger`
	// column type) — the previous `^uint32(0)` (MaxUint32) overflowed
	// every call with `pq: value "4294967295" is out of range for
	// type integer (22003)`, defeating the LCM path on r1 and
	// silently routing every issuer through the static-map fallback.
	// math.MaxInt32 (2,147,483,647) is far past Stellar's current
	// ledger (~62M @ 5 ledgers/sec → ~13y of headroom) so the
	// "ledger <= sentinel" semantics are preserved without overflow.
	const observerLatestLedger = uint32(math.MaxInt32)
	domain, ok, err := r.store.HomeDomainAtOrBefore(ctx, issuer, observerLatestLedger)
	if err != nil {
		// Wrap so the caller can errors.Is for fall-back behaviour.
		return "", false, fmt.Errorf("%w: %w", ErrLCMUnavailable, err)
	}
	if !ok || domain == "" {
		return "", false, nil
	}
	return domain, true, nil
}

// ErrLCMUnavailable signals the LCM-derived path failed to read
// (storage error, not "no observation"). The chained-fallback
// caller logs + drops to the static map.
var ErrLCMUnavailable = errors.New("metadata: LCM resolver storage error")

// ChainedHomeDomainLookup composes a live LCM resolver with an
// operator-static fallback map into a single sync function value
// matching the existing
// `func(issuer string) (string, bool)` signature that
// storeAssetReader.homeDomainLookup expects.
//
// On every call:
//
//  1. Try the LCM resolver with a baked-in 100ms timeout. If it
//     returns a non-empty domain, return it.
//  2. If LCM returned ("", false, nil) (no observation OR empty
//     domain), fall through to the static map.
//  3. If LCM returned a wrapped ErrLCMUnavailable (storage error),
//     log via the supplied warnFn and fall through to the static map.
//
// `static` mirrors `cfg.Metadata.HomeDomainFor`. `warnFn` is the
// API binary's logger hook — pass a no-op for tests.
func ChainedHomeDomainLookup(
	live *LCMHomeDomainResolver,
	static func(issuer string) (string, bool),
	warnFn func(msg string, kv ...any),
) func(issuer string) (string, bool) {
	return func(issuer string) (string, bool) {
		ctx, cancel := contextWithTimeoutMs(100)
		defer cancel()
		domain, ok, err := live.HomeDomainFor(ctx, issuer)
		switch {
		case err != nil:
			if warnFn != nil {
				warnFn("LCM home-domain resolver failed; falling back to static map",
					"issuer", issuer, "err", err)
			}
		case ok:
			return domain, true
		}
		return static(issuer)
	}
}
