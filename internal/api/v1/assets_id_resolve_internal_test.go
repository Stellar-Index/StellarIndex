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
	"time"
)

// registrySlugResolver answers only the asset_ids it holds, as the store does: the exact slug, else the single case-folded
// match, else no match (several case variants and no exact one included).
type registrySlugResolver struct {
	AssetReader
	ids []string
}

func (r registrySlugResolver) ClassicAssetBySlug(_ context.Context, slug string) (string, string, bool, error) {
	var folded []string
	for _, id := range r.ids {
		if id == slug {
			folded = []string{id}
			break
		}
		if strings.EqualFold(id, slug) {
			folded = append(folded, id)
		}
	}
	if len(folded) != 1 {
		return "", "", false, nil
	}
	i := strings.LastIndexByte(folded[0], '-')
	return folded[0][:i], folded[0][i+1:], true, nil
}

// An id resolves to the code spelled as typed when that asset exists, else to its one case variant, and never to an
// arbitrary pick between two variants; the issuer's case never matters.
func TestResolveClassicSlug_ExactCodeWinsElseTheOneVariant(t *testing.T) {
	const issuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	lower, mangled := strings.ToLower(issuer), strings.ToLower(issuer[:20])+issuer[20:]
	upperID, lowerID := "USDC-"+issuer, "usdc-"+issuer
	for _, tc := range []struct {
		name, raw string
		ids       []string
		want      string // "" = refused
	}{
		{"only USDC, typed lower", "usdc-" + lower, []string{upperID}, upperID},
		{"both, typed USDC", "USDC-" + lower, []string{upperID, lowerID}, upperID},
		{"both, typed usdc", "usdc-" + lower, []string{upperID, lowerID}, lowerID},
		{"both, typed Usdc", "Usdc-" + lower, []string{upperID, lowerID}, ""},
		{"both, typed USDC with a mangled issuer", "USDC-" + mangled, []string{lowerID, upperID}, upperID},
		{"none", "usdc-" + lower, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{Options: Options{Assets: registrySlugResolver{ids: tc.ids}}}
			s.logger = discardLogger()
			got, ok, err := s.resolveClassicSlug(context.Background(), tc.raw)
			if err != nil {
				t.Fatalf("%s: unexpected read error %v", tc.raw, err)
			}
			if tc.want == "" {
				if ok {
					t.Errorf("%s resolved to %q, want refused", tc.raw, got.String())
				}
				return
			}
			if !ok || got.String() != tc.want {
				t.Errorf("%s resolved to %q (ok %v), want %q", tc.raw, got.String(), ok, tc.want)
			}
		})
	}
}

// A SAC-address search whose lake lookup failed cannot know what it searches for: it must answer a retryable 503, not search
// the raw address and serve (and cache) a clean page missing the classic asset; a page a resolved lookup cached still replays.
func TestAssetsQSACLookupFailureIsUnavailable(t *testing.T) {
	const path = "/v1/assets?q=CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75&limit=3"
	stub := &filterCapturingAssets{}
	s := &Server{Options: Options{AssetsReader: stub, Explorer: sacLookupExplorer{err: errors.New("lake read timed out")}}}
	s.logger = discardLogger()
	rec := httptest.NewRecorder()
	s.handleAssetList(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status = %d, Retry-After = %q; want 503 with Retry-After: %s",
			rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}

	s.assetListCache = newAssetListResponseCache(time.Minute, time.Hour)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if err := s.assetListCache.put(assetListCacheKey(req), Envelope{Data: []AssetDetail{{AssetID: "USDC-" + cetesIssuer}}}); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	s.handleAssetList(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "USDC-"+cetesIssuer) {
		t.Errorf("with a cached page: status = %d body %s; want the cached page replayed", rec.Code, rec.Body.String())
	}
}
