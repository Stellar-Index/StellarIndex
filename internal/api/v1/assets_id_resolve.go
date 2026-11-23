// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"net/http"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// classicSlugResolver is the optional reader capability behind /v1/assets/{slug} classic-slug
// resolution (the proxyPairGate idiom); a reader without it answers 400 on non-canonical ids.
type classicSlugResolver interface {
	ClassicAssetBySlug(ctx context.Context, slug string) (code, issuer string, ok bool, err error)
}

// issuerFoldedClassic reads raw as <code>-<issuer> with only the issuer case-folded: a strkey is base32 and case-free, a classic
// code is case-significant, so "usdc-<issuer>" names the asset usdc and never another case variant of it.
func issuerFoldedClassic(raw string) (canonical.Asset, bool) {
	i := strings.LastIndexByte(raw, '-')
	if i <= 0 {
		return canonical.Asset{}, false
	}
	asset, err := canonical.NewClassicAsset(raw[:i], strings.ToUpper(raw[i+1:]))
	if err != nil || asset.Validate() != nil {
		return canonical.Asset{}, false
	}
	return asset, true
}

// resolveClassicSlug maps a classic-asset slug to (code, issuer). A lookup error logs and reads as a miss (caller 400s), never
// a 500.
func (s *Server) resolveClassicSlug(ctx context.Context, raw string) (canonical.Asset, bool) {
	// The store's slug match folds the code too and returns any one case variant, so an id that names a code and an issuer
	// never reaches it.
	if asset, ok := issuerFoldedClassic(raw); ok {
		return asset, true
	}
	// Two reader seams exist (`assets` and `assetsReader`); binaries wire the same adapter behind
	// both but a test may wire only one, so probe both.
	res, ok := s.Assets.(classicSlugResolver)
	if !ok {
		res, ok = s.AssetsReader.(classicSlugResolver)
	}
	if !ok {
		return canonical.Asset{}, false
	}
	code, issuer, found, err := res.ClassicAssetBySlug(ctx, raw)
	if err != nil {
		s.logger.Warn("classic slug lookup failed", "slug", raw, "err", err)
		return canonical.Asset{}, false
	}
	if !found {
		return canonical.Asset{}, false
	}
	asset, err := canonical.NewClassicAsset(code, issuer)
	if err != nil {
		s.logger.Warn("classic slug resolved to unbuildable asset", "slug", raw, "code", code, "err", err)
		return canonical.Asset{}, false
	}
	return asset, true
}

// resolveSACToClassic maps a SAC address to the classic (or native) asset it wraps; the resolved asset's deterministic SAC
// address must equal the queried contract, so spoofed metadata cannot redirect pricing.
func (s *Server) resolveSACToClassic(ctx context.Context, contractID string) (canonical.Asset, bool) {
	asset, ok, _ := s.resolveSAC(ctx, contractID)
	return asset, ok
}

// resolveSAC is [Server.resolveSACToClassic] that also returns the lake read error, so "not a SAC" is told apart from "unknown".
func (s *Server) resolveSAC(ctx context.Context, contractID string) (canonical.Asset, bool, error) {
	name, found, err := s.Explorer.SACClassicAssetName(ctx, contractID)
	if err != nil {
		return canonical.Asset{}, false, err
	}
	if !found {
		return canonical.Asset{}, false, nil
	}
	asset, ok := sacNameVerified(name, contractID)
	return asset, ok, nil
}

// sacNameVerified parses the lake's wrapped-asset name and keeps it only if its derived SAC address is contractID; metadata
// that fails either test is a definite "not this SAC", not a read error.
func sacNameVerified(name, contractID string) (canonical.Asset, bool) {
	asset := canonical.NativeAsset()
	if name != "native" {
		code, issuer, ok := strings.Cut(name, ":")
		if !ok {
			return canonical.Asset{}, false
		}
		var err error
		if asset, err = canonical.NewClassicAsset(code, issuer); err != nil {
			return canonical.Asset{}, false
		}
	}
	derived, err := asset.SacContractID()
	if err != nil || derived != contractID {
		return canonical.Asset{}, false
	}
	return asset, true
}

// sacSearchAsClassic rewrites a q that is a classic asset's SAC address to that asset's id, so the C-form finds the rows its
// asset_id does; the SAC's own listing row carries no code or issuer to match on. ok=false means a 503 was written: an
// unread lookup leaves the query's meaning unknown, and searching the raw address would serve and cache a wrong page.
func (s *Server) sacSearchAsClassic(w http.ResponseWriter, r *http.Request, q string) (string, bool) {
	if s.Explorer == nil || !canonical.IsContractID(q) {
		return q, true
	}
	classic, ok, err := s.resolveSAC(r.Context(), q)
	if err != nil {
		if !clientAborted(r, err) {
			s.logger.Warn("SAC search lookup failed", "q", q, "err", err)
		}
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/contract-identity-unavailable",
			"Contract identity unavailable", http.StatusServiceUnavailable,
			"could not read whether this contract is a Stellar Asset Contract; retry")
		return "", false
	}
	if ok && classic.Type == canonical.AssetClassic {
		return classic.String(), true
	}
	return q, true
}
