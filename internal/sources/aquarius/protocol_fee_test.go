package aquarius

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Real mainnet bodies captured from the ClickHouse lake:
//   - set_protocol_fee: Map{fee_protocol{0,1}_{new,old}: u32}
//   - claim_protocol_fee: Vec[recipient: Address, amount: i128]
//     (pool CCNXGPE4…, ledger 63,276,201, event_index 1).
const (
	realSetFeeTopic0   = "AAAADwAAABBzZXRfcHJvdG9jb2xfZmVl"         // Symbol("set_protocol_fee")
	realClaimFeeTopic0 = "AAAADwAAABJjbGFpbV9wcm90b2NvbF9mZWUAAA==" // Symbol("claim_protocol_fee")
	realSetFeeBody     = "AAAAEQAAAAEAAAAEAAAADwAAABFmZWVfcHJvdG9jb2wwX25ldwAAAAAAAAMAAAAEAAAADwAAABFmZWVfcHJvdG9jb2wwX29sZAAAAAAAAAMAAAAAAAAADwAAABFmZWVfcHJvdG9jb2wxX25ldwAAAAAAAAMAAAAEAAAADwAAABFmZWVfcHJvdG9jb2wxX29sZAAAAAAAAAMAAAAA"
	realClaimFeeBody   = "AAAAEAAAAAEAAAACAAAAEgAAAAAAAAAAh6JlBcp50WuRpV9UHUpdrSrIBYRyek+yAen9HUdZTT8AAAAKAAAAAAAAAAAAAAAAAAEuGw=="
)

func TestDecodeFee_setProtocolFee_realFixture(t *testing.T) {
	e := &events.Event{
		ContractID: "CCNXGPE4AQCSNEBZO3XJDKKDI3CRLYMVS6UWBBTVDLALLWMJEXBORQ2A",
		Ledger:     63_000_000,
		TxHash:     "aa",
		Topic:      []string{realSetFeeTopic0},
		Value:      realSetFeeBody,
	}
	if got := classify(e); got != EventSetProtocolFee {
		t.Fatalf("classify = %q, want %q", got, EventSetProtocolFee)
	}
	fe, err := decodeFee(e, closedAtTest, EventSetProtocolFee)
	if err != nil {
		t.Fatalf("decodeFee: %v", err)
	}
	if fe.Kind != EventSetProtocolFee {
		t.Errorf("Kind = %q", fe.Kind)
	}
	if fe.Fee0New != 4 || fe.Fee0Old != 0 || fe.Fee1New != 4 || fe.Fee1Old != 0 {
		t.Errorf("fees = (%d,%d,%d,%d), want (4,0,4,0)", fe.Fee0New, fe.Fee0Old, fe.Fee1New, fe.Fee1Old)
	}
}

func TestDecodeFee_claimProtocolFee_realFixture(t *testing.T) {
	// topic[1] carries the claimed token address on every sampled
	// on-chain event (audit 2026-08-04 finding 5) — the original
	// fixture captured only topic[0]; the token topic is rebuilt with
	// the same encoding the lake stores.
	wantClaimedAsset := "CCNXGPE4AQCSNEBZO3XJDKKDI3CRLYMVS6UWBBTVDLALLWMJEXBORQ2A" // public contract strkey, not a credential
	e := &events.Event{
		ContractID: "CCNXGPE4AQCSNEBZO3XJDKKDI3CRLYMVS6UWBBTVDLALLWMJEXBORQ2A",
		Ledger:     63_276_201,
		TxHash:     "b6c8ece0e7c9995967448fba5f0f7ab99ffc78677a259ab74dd2c04b46bac28e",
		EventIndex: 1,
		Topic:      []string{realClaimFeeTopic0, encodeContractAddrFromStrkey(t, wantClaimedAsset)},
		Value:      realClaimFeeBody,
	}
	if got := classify(e); got != EventClaimProtocolFee {
		t.Fatalf("classify = %q, want %q", got, EventClaimProtocolFee)
	}
	fe, err := decodeFee(e, closedAtTest, EventClaimProtocolFee)
	if err != nil {
		t.Fatalf("decodeFee: %v", err)
	}
	if fe.Token != wantClaimedAsset {
		t.Errorf("Token = %q, want %q (from topic[1])", fe.Token, wantClaimedAsset)
	}
	if fe.Kind != EventClaimProtocolFee {
		t.Errorf("Kind = %q", fe.Kind)
	}
	if fe.Recipient == "" || fe.Recipient[0] != 'G' {
		t.Errorf("recipient = %q, want a G-address", fe.Recipient)
	}
	if fe.Amount.String() != "77339" {
		t.Errorf("amount = %s, want 77339", fe.Amount)
	}
}

// Both fee events are pool-gated (same trust root as trade/reserves).
func TestMatches_protocolFeePoolGated(t *testing.T) {
	d := NewDecoder()
	pool := makeContractStrkey(t, 0xB2)
	d.reg.Seed(pool, MainnetRouter, 63_000_000)
	unregistered := makeContractStrkey(t, 0xBE)

	for _, topic := range []string{realSetFeeTopic0, realClaimFeeTopic0} {
		if !d.Matches(events.Event{ContractID: pool, Topic: []string{topic}}) {
			t.Errorf("fee topic %s from registered pool: Matches=false, want true", topic)
		}
		if d.Matches(events.Event{ContractID: unregistered, Topic: []string{topic}}) {
			t.Errorf("fee topic %s from unregistered contract: Matches=true, want false", topic)
		}
	}
}
