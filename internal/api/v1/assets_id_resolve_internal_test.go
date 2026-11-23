// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// anyVariantSlugResolver answers every slug with one fixed case variant, as the store's lower(slug) match may when several
// case variants of a code share an issuer.
type anyVariantSlugResolver struct {
	AssetReader
	code, issuer string
}

func (r anyVariantSlugResolver) ClassicAssetBySlug(context.Context, string) (string, string, bool, error) {
	return r.code, r.issuer, true, nil
}

// A case-folded issuer names the asset whose code is spelled as typed: USDC-<issuer, any case> is USDC, never the trustline-only
// usdc the store's fold could return.
func TestResolveClassicSlug_FoldsTheIssuerNeverTheCode(t *testing.T) {
	const issuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	s := &Server{Options: Options{Assets: anyVariantSlugResolver{code: "usdc", issuer: issuer}}}
	s.logger = discardLogger()
	for raw, want := range map[string]string{
		"USDC-" + strings.ToLower(issuer): "USDC-" + issuer,
		"usdc-" + strings.ToLower(issuer): "usdc-" + issuer,
	} {
		got, ok := s.resolveClassicSlug(context.Background(), raw)
		if !ok || got.String() != want {
			t.Errorf("%s resolved to %q (ok %v), want %q", raw, got.String(), ok, want)
		}
	}
}

// A SAC-address search whose lake lookup failed cannot know what it searches for: it must answer 503, not search the raw
// address and serve (and cache) a clean page missing the classic asset.
func TestAssetsQSACLookupFailureIsUnavailable(t *testing.T) {
	stub := &filterCapturingAssets{}
	s := &Server{Options: Options{AssetsReader: stub, Explorer: sacLookupExplorer{err: errors.New("lake read timed out")}}}
	s.logger = discardLogger()
	rec := httptest.NewRecorder()
	s.handleAssetList(rec, httptest.NewRequest(http.MethodGet,
		"/v1/assets?q=CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75&limit=3", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
}
