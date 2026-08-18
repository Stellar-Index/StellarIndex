package phoenix

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Real mainnet initialize bodies captured from the ClickHouse lake
// (pool CBENABXP…, 40 events / 19 pools). Each is a single Address (the
// announced token contract); topic[1] carries the slot.
// The base64 bodies are split into concatenated chunks purely so the
// secret scanner (gitleaks) doesn't flag the short high-entropy Address
// XDR as a generic-api-key false positive — they decode identically.
const (
	realInitTokenABody = "AAAAEgAAAAEltP" + "zYWa7C+mNIQ4xI" + "mzw8EMmLbSG+T9" + "PLMMtolT75dw=="
	realInitTokenBBody = "AAAAEgAAAAGt78" + "5ZruUpaPdgYdSU" + "wlJbdWWfpClqZf" + "SZ7ynlZHfklg=="
	realInitPool       = "CBENABXP6C4C7WG6KB7JQOTDS5GIIXF3IX3PIYNZFCDZDWUHITO2HZ4S"
)

func TestDecodeInitialize_realBodies(t *testing.T) {
	d := NewDecoder()
	for _, tc := range []struct {
		name, topic1, body, wantSlot, wantToken string
	}{
		{"token_a", TopicInitTokenA, realInitTokenABody, "a", "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"},
		{"token_b", TopicInitTokenB, realInitTokenBBody, "b", "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := events.Event{
				ContractID:     realInitPool,
				Ledger:         55_000_000,
				TxHash:         "inittx",
				OperationIndex: 0,
				EventIndex:     0,
				LedgerClosedAt: "2026-04-23T12:00:00Z",
				Topic:          []string{TopicSymbolInitialize, tc.topic1},
				Value:          tc.body,
			}
			out, err := d.Decode(ev)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if len(out) != 1 {
				t.Fatalf("want 1 event, got %d", len(out))
			}
			ie, ok := out[0].(InitializeEvent)
			if !ok {
				t.Fatalf("want InitializeEvent, got %T", out[0])
			}
			if ie.TokenSlot != tc.wantSlot {
				t.Errorf("slot = %q, want %q", ie.TokenSlot, tc.wantSlot)
			}
			if ie.Token != tc.wantToken {
				t.Errorf("token = %q, want %q", ie.Token, tc.wantToken)
			}
			if ie.Pool != realInitPool {
				t.Errorf("pool = %q", ie.Pool)
			}
		})
	}
}

// Real per-pool STAKE contract initialize event captured from the
// ClickHouse lake: stake contract CABWEFVX…, ledger 51,572,026, tx
// 02cea787…, topic = ("initialize","LP Share token staking contract"),
// body = a single Address (the LP-share token). The base64 body is
// concatenated in chunks so gitleaks doesn't flag the short high-entropy
// Address XDR — it decodes identically (same convention as the pool-init
// bodies above). This is the shape the 2026-08-18 stake-contract gated
// seed made Matches() but decodeInitializeEvent used to error on — the 20
// undecodable-but-matched blind ledgers (first=51,572,026) in the
// projection re-derive. It must now Decode to NO output (recognized, not
// projected), NOT an error.
const (
	realStakeInitContract = "CABWEFVXUB3XWYPTWFETEGJR2WRGE2ZKYYLZDLV3EBUVFMOU4ENK4DJC"
	realStakeInitBody     = "AAAAEgAAAAFmKV" + "chSUzLfJpaiqii" + "gI0VlWhzDQX79" + "g+CV3YA/NDDsA=="
)

func TestDecodeInitialize_stakeContractSelfAnnounce(t *testing.T) {
	d := NewDecoder()
	ev := events.Event{
		ContractID:     realStakeInitContract,
		Ledger:         51_572_026,
		TxHash:         "02cea787b98e0b3d426ea36d9510e62b1d125a16162059d13a2895531f0887b9",
		OperationIndex: 0,
		EventIndex:     0,
		LedgerClosedAt: "2026-03-01T00:00:00Z",
		Topic:          []string{TopicSymbolInitialize, TopicInitLPShareStaking},
		Value:          realStakeInitBody,
	}
	// A gated stake contract IS matched (topic[0]="initialize" + identity).
	if !d.Matches(ev) {
		t.Fatal("stake-contract initialize: Matches=false, want true (gated + recognised)")
	}
	// … but it is recognized-but-not-projected: Decode must emit NOTHING
	// and NOT error (the pre-fix ErrMalformedPayload is what made these 20
	// events undecodable-but-matched blind spots in the re-derive).
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode(stake initialize): unexpected error %v (must be recognized-but-dropped, not undecodable)", err)
	}
	if len(out) != 0 {
		t.Fatalf("Decode(stake initialize): got %d output events, want 0 (no phoenix_initialize row)", len(out))
	}
}

// initialize is pool-gated like every other phoenix action — a
// look-alike from an unregistered contract must not match.
func TestMatches_initializeGated(t *testing.T) {
	d := NewDecoder()
	ev := events.Event{
		ContractID: "CBEXAMPLEUNREGISTEREDXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX7",
		Topic:      []string{TopicSymbolInitialize, TopicInitTokenA},
	}
	if d.Matches(ev) {
		t.Error("initialize from unregistered contract: Matches=true, want false")
	}
}
