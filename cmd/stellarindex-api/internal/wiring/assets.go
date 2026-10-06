package wiring

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// StoreAssetReader adapts *timescale.Store to v1.AssetReader. Keeps
// the typed boundary: the store returns canonical.Asset; the API
// layer owns the wire-shape conversion to v1.AssetDetail.
//
// listingHomeDomains (listing page) and detailHomeDomainLookup (GetAsset)
// are the two surface lookups from newHomeDomainLookups. An issuer with
// no known domain is absent / returns ("", false); the AssetDetail then
// has HomeDomain==nil and the overlay handler stamps
// sep1_status="not_fetched" for that case.
type StoreAssetReader struct {
	S                      *timescale.Store
	ListingHomeDomains     func(ctx context.Context, issuers []string) map[string]string
	DetailHomeDomainLookup func(ctx context.Context, issuer string) (string, bool)
}

// homeDomainLookups is the ADR-0021 home-domain chain (observation, then
// the operator-static map) split by surface. The listing has no live
// on-chain read, so it takes the whole chain. The asset-detail surfaces
// run v1's live ClickHouse AccountEntry read, which must outrank the
// static map: detail carries only the observation layer, and static is
// handed to v1 to consult after that read.

// ClassicAssetBySlug satisfies v1's optional classicSlugResolver
// capability — /v1/assets/{slug} resolution for the migration-0134
// public slugs. Pure delegation to the store.
func (r StoreAssetReader) ClassicAssetBySlug(ctx context.Context, slug string) (string, string, bool, error) {
	return r.S.ClassicAssetBySlug(ctx, slug)
}

func (r StoreAssetReader) ListAssets(ctx context.Context, cursor string, limit int) ([]v1.AssetDetail, string, error) {
	assets, next, err := r.S.DistinctAssets(ctx, cursor, limit)
	if err != nil {
		return nil, "", err
	}
	return AssetsToDetails(ctx, assets, r.ListingHomeDomains), next, nil
}

// AssetsToDetails resolves a listing page's issuer home domains in one
// batch read, then maps each asset through AssetToDetail.
func AssetsToDetails(ctx context.Context, assets []canonical.Asset, homeDomains func(ctx context.Context, issuers []string) map[string]string) []v1.AssetDetail {
	var lookup func(ctx context.Context, issuer string) (string, bool)
	if homeDomains != nil {
		seen := make(map[string]bool, len(assets))
		issuers := make([]string, 0, len(assets))
		for _, a := range assets {
			if a.Issuer != "" && !seen[a.Issuer] {
				seen[a.Issuer] = true
				issuers = append(issuers, a.Issuer)
			}
		}
		domains := homeDomains(ctx, issuers)
		lookup = func(_ context.Context, issuer string) (string, bool) {
			d, ok := domains[issuer]
			return d, ok
		}
	}
	out := make([]v1.AssetDetail, len(assets))
	for i, a := range assets {
		out[i] = AssetToDetail(ctx, a, lookup)
	}
	return out
}

func (r StoreAssetReader) GetAsset(ctx context.Context, a canonical.Asset) (v1.AssetDetail, error) {
	has, err := r.S.HasAsset(ctx, a)
	if err != nil {
		return v1.AssetDetail{}, err
	}
	if !has {
		return v1.AssetDetail{}, v1.ErrAssetNotFound
	}
	detail := AssetToDetail(ctx, a, r.DetailHomeDomainLookup)

	// Best-effort F2 enrichment from the per-asset stats lookup
	// — same data the /v1/coins listing carries. Failures here
	// don't break the detail response; the field stays null and
	// the rest of the body still serves cleanly. The proper
	// supply pipeline (asset_supply_history) will overwrite
	// these when it has a snapshot — populateF2Fields runs
	// AFTER us in the handler stack.
	if stats, err := r.S.LatestAssetStats(ctx, a.String()); err == nil {
		if detail.VolumeUSD24h == nil && stats.Volume24hUSD != nil {
			detail.VolumeUSD24h = stats.Volume24hUSD
		}
		if detail.CirculatingSupply == nil && stats.CirculatingSupply != nil {
			detail.CirculatingSupply = stats.CirculatingSupply
		}
		if detail.MarketCapUSD == nil && stats.MarketCapUSD != nil {
			detail.MarketCapUSD = stats.MarketCapUSD
		}
	}
	return detail, nil
}

// CachedAssetReader / CachedMarketsReader — Redis read-through
// caches for the catalogue list endpoints. Same shape as
// CachedOracleReader: deserialise on hit, hit-the-DB-then-SET on
// miss, fall through on error. Single-asset / single-pair lookups
// pass through unchanged — they're already fast and benefit less
// from caching.

type listCachePayload[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next"`
}

type CachedAssetReader struct {
	Inner v1.AssetReader
	RDB   redis.UniversalClient
	Log   *slog.Logger
}

// ClassicAssetBySlug forwards the optional classicSlugResolver
// capability through the cache wrapper — a wrapping reader that
// swallowed it would silently 400 every slug URL in production while
// tests against the bare reader stayed green (the exact
// capability-erasure bug class the mutation audit flagged). Uncached:
// the lookup is a single unique-index point read.
func (r CachedAssetReader) ClassicAssetBySlug(ctx context.Context, slug string) (string, string, bool, error) {
	res, ok := r.Inner.(interface {
		ClassicAssetBySlug(ctx context.Context, slug string) (string, string, bool, error)
	})
	if !ok {
		return "", "", false, nil
	}
	return res.ClassicAssetBySlug(ctx, slug)
}

func (r CachedAssetReader) GetAsset(ctx context.Context, a canonical.Asset) (v1.AssetDetail, error) {
	return r.Inner.GetAsset(ctx, a)
}

func (r CachedAssetReader) ListAssets(ctx context.Context, cursor string, limit int) ([]v1.AssetDetail, string, error) {
	if r.RDB == nil {
		return r.Inner.ListAssets(ctx, cursor, limit)
	}
	cacheKey := cachekeys.AssetsList(cursor, limit)
	if raw, err := r.RDB.Get(ctx, cacheKey.String()).Bytes(); err == nil {
		var p listCachePayload[v1.AssetDetail]
		if jerr := json.Unmarshal(raw, &p); jerr == nil {
			return p.Items, p.NextCursor, nil
		}
		r.Log.Warn("assets cache decode failed", "key", cacheKey)
	} else if !errors.Is(err, redis.Nil) {
		r.Log.Warn("assets cache read failed", "key", cacheKey, "err", err)
	}

	items, next, err := r.Inner.ListAssets(ctx, cursor, limit)
	if err != nil {
		return nil, "", err
	}
	if buf, jerr := json.Marshal(listCachePayload[v1.AssetDetail]{Items: items, NextCursor: next}); jerr == nil {
		if serr := r.RDB.Set(ctx, cacheKey.String(), buf, cachekeys.CatalogueListTTL).Err(); serr != nil {
			r.Log.Warn("assets cache write failed", "key", cacheKey, "err", serr)
		}
	}
	return items, next, nil
}

// AssetToDetail converts canonical.Asset → v1.AssetDetail. Nullable
// fields become nil pointers when empty so the JSON omits them.
//
// homeDomainLookup populates HomeDomain for classic assets whose
// issuer has a known home_domain (one of the homeDomainLookups). When
// one is known, the
// SEP-1 overlay handler downstream resolves stellar.toml and fills the
// overlay fields; otherwise HomeDomain stays nil and the handler
// stamps sep1_status="not_fetched". Pass nil for the lookup if the
// caller doesn't have one (tests + scaffolding paths).
//
// SAC-wrapped classics + Soroban tokens have no issuer in the
// classic sense — HomeDomain stays nil; sep1_status falls through
// to "not_applicable" via the handler.
func AssetToDetail(ctx context.Context, a canonical.Asset, homeDomainLookup func(ctx context.Context, issuer string) (string, bool)) v1.AssetDetail {
	d := v1.AssetDetail{
		AssetID: a.String(),
		Type:    string(a.Type),
		Code:    a.Code,
		// Classic + native are 7 by protocol (stroops). Soroban tokens get
		// their real on-chain decimals() overlaid by the v1 handler
		// (applyTokenDecimals, reading the lake's instance METADATA).
		Decimals:   7,
		Sep1Status: "not_applicable",
	}
	if a.Issuer != "" {
		v := a.Issuer
		d.Issuer = &v
		// Classic asset with a known issuer — try the curated lookup
		// to populate HomeDomain. The handler's overlay logic takes
		// over from here: with HomeDomain set + s.meta wired,
		// applySep1Overlay runs and stamps the resulting status; with
		// HomeDomain set + s.meta nil, the handler stamps "not_fetched".
		if homeDomainLookup != nil {
			if hd, ok := homeDomainLookup(ctx, a.Issuer); ok {
				d.HomeDomain = &hd
				// Clear the "not_applicable" so the handler's overlay
				// logic kicks in. The handler stamps the right value
				// based on overlay outcome.
				d.Sep1Status = ""
			}
		}
	}
	if a.ContractID != "" {
		v := a.ContractID
		d.ContractID = &v
	}
	return d
}
