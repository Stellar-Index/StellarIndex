// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/holds"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	heldListingAsset   = "FOO-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	unheldListingAsset = "BAR-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
)

// listingRowsByID serves GET /v1/assets through the mounted route and indexes its rows by asset_id.
func listingRowsByID(t *testing.T, s *Server) (map[string]json.RawMessage, map[string]map[string]json.RawMessage) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/assets?asset_class=all&limit=200", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
	}
	var env struct {
		Data  []map[string]json.RawMessage `json:"data"`
		Flags map[string]json.RawMessage   `json:"flags"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	rows := map[string]map[string]json.RawMessage{}
	for _, row := range env.Data {
		var id string
		_ = json.Unmarshal(row["asset_id"], &id)
		rows[id] = row
	}
	return env.Flags, rows
}

// TestUnderReview_ListingMarksEachHeldRow: a held asset is marked on the listing as on its detail, on its own row only, and the
// marker follows the hold list rather than any cached body.
func TestUnderReview_ListingMarksEachHeldRow(t *testing.T) {
	s := shippedServer(t, &armFilteringAssets{rows: []timescale.AssetRow{
		{AssetID: heldListingAsset, Code: "FOO", IssuerGStrkey: "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", Volume24hUSD: strp("10")},
		{AssetID: unheldListingAsset, Code: "BAR", IssuerGStrkey: "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", Volume24hUSD: strp("5")},
	}})
	if _, rows := listingRowsByID(t, s); rows[heldListingAsset] == nil || rows[heldListingAsset]["under_review"] != nil {
		t.Fatalf("before any hold: row %s", rows[heldListingAsset])
	}

	s.SetHolds([]holds.Hold{{Asset: heldListingAsset, Reason: "issuer under investigation"}})
	flags, rows := listingRowsByID(t, s)
	held, other := rows[heldListingAsset], rows[unheldListingAsset]
	if string(held["under_review"]) != "true" || string(held["under_review_reason"]) != `"issuer under investigation"` {
		t.Fatalf("held row under_review=%s reason=%s, want true and the hold's reason", held["under_review"], held["under_review_reason"])
	}
	if other == nil || other["under_review"] != nil || other["under_review_reason"] != nil {
		t.Fatalf("unheld row marked: %s", other)
	}
	if string(flags["under_review"]) != "true" {
		t.Fatalf("flags.under_review = %s, want true", flags["under_review"])
	}

	s.SetHolds(nil)
	if _, rows := listingRowsByID(t, s); rows[heldListingAsset]["under_review"] != nil {
		t.Fatalf("hold lifted but row still marked: %s", rows[heldListingAsset])
	}
}
