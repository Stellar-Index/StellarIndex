// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The realistic USDC classic + a valid, distinct C-strkey standing in for
// its SAC wrapper (same fixtures the canonical alias-registry tests use).
const (
	foldUSDCIssuer  = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	foldUSDCClassic = "USDC-" + foldUSDCIssuer
	foldUSDCSAC     = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"
)

// installFoldRegistry installs a process AliasRegistry carrying the USDC↔SAC
// wrapper and resets to the XLM-only default on cleanup. NOT parallel: the
// registry is process-global (see canonical.InstallAliasRegistry).
func installFoldRegistry(t *testing.T) {
	t.Helper()
	reg, err := canonical.NewAliasRegistry(map[string]string{foldUSDCSAC: "USDC:" + foldUSDCIssuer})
	if err != nil {
		t.Fatalf("NewAliasRegistry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })
}

func i64(v int64) *int64    { return &v }
func strp(v string) *string { return &v }

// TestFoldAliasTwins_MergesSACIntoClassic is the "USDC shows twice" fix:
// a SAC-form base row and its classic asset in the same listing input fold
// into ONE canonical (classic) row whose 24h volume + trade count are the
// SUM of both forms; the SAC standalone row is suppressed.
//
// RED without foldAliasTwins: the input is returned unchanged — two rows
// (classic USDC and the CCW67T… SAC), the second the leaking duplicate.
func TestFoldAliasTwins_MergesSACIntoClassic(t *testing.T) {
	installFoldRegistry(t)
	s := &Server{}

	rows := []AssetDetail{
		{AssetID: foldUSDCClassic, Code: "USDC", VolumeUSD24h: strp("35600000"), TradeCount24h: i64(5000)},
		{AssetID: foldUSDCSAC, Code: "USDC", VolumeUSD24h: strp("9000000"), TradeCount24h: i64(3000)},
	}

	out, _ := s.foldAliasTwins(rows)

	if len(out) != 1 {
		t.Fatalf("fold returned %d rows, want 1 merged canonical row: %+v", len(out), out)
	}
	if out[0].AssetID != foldUSDCClassic {
		t.Errorf("surviving row asset_id = %q, want the classic canonical %q", out[0].AssetID, foldUSDCClassic)
	}
	if out[0].VolumeUSD24h == nil || *out[0].VolumeUSD24h != "44600000" {
		t.Errorf("merged 24h volume = %v, want the SUM \"44600000\" (35.6M classic + 9M SAC)", out[0].VolumeUSD24h)
	}
	if out[0].TradeCount24h == nil || *out[0].TradeCount24h != 8000 {
		t.Errorf("merged trade count = %v, want the SUM 8000 (5000 + 3000)", out[0].TradeCount24h)
	}
}

// TestFoldAliasTwins_SuppressesLoneSAC: a SAC row whose canonical classic
// twin is NOT in the page (it was the verified-catalogue row served in the
// catalogue phase) is still dropped — a stray SAC row is the duplicate the
// fold removes. An unrelated asset is left untouched.
//
// RED without foldAliasTwins: the SAC row survives as its own directory row.
func TestFoldAliasTwins_SuppressesLoneSAC(t *testing.T) {
	installFoldRegistry(t)
	s := &Server{}

	rows := []AssetDetail{
		{AssetID: foldUSDCSAC, Code: "USDC", VolumeUSD24h: strp("9000000")},
		{AssetID: "AQUA-" + foldUSDCIssuer, Code: "AQUA", VolumeUSD24h: strp("1234")},
	}

	out, _ := s.foldAliasTwins(rows)

	if len(out) != 1 {
		t.Fatalf("fold returned %d rows, want 1 (lone SAC suppressed): %+v", len(out), out)
	}
	if out[0].Code != "AQUA" {
		t.Errorf("surviving row = %q, want the unrelated AQUA row (the lone SAC must be dropped)", out[0].AssetID)
	}
}

// TestFoldAliasTwins_FractionalVolumesSumExactly guards the ADR-0003 exact
// arithmetic in addDecimalStrings: fractional NUMERIC volumes sum without
// float drift and keep their fractional precision.
func TestFoldAliasTwins_FractionalVolumesSumExactly(t *testing.T) {
	installFoldRegistry(t)
	s := &Server{}

	rows := []AssetDetail{
		{AssetID: foldUSDCClassic, VolumeUSD24h: strp("0.1")},
		{AssetID: foldUSDCSAC, VolumeUSD24h: strp("0.2")},
	}
	out, _ := s.foldAliasTwins(rows)
	if len(out) != 1 || out[0].VolumeUSD24h == nil || *out[0].VolumeUSD24h != "0.3" {
		t.Fatalf("0.1 + 0.2 folded volume = %v, want exact \"0.3\" (big.Rat, not float)", out[0].VolumeUSD24h)
	}
}

// offPageSpine is a listing spine whose page carries only `page`, while
// the point reader also resolves `offPage`: the rows the volume-ranked
// keyset put on some other page of the same listing.
type offPageSpine struct {
	*armFilteringAssets
	offPage []timescale.AssetRow
}

func (a *offPageSpine) GetAssetByAssetID(ctx context.Context, assetID string) (timescale.AssetRow, error) {
	for _, row := range a.offPage {
		if row.AssetID == assetID {
			return row, nil
		}
	}
	return a.armFilteringAssets.GetAssetByAssetID(ctx, assetID)
}

// TestAssetsUnifiedFoldsContractArmWhateverItsPage pins CA2-A01-correct-4.
// A Soroban-heavy wrapper outranks its own classic row on the
// volume-desc spine, so the two land out of order on one page or on
// different pages. Either way the classic row must publish the classic +
// contract sum on an unfiltered request, and a wrapper merged in-page
// must not be added a second time by the off-page point read.
func TestAssetsUnifiedFoldsContractArmWhateverItsPage(t *testing.T) {
	installSpineFoldRegistry(t)
	classic := timescale.AssetRow{AssetID: spineClassic, Code: "TESTX", IssuerGStrkey: spineIssuer, Volume24hUSD: strp("1000")}
	sac := timescale.AssetRow{AssetID: spineSAC, Volume24hUSD: strp("400000")}

	for _, tc := range []struct {
		name  string
		spine AssetsReader
	}{
		{
			name:  "wrapper ranks above its classic row on the same page",
			spine: &armFilteringAssets{rows: []timescale.AssetRow{sac, classic}},
		},
		{
			name: "wrapper paged elsewhere",
			spine: &offPageSpine{
				armFilteringAssets: &armFilteringAssets{rows: []timescale.AssetRow{classic}},
				offPage:            []timescale.AssetRow{sac},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := shippedServer(t, tc.spine)
			rows := decodeAssetRows(t, serveAssets(t, s, "/v1/assets?asset_class=all&limit=200"))
			if got := volumeOf(rowByAssetID(t, rows, spineClassic)); got != "401000" {
				t.Errorf("classic row volume_24h_usd = %q, want \"401000\" (1000 classic + 400000 contract arm, once)", got)
			}
			for _, row := range rows {
				if row.AssetID == spineSAC {
					t.Errorf("SAC wrapper served as its own row; it must fold onto %s", spineClassic)
				}
			}
		})
	}

	t.Run("type=classic keeps the classic arm alone", func(t *testing.T) {
		s := shippedServer(t, &offPageSpine{
			armFilteringAssets: &armFilteringAssets{rows: []timescale.AssetRow{classic}},
			offPage:            []timescale.AssetRow{sac},
		})
		rows := decodeAssetRows(t, serveAssets(t, s, "/v1/assets?asset_class=all&type=classic&limit=200"))
		if got := volumeOf(rowByAssetID(t, rows, spineClassic)); got != "1000" {
			t.Errorf("type=classic volume_24h_usd = %q, want the classic arm \"1000\"", got)
		}
	})
}
