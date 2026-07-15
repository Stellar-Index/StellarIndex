package aquarius

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Golden decode tests for the twelve rewards-gauge event kinds
// (migration 0099, ROADMAP #89). Every topic/body blob below is an
// UNTOUCHED base64 SCVal captured from the r1 ClickHouse lake
// (stellar.contract_events) on 2026-07-10 — real production wire
// format, same capture method liquidity_decode_test.go documents.
// Exact provenance (contract, ledger, tx) is cited per case.

var rewardsClosedAtTest = time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)

// ─── pool_state ─────────────────────────────────────────────────

func TestDecodePoolState_realFixture(t *testing.T) {
	// Pool CD3INVPZI3UBNYU3FEMTIGUJCYQVVMD73XSAOL7FFCYOUQ34DSFUZUZT,
	// ledger 62006854, tx 35d81b492f16…, event_index 7.
	e := &events.Event{
		ContractID: "CD3INVPZI3UBNYU3FEMTIGUJCYQVVMD73XSAOL7FFCYOUQ34DSFUZUZT",
		Ledger:     62006854,
		TxHash:     "35d81b492f16643c537e1daa636ff6c1dc48350f31884a73edda123372a72f4c",
		EventIndex: 7,
		Topic:      []string{"AAAADwAAAApwb29sX3N0YXRlAAA="},
		Value:      "AAAAEAAAAAEAAAADAAAACwAAAAAAAAAAAAAAAAAAAAAAAABTfoygXKV8zXbEHVNtAAAABAABWbMAAAAKAAAAAAAAAAAAAAIoYsEsnQ==",
	}
	if got := classify(e); got != EventPoolState {
		t.Fatalf("classify = %q, want %q", got, EventPoolState)
	}
	rv, err := decodeRewardsEvent(e, EventPoolState, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeRewardsEvent: %v", err)
	}
	if rv.Kind != RewardsPoolState {
		t.Errorf("Kind = %q, want %q", rv.Kind, RewardsPoolState)
	}
	if rv.UserAddress != "" {
		t.Errorf("UserAddress = %q, want empty (no actor topic)", rv.UserAddress)
	}
	if rv.Amount != nil {
		t.Errorf("Amount = %v, want nil (state snapshot, not a claim amount)", rv.Amount)
	}
	if got := rv.Attributes["accumulator"]; got != "6615102606823837892755792417645" {
		t.Errorf("accumulator = %v", got)
	}
	if got := rv.Attributes["checkpoint"]; got != int32(88499) {
		t.Errorf("checkpoint = %v, want 88499", got)
	}
	if got := rv.Attributes["value"]; got != "2372478774429" {
		t.Errorf("value = %v, want 2372478774429", got)
	}
}

// ─── claim_reward ───────────────────────────────────────────────

func TestDecodeClaimReward_realFixture(t *testing.T) {
	e := &events.Event{
		ContractID: "CCFGZJTHQZGDZP5PK6WMLKHKJ72ACSVMJGCI2NFR7Q6EAVSKWLJB3ZH3",
		Ledger:     62000053,
		TxHash:     "3c3a180d0a7d467621df239a9370355e4e4249c8f98729f7163510dde8a80899",
		EventIndex: 1,
		Topic: []string{
			"AAAADwAAAAxjbGFpbV9yZXdhcmQ=",
			"AAAAEgAAAAEohS9owZhIjjRvsSEu1QKQU3Ycwk9FM5LjU5ggGwgl5w==",
			"AAAAEgAAAAAAAAAAGFJvImUhe1Um7DcQIll44FVjzfnDHLalppun+3zFidQ=",
		},
		Value: "AAAAEAAAAAEAAAABAAAACgAAAAAAAAAAAAAADA0rT7g=",
	}
	if got := classify(e); got != EventClaimReward {
		t.Fatalf("classify = %q, want %q", got, EventClaimReward)
	}
	rv, err := decodeRewardsEvent(e, EventClaimReward, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeRewardsEvent: %v", err)
	}
	if rv.UserAddress != "GAMFE3ZCMUQXWVJG5Q3RAISZPDQFKY6N7HBRZNVFU2N2P634YWE5JOX5" {
		t.Errorf("UserAddress = %q", rv.UserAddress)
	}
	if rv.Amount == nil || rv.Amount.String() != "51760549816" {
		t.Errorf("Amount = %v, want 51760549816", rv.Amount)
	}
	if got := rv.Attributes["reward_token"]; got != "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK" {
		t.Errorf("reward_token = %v", got)
	}
}

func TestDecodeClaimReward_wrongTopicArity(t *testing.T) {
	e := &events.Event{Topic: []string{"AAAADwAAAAxjbGFpbV9yZXdhcmQ="}, Value: "AAAACgAAAAAAAAAAAAAAAAAAAAA="}
	if _, err := decodeRewardsEvent(e, EventClaimReward, rewardsClosedAtTest); err == nil {
		t.Error("expected error on wrong topic arity")
	}
}

// ─── set_rewards_config ─────────────────────────────────────────

func TestDecodeSetRewardsConfig_realFixture(t *testing.T) {
	// Pool CAQODUH4XNX2NTFVACRMO4UR7MA5RLSZA5ZQTHILQYGYYCFQ3LUATIGM,
	// ledger 59002073 — same tx as the router's config_rewards for
	// this pool (cross-checked below): amount + expires_at match.
	e := &events.Event{
		ContractID: "CAQODUH4XNX2NTFVACRMO4UR7MA5RLSZA5ZQTHILQYGYYCFQ3LUATIGM",
		Ledger:     59002073,
		TxHash:     "50bc35c5e8be0f8c9724cbfe9b3757ba84d530f82ea5e324646551ae9c51a29f",
		Topic:      []string{"AAAADwAAABJzZXRfcmV3YXJkc19jb25maWcAAA=="},
		Value:      "AAAAEAAAAAEAAAACAAAABQAAAABoziklAAAACQAAAAAAAAAAAAAAAABxWBw=",
	}
	rv, err := decodeRewardsEvent(e, EventSetRewardsConfig, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeRewardsEvent: %v", err)
	}
	if rv.Amount == nil || rv.Amount.String() != "7428124" {
		t.Errorf("Amount = %v, want 7428124", rv.Amount)
	}
	if got := rv.Attributes["expires_at"]; got != uint64(1758341413) {
		t.Errorf("expires_at = %v, want 1758341413", got)
	}
}

// ─── position_update ────────────────────────────────────────────

func TestDecodePositionUpdate_realFixture(t *testing.T) {
	// Same pool + tx as TestDecodePoolState_realFixture — the i128
	// delta here (2372478774429) matches pool_state's "value" field
	// exactly, and the checkpoint (88499) sits between range_from
	// (86260) and range_to (90340).
	e := &events.Event{
		ContractID: "CD3INVPZI3UBNYU3FEMTIGUJCYQVVMD73XSAOL7FFCYOUQ34DSFUZUZT",
		Ledger:     62006854,
		TxHash:     "35d81b492f16643c537e1daa636ff6c1dc48350f31884a73edda123372a72f4c",
		EventIndex: 6,
		Topic: []string{
			"AAAADwAAAA9wb3NpdGlvbl91cGRhdGUA",
			"AAAAEgAAAAAAAAAAtLXUxXrrKPUi8mO1zfvuEI/VRbqTRiHrgktRuXg7VGA=",
		},
		Value: "AAAAEAAAAAEAAAADAAAABAABUPQAAAAEAAFg5AAAAAoAAAAAAAAAAAAAAihiwSyd",
	}
	rv, err := decodeRewardsEvent(e, EventPositionUpdate, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeRewardsEvent: %v", err)
	}
	if rv.UserAddress != "GC2LLVGFPLVSR5JC6JR3LTP35YII7VKFXKJUMIPLQJFVDOLYHNKGARRY" {
		t.Errorf("UserAddress = %q", rv.UserAddress)
	}
	if rv.Amount != nil {
		t.Errorf("Amount = %v, want nil (signed delta lives in Attributes)", rv.Amount)
	}
	if got := rv.Attributes["range_from"]; got != int32(86260) {
		t.Errorf("range_from = %v, want 86260", got)
	}
	if got := rv.Attributes["range_to"]; got != int32(90340) {
		t.Errorf("range_to = %v, want 90340", got)
	}
	if got := rv.Attributes["delta"]; got != "2372478774429" {
		t.Errorf("delta = %v, want 2372478774429", got)
	}
}

// TestDecodePositionUpdate_negativeDelta proves the signed i128 delta
// round-trips through a NEGATIVE value (a withdrawal — real bytes
// captured from the SAME pool + range, a later ledger where the
// amount is negated: 62007126).
func TestDecodePositionUpdate_negativeDelta(t *testing.T) {
	e := &events.Event{
		ContractID: "CD3INVPZI3UBNYU3FEMTIGUJCYQVVMD73XSAOL7FFCYOUQ34DSFUZUZT",
		Ledger:     62007126,
		TxHash:     "523ebab59c01b39c8a301bf2da44c8011229b87f48468debba1c21c9ce01f719",
		EventIndex: 4,
		Topic: []string{
			"AAAADwAAAA9wb3NpdGlvbl91cGRhdGUA",
			"AAAAEgAAAAAAAAAAtLXUxXrrKPUi8mO1zfvuEI/VRbqTRiHrgktRuXg7VGA=",
		},
		Value: "AAAAEAAAAAEAAAADAAAABAABUPQAAAAEAAFg5AAAAAr//////////////dedPtNj",
	}
	rv, err := decodeRewardsEvent(e, EventPositionUpdate, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeRewardsEvent: %v", err)
	}
	got, ok := rv.Attributes["delta"].(string)
	if !ok || len(got) == 0 || got[0] != '-' {
		t.Errorf("delta = %v, want a negative decimal string", rv.Attributes["delta"])
	}
}

// ─── deposit (bare) ─────────────────────────────────────────────

func TestDecodeGaugeDeposit_realFixture(t *testing.T) {
	e := &events.Event{
		ContractID: "CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7",
		Ledger:     62000025,
		TxHash:     "6ee120be95679f0ebf25880bac76bd753d36d30051ae8dfc83aa3b3c85dcdab3",
		EventIndex: 3,
		Topic: []string{
			"AAAADwAAAAdkZXBvc2l0AA==",
			"AAAAEgAAAAESnMjMYzbx/bvcwPOYNDTDzbhf2eqFaXo3gtMY2HSlgA==",
			"AAAAEgAAAAAAAAAAipWJPDkD10IjkgloKAv7guabkiWaStt8kmBhQ+BZTQ0=",
		},
		Value: "AAAAEAAAAAEAAAACAAAACgAAAAAAAAAAAAAAANIMKZYAAAAKAAAAAAAAAAAAAAAAo62LxQ==",
	}
	if got := classify(e); got != EventGaugeDeposit {
		t.Fatalf("classify = %q, want %q", got, EventGaugeDeposit)
	}
	rv, err := decodeRewardsEvent(e, EventGaugeDeposit, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeRewardsEvent: %v", err)
	}
	if rv.UserAddress != "GCFJLCJ4HEB5OQRDSIEWQKAL7OBONG4SEWNEVW34SJQGCQ7ALFGQ22FV" {
		t.Errorf("UserAddress = %q", rv.UserAddress)
	}
	if got := rv.Attributes["ref_address"]; got != "CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD" {
		t.Errorf("ref_address = %v", got)
	}
	if got := rv.Attributes["amount_0"]; got != "3524012438" {
		t.Errorf("amount_0 = %v", got)
	}
	if got := rv.Attributes["amount_1"]; got != "2746059717" {
		t.Errorf("amount_1 = %v", got)
	}
}

// ─── claim_fees ─────────────────────────────────────────────────

func TestDecodeClaimFees_realFixture(t *testing.T) {
	e := &events.Event{
		ContractID: "CD3INVPZI3UBNYU3FEMTIGUJCYQVVMD73XSAOL7FFCYOUQ34DSFUZUZT",
		Ledger:     62007126,
		TxHash:     "523ebab59c01b39c8a301bf2da44c8011229b87f48468debba1c21c9ce01f719",
		EventIndex: 1,
		Topic: []string{
			"AAAADwAAAApjbGFpbV9mZWVzAAA=",
			"AAAAEgAAAAAAAAAAtLXUxXrrKPUi8mO1zfvuEI/VRbqTRiHrgktRuXg7VGA=",
			"AAAAEgAAAAEohS9owZhIjjRvsSEu1QKQU3Ycwk9FM5LjU5ggGwgl5w==",
			"AAAAEgAAAAHflXTUf95EXmoIJAmLcum3EvAciNFWU0XVKojd0YJ41Q==",
		},
		Value: "AAAAEAAAAAEAAAACAAAACgAAAAAAAAAAAAAAACQEHtwAAAAKAAAAAAAAAAAAAAAAAAAAAA==",
	}
	if got := classify(e); got != EventClaimFees {
		t.Fatalf("classify = %q, want %q", got, EventClaimFees)
	}
	rv, err := decodeRewardsEvent(e, EventClaimFees, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeRewardsEvent: %v", err)
	}
	// NOTE the user address is topic[1] here, not topic[len-1] — the
	// one rewards-family kind where user comes first (real bytes).
	if rv.UserAddress != "GC2LLVGFPLVSR5JC6JR3LTP35YII7VKFXKJUMIPLQJFVDOLYHNKGARRY" {
		t.Errorf("UserAddress = %q", rv.UserAddress)
	}
	if got := rv.Attributes["token_a"]; got != "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK" {
		t.Errorf("token_a = %v", got)
	}
	if got := rv.Attributes["token_b"]; got != "CDPZK5GUP7PEIXTKBASATC3S5G3RF4A4RDIVMU2F2UVIRXORQJ4NLOP7" {
		t.Errorf("token_b = %v", got)
	}
	if got := rv.Attributes["amount_a"]; got != "604249820" {
		t.Errorf("amount_a = %v", got)
	}
	if got := rv.Attributes["amount_b"]; got != "0" {
		t.Errorf("amount_b = %v, want 0 (one-sided fee accrual is valid)", got)
	}
}

// ─── rewards_gauge_claim ────────────────────────────────────────

func TestDecodeRewardsGaugeClaim_realFixture(t *testing.T) {
	e := &events.Event{
		ContractID: "CA6PUJLBYKZKUEKLZJMKBZLEKP2OTHANDEOWSFF44FTSYLKQPIICCJBE",
		Ledger:     59001286,
		TxHash:     "ee79648efd11f975e00207346915ef7cff26f121ca9fdbb05cfabf022da71fb1",
		EventIndex: 5,
		Topic: []string{
			"AAAADwAAABNyZXdhcmRzX2dhdWdlX2NsYWltAA==",
			"AAAAEgAAAAEltPzYWa7C+mNIQ4xImzw8EMmLbSG+T9PLMMtolT75dw==",
			"AAAAEgAAAAAAAAAALMF4OglMXurZyH6k+EU2gTuv+L3lRID4NvbQvLj1B7M=",
		},
		Value: "AAAAEAAAAAEAAAABAAAACQAAAAAAAAAAAAAAAAAAMug=",
	}
	if got := classify(e); got != EventRewardsGaugeClaim {
		t.Fatalf("classify = %q, want %q", got, EventRewardsGaugeClaim)
	}
	rv, err := decodeRewardsEvent(e, EventRewardsGaugeClaim, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeRewardsEvent: %v", err)
	}
	if rv.UserAddress != "GAWMC6B2BFGF52WZZB7KJ6CFG2ATXL7YXXSUJAHYG33NBPFY6UD3HHH5" {
		t.Errorf("UserAddress = %q", rv.UserAddress)
	}
	if rv.Amount == nil || rv.Amount.String() != "13032" {
		t.Errorf("Amount = %v, want 13032", rv.Amount)
	}
	// Reward token here is the XLM SAC — a different token than
	// claim_reward's AQUA-flavoured samples, confirming this is a
	// more general (arbitrary reward-token) gauge path.
	if got := rv.Attributes["reward_token"]; got != MainnetXLMSAC {
		t.Errorf("reward_token = %v, want MainnetXLMSAC", got)
	}
}

// ─── claim (bare) ───────────────────────────────────────────────

func TestDecodeGaugeClaim_realFixture(t *testing.T) {
	e := &events.Event{
		ContractID: "CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7",
		Ledger:     62000025,
		TxHash:     "6ee120be95679f0ebf25880bac76bd753d36d30051ae8dfc83aa3b3c85dcdab3",
		EventIndex: 4,
		Topic: []string{
			"AAAADwAAAAVjbGFpbQAAAA==",
			"AAAAEgAAAAAAAAAAipWJPDkD10IjkgloKAv7guabkiWaStt8kmBhQ+BZTQ0=",
		},
		// Bare I128, NOT wrapped in a Vec — the one rewards-family
		// kind whose body isn't a Vec at all.
		Value: "AAAACgAAAAAAAAAAAAAAANIMKZY=",
	}
	if got := classify(e); got != EventGaugeClaim {
		t.Fatalf("classify = %q, want %q", got, EventGaugeClaim)
	}
	rv, err := decodeRewardsEvent(e, EventGaugeClaim, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeRewardsEvent: %v", err)
	}
	if rv.UserAddress != "GCFJLCJ4HEB5OQRDSIEWQKAL7OBONG4SEWNEVW34SJQGCQ7ALFGQ22FV" {
		t.Errorf("UserAddress = %q", rv.UserAddress)
	}
	if rv.Amount == nil || rv.Amount.String() != "3524012438" {
		t.Errorf("Amount = %v, want 3524012438", rv.Amount)
	}
}

func TestDecodeGaugeClaim_notI128Rejected(t *testing.T) {
	e := &events.Event{
		Topic: []string{"AAAADwAAAAVjbGFpbQAAAA==", "AAAAEgAAAAAAAAAAipWJPDkD10IjkgloKAv7guabkiWaStt8kmBhQ+BZTQ0="},
		Value: "AAAAAQ==", // void, not I128
	}
	if _, err := decodeRewardsEvent(e, EventGaugeClaim, rewardsClosedAtTest); err == nil {
		t.Error("expected error on non-I128 body")
	}
}

// ─── rewards_gauge_schedule_reward ──────────────────────────────

func TestDecodeRewardsGaugeScheduleReward_realFixture(t *testing.T) {
	e := &events.Event{
		ContractID: "CCNXGPE4AQCSNEBZO3XJDKKDI3CRLYMVS6UWBBTVDLALLWMJEXBORQ2A",
		Ledger:     61001962,
		TxHash:     "36b8a4a897952f9c9c4c5499023f5a6c136dc8cca29d3cc127ae9a8a0e2acb8b",
		EventIndex: 1,
		Topic: []string{
			"AAAADwAAAB1yZXdhcmRzX2dhdWdlX3NjaGVkdWxlX3Jld2FyZAAAAA==",
			"AAAAEgAAAAEltPzYWa7C+mNIQ4xImzw8EMmLbSG+T9PLMMtolT75dw==",
		},
		Value: "AAAAEAAAAAEAAAADAAAABQAAAABpfNWAAAAABQAAAABphhAAAAAACQAAAAAAAAAAAAAAAAACvMY=",
	}
	rv, err := decodeRewardsEvent(e, EventRewardsGaugeScheduleReward, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeRewardsEvent: %v", err)
	}
	if rv.Amount == nil || rv.Amount.String() != "179398" {
		t.Errorf("Amount = %v, want 179398", rv.Amount)
	}
	starts, _ := rv.Attributes["starts_at"].(uint64)
	ends, _ := rv.Attributes["ends_at"].(uint64)
	if starts != 1769788800 {
		t.Errorf("starts_at = %v, want 1769788800", starts)
	}
	if ends != 1770393600 {
		t.Errorf("ends_at = %v, want 1770393600", ends)
	}
	if starts >= ends {
		t.Errorf("starts_at (%d) should be < ends_at (%d)", starts, ends)
	}
}

// ─── set_rewards_state ──────────────────────────────────────────

func TestDecodeSetRewardsState_realFixture(t *testing.T) {
	e := &events.Event{
		ContractID: "CAA6RWSD7SFHXTWLNTZQCIIZEFWNTBEEY7JZGNCFXZ5FDGACQMXS2KXJ",
		Ledger:     62008272,
		TxHash:     "5411c89a5cae0072935d2cfa3fd329449024d5f377855a199c30053f7bfa8cfb",
		Topic: []string{
			"AAAADwAAABFzZXRfcmV3YXJkc19zdGF0ZQAAAA==",
			"AAAAEgAAAAAAAAAAGM0wZqZUnYXTDfiwjygbqwWDaA9mDMUfDSPXgIcq6/Y=",
		},
		Value: "AAAAEAAAAAEAAAABAAAAAAAAAAE=",
	}
	rv, err := decodeRewardsEvent(e, EventSetRewardsState, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeRewardsEvent: %v", err)
	}
	if rv.UserAddress != "GAMM2MDGUZKJ3BOTBX4LBDZIDOVQLA3IB5TAZRI7BUR5PAEHFLV7MILL" {
		t.Errorf("UserAddress = %q", rv.UserAddress)
	}
	if got := rv.Attributes["enabled"]; got != true {
		t.Errorf("enabled = %v, want true", got)
	}
}

// TestDecodeSetRewardsState_falseRealFixture proves the `false` value
// round-trips too (real bytes: pool
// CBHWO677JA72CGF2SPTWISMJP7PQTT7NQBIO6W5YQ62UJBF55IN34F26, ledger
// 61319440).
func TestDecodeSetRewardsState_falseRealFixture(t *testing.T) {
	e := &events.Event{
		ContractID: "CBHWO677JA72CGF2SPTWISMJP7PQTT7NQBIO6W5YQ62UJBF55IN34F26",
		Ledger:     61319440,
		TxHash:     "284aeefc7c4185141440edc27dedf70e2110795c662646799e7007dc7046015b",
		Topic: []string{
			"AAAADwAAABFzZXRfcmV3YXJkc19zdGF0ZQAAAA==",
			"AAAAEgAAAAAAAAAAAuPAcYNM5pIG/e6c2bwM4YyWnVJ7NX5A1Mi91AaJ6C0=",
		},
		Value: "AAAAEAAAAAEAAAABAAAAAAAAAAA=",
	}
	rv, err := decodeRewardsEvent(e, EventSetRewardsState, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeRewardsEvent: %v", err)
	}
	if got := rv.Attributes["enabled"]; got != false {
		t.Errorf("enabled = %v, want false", got)
	}
}

// ─── rewards_gauge_add ───────────────────────────────────────────

func TestDecodeRewardsGaugeAdd_realFixture(t *testing.T) {
	e := &events.Event{
		ContractID: "CD3INVPZI3UBNYU3FEMTIGUJCYQVVMD73XSAOL7FFCYOUQ34DSFUZUZT",
		Ledger:     62006843,
		TxHash:     "cf81fe7f8507d62ae128643350aea652fa080ca0ec834ebfadd4f3a47422b64d",
		Topic:      []string{"AAAADwAAABFyZXdhcmRzX2dhdWdlX2FkZAAAAA=="},
		Value:      "AAAAEAAAAAEAAAACAAAAEgAAAAEohS9owZhIjjRvsSEu1QKQU3Ycwk9FM5LjU5ggGwgl5wAAABIAAAAB7YWwt5FHFyGVPDQkVAHNPng0J/y/lY6ilQ4nsaJfilY=",
	}
	rv, err := decodeRewardsEvent(e, EventRewardsGaugeAdd, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeRewardsEvent: %v", err)
	}
	if got := rv.Attributes["address_0"]; got != "CAUIKL3IYGMERDRUN6YSCLWVAKIFG5Q4YJHUKM4S4NJZQIA3BAS6OJPK" {
		t.Errorf("address_0 = %v", got)
	}
	if got := rv.Attributes["address_1"]; got != "CDWYLMFXSFDROIMVHQ2CIVABZU7HQNBH7S7ZLDVCSUHCPMNCL6FFNAIL" {
		t.Errorf("address_1 = %v", got)
	}
}

// ─── config_rewards (router-side) ───────────────────────────────

func TestDecodeConfigRewards_realFixture(t *testing.T) {
	// Router CBQDHNBF… (MainnetRouter), ledger 59002073, SAME tx as
	// TestDecodeSetRewardsConfig_realFixture — amount + expires_at
	// match exactly (7428124 / 1758341413), confirming the pool ↔
	// router field correlation.
	e := &events.Event{
		ContractID: MainnetRouter,
		Ledger:     59002073,
		TxHash:     "50bc35c5e8be0f8c9724cbfe9b3757ba84d530f82ea5e324646551ae9c51a29f",
		EventIndex: 1,
		Topic: []string{
			"AAAADwAAAA5jb25maWdfcmV3YXJkcwAA",
			"AAAAEAAAAAEAAAACAAAAEgAAAAEBXYCbqoen8nj67TgxiToTyzhZ6BokJeLbYyJFVbtOGgAAABIAAAABJbT82FmuwvpjSEOMSJs8PBDJi20hvk/TyzDLaJU++Xc=",
		},
		Value: "AAAAEAAAAAEAAAADAAAAEgAAAAEg4dD8u2+mzLUAosdykfsB2K5ZB3MJnQuGDYwIsNroCQAAAAkAAAAAAAAAAAAAAAAAcVgcAAAABQAAAABozikl",
	}
	if got := classify(e); got != EventConfigRewards {
		t.Fatalf("classify = %q, want %q", got, EventConfigRewards)
	}
	rv, err := decodeRewardsEvent(e, EventConfigRewards, rewardsClosedAtTest)
	if err != nil {
		t.Fatalf("decodeRewardsEvent: %v", err)
	}
	if rv.Amount == nil || rv.Amount.String() != "7428124" {
		t.Errorf("Amount = %v, want 7428124 (must match set_rewards_config on the same pool)", rv.Amount)
	}
	if got := rv.Attributes["expires_at"]; got != uint64(1758341413) {
		t.Errorf("expires_at = %v, want 1758341413", got)
	}
	if got := rv.Attributes["pool"]; got != "CAQODUH4XNX2NTFVACRMO4UR7MA5RLSZA5ZQTHILQYGYYCFQ3LUATIGM" {
		t.Errorf("pool = %v", got)
	}
	refs, ok := rv.Attributes["refs"].([]string)
	if !ok || len(refs) != 2 {
		t.Fatalf("refs = %v, want a 2-element []string", rv.Attributes["refs"])
	}
	if refs[1] != MainnetXLMSAC {
		t.Errorf("refs[1] = %q, want MainnetXLMSAC", refs[1])
	}
}
