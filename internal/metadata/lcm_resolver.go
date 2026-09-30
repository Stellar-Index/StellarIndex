package metadata

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
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
// error (the batch form omits the issuer); only storage failures return an
// error.
type AccountObservationLookup interface {
	HomeDomainAtOrBefore(ctx context.Context, issuer string, asOfLedger uint32) (IssuerHomeDomain, error)
	HomeDomainsAtOrBefore(ctx context.Context, issuers []string, asOfLedger uint32) (map[string]IssuerHomeDomain, error)
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

// observerLatestLedger is the "latest observation" sentinel; it must fit
// the int4 ledger column (^uint32(0) overflowed it with pq 22003).
const observerLatestLedger = uint32(math.MaxInt32)

// HomeDomainFor returns the latest observed home_domain reading for the
// issuer's G-strkey. Storage errors are wrapped in [ErrLCMUnavailable].
func (r *LCMHomeDomainResolver) HomeDomainFor(ctx context.Context, issuer string) (IssuerHomeDomain, error) {
	hd, err := r.store.HomeDomainAtOrBefore(ctx, issuer, observerLatestLedger)
	if err != nil {
		return IssuerHomeDomain{}, fmt.Errorf("%w: %w", ErrLCMUnavailable, err)
	}
	if !hd.Observed {
		return IssuerHomeDomain{}, nil
	}
	return hd, nil
}

// HomeDomainsFor is [LCMHomeDomainResolver.HomeDomainFor] for many issuers
// in one store read. Unobserved issuers are absent from the map.
func (r *LCMHomeDomainResolver) HomeDomainsFor(ctx context.Context, issuers []string) (map[string]IssuerHomeDomain, error) {
	hds, err := r.store.HomeDomainsAtOrBefore(ctx, issuers, observerLatestLedger)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrLCMUnavailable, err)
	}
	return hds, nil
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

// ChainedHomeDomainBatch is [ChainedHomeDomainLookup] for a page of
// issuers: one bounded store read instead of one per row. The result
// holds only issuers with a known domain.
func ChainedHomeDomainBatch(
	live *LCMHomeDomainResolver,
	static func(issuer string) (string, bool),
	warnFn func(msg string, kv ...any),
) func(ctx context.Context, issuers []string) map[string]string {
	return func(ctx context.Context, issuers []string) map[string]string {
		out := make(map[string]string, len(issuers))
		if len(issuers) == 0 {
			return out
		}
		observed := observeHomeDomains(ctx, live, issuers, warnFn)
		for _, issuer := range issuers {
			var (
				d  string
				ok bool
			)
			if hd := observed[issuer]; hd.Observed {
				d, ok = hd.Domain, hd.Domain != ""
			} else {
				d, ok = static(issuer)
			}
			if ok {
				out[issuer] = d
			}
		}
		return out
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
		obs.APILCMHomeDomainFallbackTotal.Inc()
		if warnFn != nil {
			warnFn("LCM home-domain resolver failed; falling back to static map",
				"issuer", issuer, "err", err)
		}
		return IssuerHomeDomain{}
	}
	return hd
}

// observeHomeDomains is [observeHomeDomain] for many issuers in one read;
// a storage error reports every issuer as unobserved.
func observeHomeDomains(ctx context.Context, live *LCMHomeDomainResolver, issuers []string, warnFn func(msg string, kv ...any)) map[string]IssuerHomeDomain {
	ctx, cancel := contextWithTimeoutMs(ctx, lcmLookupTimeoutMs)
	defer cancel()
	hds, err := live.HomeDomainsFor(ctx, issuers)
	if err != nil {
		obs.APILCMHomeDomainFallbackTotal.Inc()
		if warnFn != nil {
			warnFn("LCM home-domain resolver failed; falling back to static map",
				"issuers", len(issuers), "err", err)
		}
		return nil
	}
	return hds
}
