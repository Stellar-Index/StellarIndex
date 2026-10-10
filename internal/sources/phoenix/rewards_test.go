package phoenix

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

const goldenStakeContract = "CBRGNWGAC25CPLMOAMR7WBPOF5QTFA5RYXQH4DEJ4K65G2QFLTLMW7RO"

// TestGolden_WithdrawRewards pins classifyAny + the correlation
// buffer + decodeWithdrawRewards against a real 2-event
// withdraw_rewards call: ledger 53,588,319, stake contract
// CBRGNWGAC25…, tx 734306f2….
func TestGolden_WithdrawRewards(t *testing.T) {
	t.Parallel()
	closedAt, err := time.Parse(time.RFC3339, "2026-05-15T00:00:00Z")
	if err != nil {
		t.Fatalf("parse closedAt: %v", err)
	}
	base := events.Event{
		Type:           "contract",
		ContractID:     goldenStakeContract,
		Ledger:         53_588_319,
		LedgerClosedAt: "2026-05-15T00:00:00Z",
		TxHash:         "734306f2ebe0315333d46a13dc145a917ca7156b08fcb885ddba6c622605200a",
		OperationIndex: 0,
	}

	userEv := base
	userEv.EventIndex = 0
	userEv.Topic = []string{
		"AAAADgAAABB3aXRoZHJhd19yZXdhcmRz", // "withdraw_rewards"
		"AAAADgAAAAR1c2Vy",                 // "user"
	}
	userEv.Value = "AAAAEgAAAAAAAAAAlUheN1hmPWXyLrWkGjGWJtLg9cObJOOedwRZG6cmhYs="

	tokenEv := base
	tokenEv.EventIndex = 1
	tokenEv.Topic = []string{
		"AAAADgAAABB3aXRoZHJhd19yZXdhcmRz", // "withdraw_rewards"
		"AAAADgAAAAxyZXdhcmRfdG9rZW4=",     // "reward_token"
	}
	tokenEv.Value = "AAAAEgAAAAFz9nQ7xy1g57dXaZEAriZ1tUpZvggsX2i7PE0ly4C7oQ=="

	// classifyAny routes both to actionWithdrawRewards with the
	// topic[1] blob threaded through.
	if a, ft := classifyAny(&userEv); a != actionWithdrawRewards || ft != userEv.Topic[1] {
		t.Fatalf("classifyAny(user) = (%v,%q), want (actionWithdrawRewards,%q)", a, ft, userEv.Topic[1])
	}
	if a, ft := classifyAny(&tokenEv); a != actionWithdrawRewards || ft != tokenEv.Topic[1] {
		t.Fatalf("classifyAny(reward_token) = (%v,%q), want (actionWithdrawRewards,%q)", a, ft, tokenEv.Topic[1])
	}

	buf := newBuffer()
	_, aFieldTopic := classifyAny(&userEv)
	completed, _, err := buf.absorbWithdrawRewards(&userEv, aFieldTopic, closedAt)
	if err != nil {
		t.Fatalf("absorbWithdrawRewards(user): %v", err)
	}
	if completed != nil {
		t.Fatalf("completed after 1/2 fields")
	}
	_, bFieldTopic := classifyAny(&tokenEv)
	completed, _, err = buf.absorbWithdrawRewards(&tokenEv, bFieldTopic, closedAt)
	if err != nil {
		t.Fatalf("absorbWithdrawRewards(reward_token): %v", err)
	}
	if completed == nil {
		t.Fatal("expected completion after 2/2 fields")
	}

	change, err := decodeWithdrawRewards(completed)
	if err != nil {
		t.Fatalf("decodeWithdrawRewards: %v", err)
	}
	if change.Action != EventActionWithdrawRewards {
		t.Errorf("Action=%q want %q", change.Action, EventActionWithdrawRewards)
	}
	if change.Contract != goldenStakeContract {
		t.Errorf("Contract=%q want %q", change.Contract, goldenStakeContract)
	}
	wantUser := "GCKUQXRXLBTD2ZPSF222IGRRSYTNFYHVYONSJY46O4CFSG5HE2CYXF2Y"
	if change.User != wantUser {
		t.Errorf("User=%q want %q", change.User, wantUser)
	}
	wantRewardToken := "CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32"
	if change.LPToken != wantRewardToken {
		t.Errorf("LPToken(reward_token)=%q want %q", change.LPToken, wantRewardToken)
	}
	if change.Amount.BigInt().Sign() != 0 {
		t.Errorf("Amount=%s want zero value (withdraw_rewards carries no amount on the wire)", change.Amount)
	}
}

// TestGolden_DistributeRewards pins classifyAny + decodeDistributeRewards
// against a real 1-event distribute_rewards call: ledger 53,587,626,
// stake contract CAF3UJ45ZQJ…, tx 4321e860….
func TestGolden_DistributeRewards(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Type:           "contract",
		ContractID:     "CAF3UJ45ZQJP6USFUIMVMGOUETUTXEC35R2247VJYIVQBGKTKBZKNBJ3",
		Ledger:         53_587_626,
		LedgerClosedAt: "2026-05-15T00:00:00Z",
		TxHash:         "4321e8608a371e2273e7acd8ea3c1b20d3aa0605be86011376a91523dde01244",
		OperationIndex: 0,
		EventIndex:     1,
		Topic: []string{
			"AAAADgAAABJkaXN0cmlidXRlX3Jld2FyZHMAAA==", // "distribute_rewards"
			"AAAADgAAAAVhc3NldAAAAA==",                 // "asset"
		},
		Value: "AAAAEgAAAAFz9nQ7xy1g57dXaZEAriZ1tUpZvggsX2i7PE0ly4C7oQ==",
	}

	if a, ft := classifyAny(ev); a != actionDistributeRewards || ft != ev.Topic[1] {
		t.Fatalf("classifyAny = (%v,%q), want (actionDistributeRewards,%q)", a, ft, ev.Topic[1])
	}

	closedAt, err := time.Parse(time.RFC3339, ev.LedgerClosedAt)
	if err != nil {
		t.Fatalf("parse closedAt: %v", err)
	}
	change, err := decodeDistributeRewards(ev, closedAt)
	if err != nil {
		t.Fatalf("decodeDistributeRewards: %v", err)
	}
	if change.Action != EventActionDistributeRewards {
		t.Errorf("Action=%q want %q", change.Action, EventActionDistributeRewards)
	}
	if change.Contract != ev.ContractID {
		t.Errorf("Contract=%q want %q", change.Contract, ev.ContractID)
	}
	if change.User != "" {
		t.Errorf("User=%q want empty (pool-wide announcement)", change.User)
	}
	wantAsset := "CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32"
	if change.LPToken != wantAsset {
		t.Errorf("LPToken(asset)=%q want %q", change.LPToken, wantAsset)
	}
}
