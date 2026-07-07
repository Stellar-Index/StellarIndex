package sep41_supply

import (
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/StellarIndex/stellar-index/internal/events"
)

// encodeMapNoAmount builds a CAP-67-shaped body map that is MISSING the
// `amount` field (only to_muxed_id), for the defensive-drop test.
func encodeMapNoAmount(t *testing.T) string {
	t.Helper()
	key := xdr.ScSymbol("to_muxed_id")
	m := xdr.ScMap{{
		Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &key},
		Val: stringScVal("note"),
	}}
	mp := &m
	return encodeScVal(t, xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp})
}

// Real mainnet event blobs pulled from the r1 ClickHouse lake
// (stellar.contract_events) on 2026-07-06 for the dropped-mints
// finding. Contract CBH4M45T…OCKF is one of the 15 watched SEP-41
// tokens whose sep41_supply_rollup showed burn_total > mint_total
// (mint_total == 0): every one of its mints carries the CAP-67 MAP
// body { amount: i128, to_muxed_id: String } and the i128-only
// decodeAmount rejected them all with ErrAmountNotI128, dropping the
// whole row. These are the base64 SCVal blobs (topics + body) verbatim.
const (
	// mint @ ledger 59977903 — DROPPED by the old decoder.
	// topics: ["mint", to=GDZS…U44, "ETH:GBFX…SOCC"]
	// body:   MAP { amount: 1509470554, to_muxed_id: "Auto recharge transaction" }
	realMintSymB64     = "AAAADwAAAARtaW50"
	realMintToB64      = "AAAAEgAAAAAAAAAA8yVUX1QM+WJ+g+VPETScU03dLHaWSQx2iPuod3JtIzY="
	realMintAssetB64   = "AAAADgAAADxFVEg6R0JGWE9IVkFTNDNPSVdOSU83WExSSkFIVDNCSUNGRUlLT0pMWlZYTlQ1NzJNSVNNNENNR1NPQ0M="
	realMintMapBodyB64 = "AAAAEQAAAAEAAAACAAAADwAAAAZhbW91bnQAAAAAAAoAAAAAAAAAAAAAAABZ+LFaAAAADwAAAAt0b19tdXhlZF9pZAAAAAAOAAAAGUF1dG8gcmVjaGFyZ2UgdHJhbnNhY3Rpb24AAAA="
	realMintTo         = "GDZSKVC7KQGPSYT6QPSU6EJUTRJU3XJMO2LESDDWRD52Q53SNURTMU44"
	realMintAmount     = int64(1509470554)

	// burn @ ledger 37649263 — always decoded (bare i128 body).
	// topics: ["burn", from=GAVP…555, "ETH:GBFX…SOCC"]
	// body:   I128 330000000
	realBurnSymB64      = "AAAADwAAAARidXJu"
	realBurnFromB64     = "AAAAEgAAAAAAAAAAKvT8xC2Qz7HI1GiGX6yv31Vy2nZgmuGpvf2Gx1W6bcg="
	realBurnI128BodyB64 = "AAAACgAAAAAAAAAAAAAAABOrZoA="
	realBurnFrom        = "GAVPJ7GEFWIM7MOI2RUIMX5MV7PVK4W2OZQJVYNJXX6YNR2VXJW4R555"
	realBurnAmount      = int64(330000000)

	realWatched = "CBH4M45TQBLDPXOK6L7VYKMEJWFITBOL64BN3WDAIIDT4LNUTWTTOCKF"
)

// TestDecoder_DecodeRealCAP67MapMint is the regression golden for the
// 2026-07-06 dropped-mints finding. It feeds the REAL on-wire XDR of a
// mint whose body is a CAP-67 map (amount wrapped alongside
// to_muxed_id) and proves the decoder now (a) does not drop it and
// (b) recovers the exact i128 amount and the topic[1] recipient. Before
// the fix decodeAmount returned ErrAmountNotI128 on the map body.
func TestDecoder_DecodeRealCAP67MapMint(t *testing.T) {
	d, _ := NewDecoder([]string{realWatched})
	ev := events.Event{
		Type:           "contract",
		ContractID:     realWatched,
		Ledger:         59977903,
		LedgerClosedAt: "2026-06-15T00:00:00Z",
		TxHash:         "deadbeef",
		OperationIndex: 0,
		EventIndex:     0,
		Topic:          []string{realMintSymB64, realMintToB64, realMintAssetB64},
		Value:          realMintMapBodyB64,
	}

	if !d.Matches(ev) {
		t.Fatalf("Matches = false, want true (watched mint)")
	}
	outs, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode dropped the CAP-67 map mint: %v", err)
	}
	if len(outs) != 1 {
		t.Fatalf("emitted %d events, want 1", len(outs))
	}
	got := outs[0].(Event)
	if got.Kind != SymbolMint {
		t.Errorf("Kind = %q, want %q", got.Kind, SymbolMint)
	}
	if got.Amount.Cmp(big.NewInt(realMintAmount)) != 0 {
		t.Errorf("Amount = %s, want %d (map amount field)", got.Amount, realMintAmount)
	}
	if got.Counterparty != realMintTo {
		t.Errorf("Counterparty = %q, want %q (topic[1] recipient)", got.Counterparty, realMintTo)
	}
}

// TestDecoder_DecodeRealI128Burn pins that the bare-i128 body path is
// unchanged by the map-aware refactor (same contract, real burn blob).
func TestDecoder_DecodeRealI128Burn(t *testing.T) {
	d, _ := NewDecoder([]string{realWatched})
	ev := events.Event{
		Type:           "contract",
		ContractID:     realWatched,
		Ledger:         37649263,
		LedgerClosedAt: "2026-03-01T00:00:00Z",
		TxHash:         "cafebabe",
		Topic:          []string{realBurnSymB64, realBurnFromB64, realMintAssetB64},
		Value:          realBurnI128BodyB64,
	}
	outs, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got := outs[0].(Event)
	if got.Kind != SymbolBurn {
		t.Errorf("Kind = %q, want %q", got.Kind, SymbolBurn)
	}
	if got.Amount.Cmp(big.NewInt(realBurnAmount)) != 0 {
		t.Errorf("Amount = %s, want %d", got.Amount, realBurnAmount)
	}
	if got.Counterparty != realBurnFrom {
		t.Errorf("Counterparty = %q, want %q (topic[1] from)", got.Counterparty, realBurnFrom)
	}
}

// TestDecodeAmount_MapMissingAmountField guards the defensive arm: a
// map body WITHOUT an `amount` field is still a drop (ErrAmountNotI128),
// not a silent zero.
func TestDecodeAmount_MapMissingAmountField(t *testing.T) {
	d, _ := NewDecoder([]string{realWatched})
	ev := events.Event{
		Type:           "contract",
		ContractID:     realWatched,
		Ledger:         1,
		LedgerClosedAt: "2026-06-15T00:00:00Z",
		TxHash:         "abc",
		Topic:          []string{realMintSymB64, realMintToB64, realMintAssetB64},
		// A map body carrying only to_muxed_id, no amount.
		Value: encodeMapNoAmount(t),
	}
	if _, err := d.Decode(ev); err == nil {
		t.Fatal("expected drop on amount-less map body, got nil error")
	}
}
