// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"fmt"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const crowdedIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// crowdedIssuerAssets mimics ListAssetsExt's SQL over one issuer of 60 assets, by 24h volume desc: exact Code filter, Limit
// clamp. The wanted twin has the least volume, so an issuer-only page of 50 cannot reach it.
type crowdedIssuerAssets struct{ AssetsReader }

func (crowdedIssuerAssets) ListAssetsExt(_ context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	var out []timescale.AssetRow
	for i := range 60 {
		code := fmt.Sprintf("TK%02d", i)
		if i == 59 {
			code = "USDC"
		}
		if opts.Issuer != crowdedIssuer || (opts.Code != "" && opts.Code != code) {
			continue
		}
		out = append(out, timescale.AssetRow{AssetID: code + "-" + crowdedIssuer, Code: code})
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 100
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func TestLookupCatalogueTwin_FindsTwinOfCrowdedIssuer(t *testing.T) {
	s := &Server{Options: Options{AssetsReader: crowdedIssuerAssets{}}}
	want := "USDC-" + crowdedIssuer
	row, unread := s.lookupCatalogueTwin(context.Background(), want)
	if unread {
		t.Fatal("unread on a read that answered")
	}
	if row == nil || row.AssetID != want {
		t.Fatalf("twin = %+v, want %s", row, want)
	}
}
