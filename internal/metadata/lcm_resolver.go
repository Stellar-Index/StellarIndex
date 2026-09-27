package metadata

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// IssuerHomeDomain is one on-chain reading of an account's home_domain.
//
// Observed=false: no AccountEntry observation exists for the account, so
// the chain has told us nothing and the operator-static map may answer.
// Observed=true, Domain=="": the latest observation says the account has
// no home_domain (never set, cleared by SetOptions, or the account was
// merged). The ledger is authoritative here: an issuer that clears its
// home_domain has withdrawn the identity claim, and a static value must
// not re-assert it.
type IssuerHomeDomain struct {
	Observed bool
	Domain   string
}

// AccountObservationLookup is the storage-side primitive the
// [LCMHomeDomainResolver] consumes. Production impl adapts
// timescale.Store.LatestAccountObservationAtOrBefore; tests pass fakes.
// A missing observation is IssuerHomeDomain{Observed: false} with a nil
// error; only storage failures return an error.
type AccountObservationLookup interface {
	HomeDomainAtOrBefore(ctx context.Context, issuer string, asOfLedger uint32) (IssuerHomeDomain, error)
}

// LCMHomeDomainResolver reads the latest home_domain the AccountEntry
// observer recorded in `account_observations` for an issuer. It only
// knows accounts the indexer watches (`[metadata].watched_issuer_accounts`
// plus `[supply].sdf_reserve_accounts`); per ADR-0021 the operator-static
// `[metadata.issuer_home_domains]` map answers for accounts with no
// observation.
type LCMHomeDomainResolver struct {
	store AccountObservationLookup
}

// NewLCMHomeDomainResolver constructs the live resolver.
func NewLCMHomeDomainResolver(store AccountObservationLookup) *LCMHomeDomainResolver {
	return &LCMHomeDomainResolver{store: store}
}

// HomeDomainFor returns the latest observed home_domain reading for the
// issuer's G-strkey. Storage errors are wrapped in [ErrLCMUnavailable].
func (r *LCMHomeDomainResolver) HomeDomainFor(ctx context.Context, issuer string) (IssuerHomeDomain, error) {
	// "Latest observation" sentinel; must fit the int4 ledger column
	// (^uint32(0) overflowed it with pq 22003 on every call).
	const observerLatestLedger = uint32(math.MaxInt32)
	hd, err := r.store.HomeDomainAtOrBefore(ctx, issuer, observerLatestLedger)
	if err != nil {
		return IssuerHomeDomain{}, fmt.Errorf("%w: %w", ErrLCMUnavailable, err)
	}
	if !hd.Observed {
		return IssuerHomeDomain{}, nil
	}
	return hd, nil
}

// ErrLCMUnavailable signals the LCM-derived path failed to read
// (storage error, not "no observation"). The chained-fallback
// caller logs + drops to the static map.
var ErrLCMUnavailable = errors.New("metadata: LCM resolver storage error")

// lcmLookupTimeoutMs bounds one resolver read inside the caller's request
// context so a slow store degrades to the static map instead of stalling
// the response.
const lcmLookupTimeoutMs = 100

// ChainedHomeDomainLookup composes the live LCM resolver with the
// operator-static map (`cfg.Metadata.HomeDomainFor`):
//
//  1. An observation exists → its reading is final: (domain, true) when
//     the account carries a home_domain, ("", false) when it does not.
//     The static map is NOT consulted, so an on-chain clear or merge
//     cannot be overridden by operator config.
//  2. No observation → the static map answers.
//  3. Storage error → warnFn logs and the static map answers.
//
// The read runs under the caller's ctx (bounded to 100ms), so a client
// that disconnects cancels it.
func ChainedHomeDomainLookup(
	live *LCMHomeDomainResolver,
	static func(issuer string) (string, bool),
	warnFn func(msg string, kv ...any),
) func(ctx context.Context, issuer string) (string, bool) {
	return func(ctx context.Context, issuer string) (string, bool) {
		if hd := observeHomeDomain(ctx, live, issuer, warnFn); hd.Observed {
			return hd.Domain, hd.Domain != ""
		}
		return static(issuer)
	}
}

// ObservedHomeDomainLookup is layer 1 of [ChainedHomeDomainLookup] alone:
// ("", false) for an unobserved issuer or a storage error. For a caller
// that runs its own live on-chain read before the static map.
func ObservedHomeDomainLookup(
	live *LCMHomeDomainResolver,
	warnFn func(msg string, kv ...any),
) func(ctx context.Context, issuer string) (string, bool) {
	return ChainedHomeDomainLookup(live, func(string) (string, bool) { return "", false }, warnFn)
}

// StaticHomeDomainFallback is the static tail of [ChainedHomeDomainLookup]
// alone: it answers only when the issuer has no observation (or the read
// failed), so an observed on-chain clear still suppresses the static map.
func StaticHomeDomainFallback(
	live *LCMHomeDomainResolver,
	static func(issuer string) (string, bool),
	warnFn func(msg string, kv ...any),
) func(ctx context.Context, issuer string) (string, bool) {
	return func(ctx context.Context, issuer string) (string, bool) {
		if observeHomeDomain(ctx, live, issuer, warnFn).Observed {
			return "", false
		}
		return static(issuer)
	}
}

// observeHomeDomain reads the issuer's latest observation under the
// caller's ctx bounded to lcmLookupTimeoutMs; a storage error is logged
// and reported as unobserved.
func observeHomeDomain(ctx context.Context, live *LCMHomeDomainResolver, issuer string, warnFn func(msg string, kv ...any)) IssuerHomeDomain {
	ctx, cancel := contextWithTimeoutMs(ctx, lcmLookupTimeoutMs)
	defer cancel()
	hd, err := live.HomeDomainFor(ctx, issuer)
	if err != nil {
		if warnFn != nil {
			warnFn("LCM home-domain resolver failed; falling back to static map",
				"issuer", issuer, "err", err)
		}
		return IssuerHomeDomain{}
	}
	return hd
}
