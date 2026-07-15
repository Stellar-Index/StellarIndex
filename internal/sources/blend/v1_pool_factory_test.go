package blend

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// ─── real-lake golden frames (base64 XDR) ────────────────────────
//
// Captured 2026-07-10 via a read-only ClickHouse query against the
// r1 raw lake (stellar.contract_events), scoped by topic_0_sym +
// ledger_seq — see internal/sources/blend/README.md "Known gap" for
// the evidence trail. These PIN the V1 pool-factory's simpler
// vocabulary: if a decode helper drifts, the asserted fields change.

// TestGolden_UpdateEmissionsV1 pins decodeUpdateEmissions against a
// real V1 pool event: ledger 51,524,668, pool CDVQVKOY…, tx
// f56fabf7…, event_index 4. Topic: [Symbol("update_emissions")].
// Body: bare i128 = 447798000000.
func TestGolden_UpdateEmissionsV1(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Type:           "contract",
		ContractID:     "CDVQVKOY2YSXS2IC7KN6MNASSHPAO7UN2UR2ON4OI2SKMFJNVAMDX6DP",
		Ledger:         51_524_668,
		LedgerClosedAt: "2026-04-14T00:00:00Z",
		TxHash:         "f56fabf75569b7106703ad0b6d26eb565d63a66d5e99a9295a180962fb3f9945",
		OperationIndex: 0,
		EventIndex:     4,
		Topic:          []string{"AAAADwAAABB1cGRhdGVfZW1pc3Npb25z"},
		Value:          "AAAACgAAAAAAAAAAAAAAaELXOYA=",
	}
	closedAt, err := time.Parse(time.RFC3339, ev.LedgerClosedAt)
	if err != nil {
		t.Fatalf("parse closedAt: %v", err)
	}
	out, err := decodeUpdateEmissions(ev, closedAt)
	if err != nil {
		t.Fatalf("decodeUpdateEmissions: %v", err)
	}
	if out.Pool != ev.ContractID {
		t.Errorf("Pool=%q want %q", out.Pool, ev.ContractID)
	}
	if out.Kind != EventUpdateEmissions {
		t.Errorf("Kind=%q want %q", out.Kind, EventUpdateEmissions)
	}
	want := big.NewInt(447798000000)
	if out.Amount == nil || out.Amount.Cmp(want) != 0 {
		t.Errorf("Amount=%v want %s", out.Amount, want)
	}
}

// TestGolden_NewLiquidationAuctionV1 pins decodeNewLiquidationAuctionV1
// against a real V1 pool event: ledger 51,611,821, pool CDVQVKOY…, tx
// c128f9ce…. Topic: [Symbol("new_liquidation_auction"),
// Address(user)]. Body: Map{bid, block, lot} — the SAME AuctionData
// shape decodeAuctionData already parses for V2's new_auction, but
// with no auction_type topic and no percent field.
func TestGolden_NewLiquidationAuctionV1(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Type:           "contract",
		ContractID:     "CDVQVKOY2YSXS2IC7KN6MNASSHPAO7UN2UR2ON4OI2SKMFJNVAMDX6DP",
		Ledger:         51_611_821,
		LedgerClosedAt: "2026-04-16T00:00:00Z",
		TxHash:         "c128f9ce868563035060163b5acabe5115da687e4aaaca438e6ee86f3fbdabb1",
		OperationIndex: 0,
		EventIndex:     0,
		Topic: []string{
			"AAAADwAAABduZXdfbGlxdWlkYXRpb25fYXVjdGlvbgA=",
			"AAAAEgAAAAAAAAAA55lmZg3eGvDC+CmG2pYQJ98JuJKj9DkZw0DnosK1jsk=",
		},
		Value: "AAAAEQAAAAEAAAADAAAADwAAAANiaWQAAAAAEQAAAAEAAAABAAAAEgAAAAGt785ZruUpaPdgYdSUwlJbdWWfpClqZfSZ7ynlZHfklgAAAAoAAAAAAAAAAAAAAABAX+1PAAAADwAAAAVibG9jawAAAAAAAAMDE4iuAAAADwAAAANsb3QAAAAAEQAAAAEAAAABAAAAEgAAAAEltPzYWa7C+mNIQ4xImzw8EMmLbSG+T9PLMMtolT75dwAAAAoAAAAAAAAAAAAAAAK2pAaD",
	}
	closedAt, err := time.Parse(time.RFC3339, ev.LedgerClosedAt)
	if err != nil {
		t.Fatalf("parse closedAt: %v", err)
	}
	out, err := decodeNewLiquidationAuctionV1(ev, closedAt)
	if err != nil {
		t.Fatalf("decodeNewLiquidationAuctionV1: %v", err)
	}
	if out.ContractID != ev.ContractID {
		t.Errorf("ContractID=%q want %q", out.ContractID, ev.ContractID)
	}
	if out.Kind != EventNewLiquidationAuction {
		t.Errorf("Kind=%q want %q", out.Kind, EventNewLiquidationAuction)
	}
	wantUser := "GDTZSZTGBXPBV4GC7AUYNWUWCAT56CNYSKR7IOIZYNAOPIWCWWHMTIPS"
	if out.Target != wantUser {
		t.Errorf("Target=%q want %q", out.Target, wantUser)
	}
	if out.AuctionBlock != 51_611_822 {
		t.Errorf("AuctionBlock=%d want 51611822", out.AuctionBlock)
	}
	if len(out.AuctionBid) != 1 {
		t.Fatalf("AuctionBid len=%d want 1", len(out.AuctionBid))
	}
	if want := "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"; out.AuctionBid[0].Asset != want {
		t.Errorf("AuctionBid[0].Asset=%q want %q", out.AuctionBid[0].Asset, want)
	}
	if want := big.NewInt(1_080_028_495); out.AuctionBid[0].Amount.Cmp(want) != 0 {
		t.Errorf("AuctionBid[0].Amount=%s want %s", out.AuctionBid[0].Amount, want)
	}
	if len(out.AuctionLot) != 1 {
		t.Fatalf("AuctionLot len=%d want 1", len(out.AuctionLot))
	}
	if want := "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"; out.AuctionLot[0].Asset != want {
		t.Errorf("AuctionLot[0].Asset=%q want %q", out.AuctionLot[0].Asset, want)
	}
	if want := big.NewInt(11_654_137_475); out.AuctionLot[0].Amount.Cmp(want) != 0 {
		t.Errorf("AuctionLot[0].Amount=%s want %s", out.AuctionLot[0].Amount, want)
	}
}

// TestGolden_DeleteLiquidationAuctionV1 pins
// decodeDeleteLiquidationAuctionV1 against the single real V1
// occurrence: ledger 54,890,906, pool CBP7NO6F…, tx 24fb3d09…. Topic:
// [Symbol("delete_liquidation_auction"), Address(user)]. Body:
// ScvVoid (not parsed — same convention as V2's delete_auction).
func TestGolden_DeleteLiquidationAuctionV1(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Type:           "contract",
		ContractID:     "CBP7NO6F7FRDHSOFQBT2L2UWYIZ2PU76JKVRYAQTG3KZSQLYAOKIF2WB",
		Ledger:         54_890_906,
		LedgerClosedAt: "2026-06-08T00:00:00Z",
		TxHash:         "24fb3d09f926c1ffe0159e76befe2a34eb1516843347af400c8a9ea4f3a69e91",
		OperationIndex: 0,
		EventIndex:     0,
		Topic: []string{
			"AAAADwAAABpkZWxldGVfbGlxdWlkYXRpb25fYXVjdGlvbgAA",
			"AAAAEgAAAAAAAAAAX0K546vMzvmIdxWJNrpvLrwHILr9Bg/9TY3zC6vwGlk=",
		},
		Value: "AAAAAQ==",
	}
	closedAt, err := time.Parse(time.RFC3339, ev.LedgerClosedAt)
	if err != nil {
		t.Fatalf("parse closedAt: %v", err)
	}
	out, err := decodeDeleteLiquidationAuctionV1(ev, closedAt)
	if err != nil {
		t.Fatalf("decodeDeleteLiquidationAuctionV1: %v", err)
	}
	if out.ContractID != ev.ContractID {
		t.Errorf("ContractID=%q want %q", out.ContractID, ev.ContractID)
	}
	if out.Kind != EventDeleteLiquidationAuction {
		t.Errorf("Kind=%q want %q", out.Kind, EventDeleteLiquidationAuction)
	}
	wantUser := "GBPUFOPDVPGM56MIO4KYSNV2N4XLYBZAXL6QMD75JWG7GC5L6ANFSYBL"
	if out.Target != wantUser {
		t.Errorf("Target=%q want %q", out.Target, wantUser)
	}
}

// TestDecodeUpdateEmissionsV1_TopicArityMismatch pins the fail-loud
// path — a stray extra topic must error rather than silently decode.
func TestDecodeUpdateEmissionsV1_TopicArityMismatch(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Topic: []string{
			"AAAADwAAABB1cGRhdGVfZW1pc3Npb25z",
			"AAAAEgAAAAAAAAAA55lmZg3eGvDC+CmG2pYQJ98JuJKj9DkZw0DnosK1jsk=", // unexpected extra topic
		},
		Value: "AAAACgAAAAAAAAAAAAAAaELXOYA=",
	}
	if _, err := decodeUpdateEmissions(ev, time.Now()); err == nil {
		t.Fatal("expected ErrMalformedPayload for arity mismatch")
	}
}
