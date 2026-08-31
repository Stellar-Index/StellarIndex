// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Regression suite for wave-D RD-02: /v1/assets accepted `order_by`
// and never read it.
//
// The storage layer had supported AssetsOrderVolume24hUSDDesc all
// along — its own ORDER BY branch, keyset cursor args, cursor
// predicate and rank-tier expression, and the unified path passes it.
// Only the wire-to-store wiring was missing, so:
//
//   - the explorer home page requested
//     `?limit=10&order_by=volume_24h_usd_desc`, was served the top ten
//     by ALL-TIME observation count, and re-sorted just those ten
//     client-side under the caption "Ranked by trailing-24h trading
//     volume across every venue we ingest";
//   - an asset with $2M of 24h volume but a modest lifetime count
//     could not enter the candidate set at all, while a dormant
//     high-lifetime-count asset held a slot and rendered as a dash;
//   - `order_by=TOTAL_GARBAGE` returned 200, unlike /v1/markets which
//     has always 400'd on an unrecognised value.
//
// These tests pin the request→store mapping directly, because RD-02
// existed precisely for want of that pin: every layer worked in
// isolation and nothing asserted they were connected.

// orderCapturingAssets records the ListAssetsOptions the handler built.
type orderCapturingAssets struct {
	AssetsReader
	got  timescale.ListAssetsOptions
	seen bool
}

func (a *orderCapturingAssets) ListAssetsExt(
	_ context.Context, opts timescale.ListAssetsOptions,
) ([]timescale.AssetRow, error) {
	a.got, a.seen = opts, true
	return nil, nil
}

func (a *orderCapturingAssets) GetAssetsPriceHistory24hBatch(
	context.Context, []string,
) (map[string][]timescale.AssetPricePoint, error) {
	return nil, nil
}

func (a *orderCapturingAssets) GetAssetsATHBatch(
	context.Context, []string,
) (map[string]timescale.AssetATH, error) {
	return nil, nil
}

func serveAssetList(t *testing.T, target string) (*orderCapturingAssets, *httptest.ResponseRecorder) {
	t.Helper()
	stub := &orderCapturingAssets{}
	s := &Server{assetsReader: stub}
	rec := httptest.NewRecorder()
	s.handleAssetList(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return stub, rec
}

// TestAssetsOrderByReachesTheStore is the pin the finding asked for.
func TestAssetsOrderByReachesTheStore(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
		want   timescale.AssetsOrder
	}{
		{
			// The explorer home page's exact request.
			name:   "volume_24h_usd_desc",
			target: "/v1/assets?limit=10&order_by=volume_24h_usd_desc",
			want:   timescale.AssetsOrderVolume24hUSDDesc,
		},
		{
			name:   "observation_count_desc",
			target: "/v1/assets?limit=10&order_by=observation_count_desc",
			want:   timescale.AssetsOrderObservationCountDesc,
		},
		{
			// Omitted keeps the long-standing default. Changing what an
			// existing caller gets is not part of this fix.
			name:   "omitted defaults to observation count",
			target: "/v1/assets?limit=10",
			want:   timescale.AssetsOrderObservationCountDesc,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub, rec := serveAssetList(t, tc.target)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
			}
			if !stub.seen {
				t.Fatal("handler never reached the store")
			}
			if stub.got.Order != tc.want {
				t.Errorf("store saw Order = %v, want %v — the handler is not "+
					"threading order_by into ListAssetsOptions, which is RD-02 "+
					"exactly: the request is accepted and the ranking is not "+
					"the one asked for", stub.got.Order, tc.want)
			}
		})
	}
}

// TestAssetsOrderByGarbageIs400 — an unrecognised value must fail
// loudly, the way /v1/markets always has. Pre-fix it returned 200 and
// the observation-count ordering, so a client typo was indistinguishable
// from a served ranking.
func TestAssetsOrderByGarbageIs400(t *testing.T) {
	stub, rec := serveAssetList(t, "/v1/assets?order_by=TOTAL_GARBAGE")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if stub.seen {
		t.Error("handler hit the store despite invalid input; bad input must " +
			"400 before any DB round-trip")
	}
	var problem map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if problem["title"] != "Invalid order_by" {
		t.Errorf("problem title = %v, want \"Invalid order_by\"", problem["title"])
	}
}

// TestAssetsOrderByWithAssetClassIs400 — the catalogue and unified
// listings rank on their own fixed schemes, each with a keyset cursor
// encoding THAT scheme's keys, so they cannot honour a caller-chosen
// order. They say so rather than accepting the parameter and ignoring
// it; silently ignoring is the defect this whole change is about.
//
// No existing caller regresses: of the explorer's four callers, only
// the home page sends order_by, and it sends no asset_class.
func TestAssetsOrderByWithAssetClassIs400(t *testing.T) {
	for _, class := range []string{"all", "fiat", "stablecoin", "crypto"} {
		t.Run(class, func(t *testing.T) {
			_, rec := serveAssetList(t,
				"/v1/assets?asset_class="+class+"&order_by=volume_24h_usd_desc")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("asset_class=%s + order_by returned %d, want 400 — "+
					"accepting an order this path cannot honour is how RD-02 "+
					"happened", class, rec.Code)
			}
		})
	}
}

// TestAssetsAssetClassWithoutOrderByStillServes guards the blast radius
// of the rule above: the asset_class listings are the explorer's
// /assets page, and they must keep working when no order_by is sent.
func TestAssetsAssetClassWithoutOrderByStillServes(t *testing.T) {
	for _, class := range []string{"all", "fiat", "stablecoin", "crypto"} {
		t.Run(class, func(t *testing.T) {
			_, rec := serveAssetList(t, "/v1/assets?asset_class="+class)
			if rec.Code == http.StatusBadRequest {
				t.Fatalf("asset_class=%s alone returned 400 — the order_by rule "+
					"must not touch requests that do not send one: %s",
					class, rec.Body.String())
			}
		})
	}
}

// TestAssetsCursorIsEncodedUnderTheActiveOrder — the two orders have
// DIFFERENT keyset shapes (`<rank_tier>:<observation_count>:<asset_id>`
// vs `<rank_tier>:<vol_or_blank>:<asset_id>`). Validating an incoming
// cursor under the wrong order rejects a good one; encoding the next
// one under the wrong order emits keys the following page's predicate
// reads positionally as something else, which skips or repeats rows
// instead of erroring. So the order must reach BOTH cursor calls, not
// just the query.
func TestAssetsCursorIsEncodedUnderTheActiveOrder(t *testing.T) {
	// A cursor valid under the volume order must be accepted when that
	// order is the one requested.
	volCursor := timescale.EncodeAssetsCursor(
		timescale.AssetRow{AssetID: "USDC-GA5Z"}, timescale.AssetsOrderVolume24hUSDDesc)

	stub, rec := serveAssetList(t,
		"/v1/assets?order_by=volume_24h_usd_desc&cursor="+volCursor)
	if rec.Code != http.StatusOK {
		t.Fatalf("a volume-order cursor was rejected under order_by="+
			"volume_24h_usd_desc: %d %s", rec.Code, rec.Body.String())
	}
	if !stub.seen {
		t.Fatal("handler never reached the store")
	}
	if stub.got.Cursor != volCursor {
		t.Errorf("store saw cursor %q, want %q", stub.got.Cursor, volCursor)
	}
}
