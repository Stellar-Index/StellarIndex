package clickhouse

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// sampleLPEntryB64 is a real `liquidity_pool` LedgerEntry pulled from
// the r1 lake (2026-07-06): a LibreDrone/deCent constant-product pool.
// Cross-checked reserves/shares/fee against a live decode.
const sampleLPEntryB64 = "A8a/3gAAAAVDAB/9TN5yNCXdEkwx8I8NDm65a8BOH7etMS7iIceaUgAAAAAAAAACTGlicmVEcm9uZQAAAAAAAH6xUQOexaHR1+O0WQhBP1t0UTn3MQ03/W9e7MUcGDgGAAAAAmRlQ2VudAAAAAAAAAAAAABrGpPCEdJc+QUisRFFw1Pek6SvFh2ULsAauCJkPhkuHAAAAB4AAAAZkxPXBQAAAABvXP+kAAAAA0pue5EAAAAAAAAAAQAAAAA="

func TestNativeLPStateFromEntry_RealSample(t *testing.T) {
	st, ok := nativeLPStateFromEntry(sampleLPEntryB64)
	if !ok {
		t.Fatal("nativeLPStateFromEntry returned ok=false for a real constant-product pool entry")
	}
	if got, want := st.PoolHex, "43001ffd4cde723425dd124c31f08f0d0e6eb96bc04e1fb7ad312ee221c79a52"; got != want {
		t.Errorf("PoolHex = %q, want %q", got, want)
	}
	if got, want := st.PoolStrkey, "LBBQAH75JTPHENBF3UJEYMPQR4GQ43VZNPAE4H5XVUYS5YRBY6NFFKVM"; got != want {
		t.Errorf("PoolStrkey = %q, want %q", got, want)
	}
	if got, want := st.AssetA, "LibreDrone-GB7LCUIDT3C2DUOX4O2FSCCBH5NXIUJZ64YQ2N75N5POZRI4DA4AMGEE"; got != want {
		t.Errorf("AssetA = %q, want %q", got, want)
	}
	if got, want := st.AssetB, "deCent-GBVRVE6CCHJFZ6IFEKYRCRODKPPJHJFPCYOZILWADK4CEZB6DEXBYTI6"; got != want {
		t.Errorf("AssetB = %q, want %q", got, want)
	}
	if got, want := st.ReserveA.String(), "109841733381"; got != want {
		t.Errorf("ReserveA = %s, want %s", got, want)
	}
	if got, want := st.ReserveB.String(), "1868365732"; got != want {
		t.Errorf("ReserveB = %s, want %s", got, want)
	}
	if got, want := st.TotalShares.String(), "14133656465"; got != want {
		t.Errorf("TotalShares = %s, want %s", got, want)
	}
	if st.Trustlines != 1 {
		t.Errorf("Trustlines = %d, want 1", st.Trustlines)
	}
	if st.FeeBps != 30 {
		t.Errorf("FeeBps = %d, want 30", st.FeeBps)
	}
}

func TestNativeLPStateFromEntry_Garbage(t *testing.T) {
	if _, ok := nativeLPStateFromEntry("not-base64!!!"); ok {
		t.Error("expected ok=false for non-base64 input")
	}
	// A valid base64 that isn't a liquidity_pool entry (an empty entry).
	if _, ok := nativeLPStateFromEntry(""); ok {
		t.Error("expected ok=false for empty input")
	}
}

func TestPoolIDToXDR_BothForms(t *testing.T) {
	const (
		lstr = "LBBQAH75JTPHENBF3UJEYMPQR4GQ43VZNPAE4H5XVUYS5YRBY6NFFKVM"
		hexs = "43001ffd4cde723425dd124c31f08f0d0e6eb96bc04e1fb7ad312ee221c79a52"
	)
	fromL, err := poolIDToXDR(lstr)
	if err != nil {
		t.Fatalf("poolIDToXDR(L-strkey): %v", err)
	}
	fromHex, err := poolIDToXDR(hexs)
	if err != nil {
		t.Fatalf("poolIDToXDR(hex): %v", err)
	}
	if fromL != fromHex {
		t.Errorf("L-strkey and hex decoded to different pool ids: %x vs %x", fromL, fromHex)
	}
	// Round-trip the key back and confirm it decodes to the same id.
	key, err := liquidityPoolKeyXDR(fromL)
	if err != nil {
		t.Fatalf("liquidityPoolKeyXDR: %v", err)
	}
	var lk xdr.LedgerKey
	if err := xdr.SafeUnmarshalBase64(key, &lk); err != nil {
		t.Fatalf("unmarshal built key: %v", err)
	}
	if lk.Type != xdr.LedgerEntryTypeLiquidityPool || lk.LiquidityPool == nil {
		t.Fatalf("built key is not a liquidity-pool key: type=%v", lk.Type)
	}
	if lk.LiquidityPool.LiquidityPoolId != fromL {
		t.Errorf("round-tripped key pool id = %x, want %x", lk.LiquidityPool.LiquidityPoolId, fromL)
	}
}

func TestPoolIDToXDR_Rejects(t *testing.T) {
	for _, bad := range []string{"", "not-a-pool", "GB7LCUIDT3C2DUOX4O2FSCCBH5NXIUJZ64YQ2N75N5POZRI4DA4AMGEE", "abcd"} {
		if _, err := poolIDToXDR(bad); err == nil {
			t.Errorf("poolIDToXDR(%q) = nil error, want error", bad)
		}
	}
}

func TestClassicAssetID(t *testing.T) {
	// native
	if got, ok := classicAssetID(xdr.MustNewNativeAsset()); !ok || got != "native" {
		t.Errorf("native → (%q, %v), want (native, true)", got, ok)
	}
	// alphanum4
	issuer := "GB7LCUIDT3C2DUOX4O2FSCCBH5NXIUJZ64YQ2N75N5POZRI4DA4AMGEE"
	a4, err := xdr.NewCreditAsset("USDC", issuer)
	if err != nil {
		t.Fatalf("NewCreditAsset a4: %v", err)
	}
	if got, ok := classicAssetID(a4); !ok || got != "USDC-"+issuer {
		t.Errorf("alphanum4 → (%q, %v), want (USDC-%s, true)", got, ok, issuer)
	}
	// alphanum12
	a12, err := xdr.NewCreditAsset("LibreDrone", issuer)
	if err != nil {
		t.Fatalf("NewCreditAsset a12: %v", err)
	}
	if got, ok := classicAssetID(a12); !ok || got != "LibreDrone-"+issuer {
		t.Errorf("alphanum12 → (%q, %v), want (LibreDrone-%s, true)", got, ok, issuer)
	}
}

func TestLiquidityPoolStrkeyPrefix(t *testing.T) {
	// Guard: the strkey we emit round-trips under the LP version byte.
	st, ok := nativeLPStateFromEntry(sampleLPEntryB64)
	if !ok {
		t.Fatal("decode failed")
	}
	if _, err := strkey.Decode(strkey.VersionByteLiquidityPool, st.PoolStrkey); err != nil {
		t.Errorf("PoolStrkey %q is not a valid L-strkey: %v", st.PoolStrkey, err)
	}
}
