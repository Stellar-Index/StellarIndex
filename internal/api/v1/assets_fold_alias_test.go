// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The realistic USDC classic and its pubnet SAC wrapper
// (same fixtures the canonical alias-registry tests use).
const (
	foldUSDCIssuer  = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	foldUSDCClassic = "USDC-" + foldUSDCIssuer
	foldUSDCSAC     = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
)

// installFoldRegistry installs a process AliasRegistry carrying the USDC↔SAC
// wrapper and resets to the XLM-only default on cleanup. NOT parallel: the
// registry is process-global (see canonical.InstallAliasRegistry).
func installFoldRegistry(t *testing.T) {
	t.Helper()
	reg, err := canonical.NewAliasRegistry(canonical.PubnetPassphrase, map[string]string{foldUSDCSAC: "USDC:" + foldUSDCIssuer})
	if err != nil {
		t.Fatalf("NewAliasRegistry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })
}

func i64(v int64) *int64    { return &v }
func strp(v string) *string { return &v }

// TestFoldAliasTwins_MergesSACIntoClassic: a SAC-form row and its classic asset in the same listing fold into one classic row
// whose 24h volume and trade count are the sum of both forms; the standalone SAC row is suppressed.
func TestFoldAliasTwins_MergesSACIntoClassic(t *testing.T) {
	installFoldRegistry(t)
	s := &Server{}

	rows := []AssetDetail{
		{AssetID: foldUSDCClassic, Code: "USDC", VolumeUSD24h: strp("35600000"), TradeCount24h: i64(5000)},
		{AssetID: foldUSDCSAC, Code: "USDC", VolumeUSD24h: strp("9000000"), TradeCount24h: i64(3000)},
	}

	out, _ := s.foldAliasTwins(context.Background(), rows)

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

// TestFoldAliasTwins_SuppressesLoneSAC: a SAC row whose classic twin is not in the page (it was served in the catalogue phase) is
// still dropped as a duplicate; an unrelated asset is untouched.
func TestFoldAliasTwins_SuppressesLoneSAC(t *testing.T) {
	installFoldRegistry(t)
	s := &Server{}

	rows := []AssetDetail{
		{AssetID: foldUSDCSAC, Code: "USDC", VolumeUSD24h: strp("9000000")},
		{AssetID: "AQUA-" + foldUSDCIssuer, Code: "AQUA", VolumeUSD24h: strp("1234")},
	}

	out, _ := s.foldAliasTwins(context.Background(), rows)

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
	out, _ := s.foldAliasTwins(context.Background(), rows)
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

// TestAssetsUnifiedFoldsContractArmWhateverItsPage: a Soroban-heavy wrapper can outrank its classic row, landing on the same or a
// different page. Either way the classic row must publish classic + contract on an unfiltered request, and a wrapper merged in-
// page must not be added again by the off-page point read.
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

// sacStubReader lets each test choose what the lake claims the
// contract's SAC metadata name is.
type sacStubReader struct {
	ExplorerReader // nil — only SACClassicAssetName is called by the code under test
	name           string
	found          bool
}

func (s *sacStubReader) SACClassicAssetName(context.Context, string) (string, bool, error) {
	return s.name, s.found, nil
}

// TestResolveSACToClassic_Genuine pins the happy path: the USDC SAC
// resolves to the classic USDC identity because the metadata name
// re-derives to the queried address.
func TestResolveSACToClassic_Genuine(t *testing.T) {
	s := &Server{Options: Options{Explorer: &sacStubReader{name: "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", found: true}}}
	got, ok := s.resolveSACToClassic(context.Background(), "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75")
	if !ok || got.Code != "USDC" {
		t.Fatalf("resolve = %+v ok=%v, want classic USDC", got, ok)
	}
}

// TestResolveSACToClassic_SpoofedName pins the defence: a contract
// whose metadata CLAIMS to be USDC but whose address does not
// re-derive from that asset must NOT redirect pricing.
func TestResolveSACToClassic_SpoofedName(t *testing.T) {
	s := &Server{Options: Options{Explorer: &sacStubReader{name: "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", found: true}}}
	if _, ok := s.resolveSACToClassic(context.Background(), "CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN"); ok {
		t.Fatal("spoofed metadata name redirected pricing — derivation cross-check failed")
	}
}

func TestResolveSACToClassic_Native(t *testing.T) {
	s := &Server{Options: Options{Explorer: &sacStubReader{name: "native", found: true}}}
	got, ok := s.resolveSACToClassic(context.Background(), "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA")
	if !ok || got.Type != canonical.AssetNative {
		t.Fatalf("native SAC resolve = %+v ok=%v", got, ok)
	}
}

const (
	foldSUSDClassic = "sUSD-GCHW7CWI7GMIYQYFXMFJNJX5645XGWIINIAEQK3SABQO6CAYL5T7JYIH"
	foldSUSDSAC     = "CDFZUVS5YNLXU7VENKOUDEOHCJGKQNVUBWD7KMN6E7ZROKPYPFLRUJFG"
)

// TestFoldAliasTwins_DerivedSACWithoutRegistryEntry: a classic asset's SAC folds into it with no registry wrapper entry, because
// the on-page classic row derives that address; the merged row ranks where the class first appeared.
func TestFoldAliasTwins_DerivedSACWithoutRegistryEntry(t *testing.T) {
	s := &Server{}
	rows := []AssetDetail{
		{AssetID: foldSUSDSAC, VolumeUSD24h: strp("1257976"), TradeCount24h: i64(40)},
		{AssetID: "AQUA-" + foldUSDCIssuer, VolumeUSD24h: strp("90000")},
		{AssetID: foldSUSDClassic, Code: "sUSD", VolumeUSD24h: strp("80126.5"), TradeCount24h: i64(2)},
	}
	out, merged := s.foldAliasTwins(context.Background(), rows)
	if len(out) != 2 {
		t.Fatalf("fold returned %d rows, want 2: %+v", len(out), out)
	}
	if out[0].AssetID != foldSUSDClassic || out[0].Code != "sUSD" {
		t.Errorf("first row = %q, want the classic sUSD row at the SAC's rank", out[0].AssetID)
	}
	if out[0].VolumeUSD24h == nil || *out[0].VolumeUSD24h != "1338102.5" {
		t.Errorf("merged volume = %v, want exact \"1338102.5\"", out[0].VolumeUSD24h)
	}
	if out[0].TradeCount24h == nil || *out[0].TradeCount24h != 42 {
		t.Errorf("merged trade count = %v, want 42", out[0].TradeCount24h)
	}
	if _, ok := merged[foldSUSDSAC]; !ok {
		t.Errorf("merged set %v lacks the SAC, so the off-page restore would count it again", merged)
	}
}

// countingSACReader is a sacStubReader that counts lake lookups.
type countingSACReader struct {
	sacStubReader
	calls int
}

func (c *countingSACReader) SACClassicAssetName(ctx context.Context, id string) (string, bool, error) {
	c.calls++
	return c.sacStubReader.SACClassicAssetName(ctx, id)
}

// TestFoldAliasTwins_LakeResolvedSACWithClassicOffPage: a SAC whose classic row is not on the page resolves through the lake and is
// dropped like a registry alias; the positive answer is memoised.
func TestFoldAliasTwins_LakeResolvedSACWithClassicOffPage(t *testing.T) {
	reader := &countingSACReader{sacStubReader: sacStubReader{name: "USDC:" + foldUSDCIssuer, found: true}}
	s := &Server{Options: Options{Explorer: reader}}
	rows := []AssetDetail{{AssetID: foldUSDCSAC, VolumeUSD24h: strp("9000000")}, {AssetID: "AQUA-" + foldUSDCIssuer}}
	for range 2 {
		out, _ := s.foldAliasTwins(context.Background(), rows)
		if len(out) != 1 || out[0].AssetID != "AQUA-"+foldUSDCIssuer {
			t.Fatalf("fold = %+v, want only AQUA (the SAC's classic row is served on its own page)", out)
		}
	}
	if reader.calls != 1 {
		t.Errorf("lake lookups = %d, want 1 (memoised)", reader.calls)
	}
}

// TestFoldAliasTwins_UnresolvedContractStays: a contract that is no classic asset's SAC is its own row.
func TestFoldAliasTwins_UnresolvedContractStays(t *testing.T) {
	s := &Server{Options: Options{Explorer: &sacStubReader{}}}
	rows := []AssetDetail{{AssetID: foldUSDCSAC, VolumeUSD24h: strp("5")}}
	if out, _ := s.foldAliasTwins(context.Background(), rows); len(out) != 1 || out[0].AssetID != foldUSDCSAC {
		t.Fatalf("fold = %+v, want the contract row kept", out)
	}
}

// TestMergeDerivedSACVolume_CountsOffPageSACOnce: a classic row takes its SAC's arm volume only when the fold did not merge it.
func TestMergeDerivedSACVolume_CountsOffPageSACOnce(t *testing.T) {
	arm := map[string]AssetDetail{foldSUSDSAC: {AssetID: foldSUSDSAC, VolumeUSD24h: strp("0.2"), TradeCount24h: i64(3)}}

	row := AssetDetail{AssetID: foldSUSDClassic, VolumeUSD24h: strp("0.1"), TradeCount24h: i64(1)}
	mergeDerivedSACVolume(&row, arm, nil)
	if *row.VolumeUSD24h != "0.3" || *row.TradeCount24h != 4 {
		t.Errorf("off-page merge = %s/%d, want 0.3/4", *row.VolumeUSD24h, *row.TradeCount24h)
	}

	row = AssetDetail{AssetID: foldSUSDClassic, VolumeUSD24h: strp("0.1")}
	mergeDerivedSACVolume(&row, arm, map[string]struct{}{foldSUSDSAC: {}})
	if *row.VolumeUSD24h != "0.1" {
		t.Errorf("volume after in-page merge = %s, want 0.1 unchanged", *row.VolumeUSD24h)
	}
}
