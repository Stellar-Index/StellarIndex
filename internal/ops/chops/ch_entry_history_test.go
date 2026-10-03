package chops

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

const (
	ehAlice   = "GAAZI4TCR3TY5OJHCTJC2A4QSY6CJWJH5IAJTGKIN2ER7LBNVKOCCWN7"
	ehBob     = "GBRPYHIL2CI3FNQ4BXLFMNDLFJUNPU2HY3ZMFSHONUCEOASW7QC7OX2H"
	ehIssuer  = "GCEZWKCA5VLDNRLN3RPRJMRZOX3Z6G5CHCGSNFHEYVXM3XOJMDS674JZ"
	ehUSDCID  = "USDC-" + ehIssuer
	ehTxHash  = "aa01"
	ehLedger  = uint32(500)
	ehFeeOpIx = int32(-1)
)

func ehAccount(id string, balance int64, seq int64) xdr.LedgerEntry {
	return xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeAccount,
		Account: &xdr.AccountEntry{
			AccountId:  xdr.MustAddress(id),
			Balance:    xdr.Int64(balance),
			SeqNum:     xdr.SequenceNumber(seq),
			Thresholds: xdr.Thresholds{1, 0, 0, 0},
		},
	}}
}

func ehTrustline(holder string, balance int64) xdr.LedgerEntry {
	return xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeTrustline,
		TrustLine: &xdr.TrustLineEntry{
			AccountId: xdr.MustAddress(holder),
			Asset:     xdr.MustNewCreditAsset("USDC", ehIssuer).ToTrustLineAsset(),
			Balance:   xdr.Int64(balance),
			Limit:     xdr.Int64(1 << 62),
		},
	}}
}

func ehSponsored(e xdr.LedgerEntry, sponsor string) xdr.LedgerEntry {
	e.Ext = xdr.LedgerEntryExt{V: 1, V1: &xdr.LedgerEntryExtensionV1{SponsoringId: xdr.MustAddressPtr(sponsor)}}
	return e
}

// ehRow is a source row for e; a nil entry is a removal (key only).
func ehRow(t *testing.T, op int32, change uint32, changeType string, key xdr.LedgerEntry, e *xdr.LedgerEntry) clickhouse.EntryHistorySourceRow {
	t.Helper()
	k, err := key.LedgerKey()
	if err != nil {
		t.Fatalf("ledger key: %v", err)
	}
	keyXDR, err := xdr.MarshalBase64(k)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	var entryXDR string
	if e != nil {
		if entryXDR, err = xdr.MarshalBase64(*e); err != nil {
			t.Fatalf("marshal entry: %v", err)
		}
	}
	names := map[xdr.LedgerEntryType]string{
		xdr.LedgerEntryTypeAccount: "account", xdr.LedgerEntryTypeTrustline: "trustline",
		xdr.LedgerEntryTypeOffer: "offer", xdr.LedgerEntryTypeLiquidityPool: "liquidity_pool",
		xdr.LedgerEntryTypeClaimableBalance: "claimable_balance",
	}
	return clickhouse.EntryHistorySourceRow{
		Ledger: ehLedger, CloseTime: time.Unix(1_700_000_000, 0).UTC(), TxHash: ehTxHash, OpIndex: op,
		ChangeIndex: change, ChangeType: changeType, EntryType: names[key.Data.Type], KeyXDR: keyXDR, EntryXDR: entryXDR,
	}
}

func ehProject(t *testing.T, p *entryHistoryProjector, r clickhouse.EntryHistorySourceRow) ([]clickhouse.AccountEntryChange, []clickhouse.AssetEntryChange) {
	t.Helper()
	acc, ast, err := p.project(r)
	if err != nil {
		t.Fatalf("project %s change %d: %v", r.ChangeType, r.ChangeIndex, err)
	}
	return acc, ast
}

func TestEntryHistoryProjector_FeeChargeIsAChangeWithItsFieldDiff(t *testing.T) {
	p := newEntryHistoryProjector()
	pre, post := ehAccount(ehAlice, 1000, 7), ehAccount(ehAlice, 900, 8)

	acc, ast := ehProject(t, p, ehRow(t, ehFeeOpIx, 0, "state", pre, &pre))
	if len(acc)+len(ast) != 0 {
		t.Fatalf("a 'state' pre-image produced %d/%d rows, want none", len(acc), len(ast))
	}
	acc, ast = ehProject(t, p, ehRow(t, ehFeeOpIx, 1, "updated", post, &post))
	if len(ast) != 0 {
		t.Fatalf("account entry produced %d asset rows, want 0 (native history is account-keyed)", len(ast))
	}
	if len(acc) != 1 {
		t.Fatalf("account rows = %d, want 1 owner row", len(acc))
	}
	r := acc[0]
	if r.Account != ehAlice || r.Role != "owner" || r.Asset != "native" || r.Balance.Int64() != 900 {
		t.Fatalf("owner row = %s/%s/%s/%s, want %s/owner/native/900", r.Account, r.Role, r.Asset, r.Balance, ehAlice)
	}
	if want := []string{"balance", "seq_num"}; !slices.Equal(r.Changed, want) {
		t.Fatalf("changed = %v, want %v", r.Changed, want)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(r.Fields), &fields); err != nil {
		t.Fatalf("fields JSON: %v", err)
	}
	if fields["balance"] != "900" || fields["seq_num"] != "8" {
		t.Fatalf("fields balance/seq_num = %v/%v, want stroop strings 900/8", fields["balance"], fields["seq_num"])
	}

	st := newEntryHistoryStats()
	st.record(acc, ast)
	if st.feeSeqOnly != 1 || st.accountRows["account"] != 1 {
		t.Fatalf("stats feeSeqOnly=%d accountRows=%d, want 1/1", st.feeSeqOnly, st.accountRows["account"])
	}
}

func TestEntryHistoryProjector_UnmergedDuplicatesReDeriveIdenticalRows(t *testing.T) {
	// The source is read without FINAL, so an unmerged duplicate arrives as
	// state, state, updated, updated — both updates must pair with the pre-image.
	p := newEntryHistoryProjector()
	pre, post := ehAccount(ehAlice, 1000, 7), ehAccount(ehAlice, 1000, 7)
	post.Data.Account.HomeDomain = "example.com"
	ehProject(t, p, ehRow(t, 0, 0, "state", pre, &pre))
	ehProject(t, p, ehRow(t, 0, 0, "state", pre, &pre))
	first, _ := ehProject(t, p, ehRow(t, 0, 1, "updated", post, &post))
	second, _ := ehProject(t, p, ehRow(t, 0, 1, "updated", post, &post))
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("rows = %d, %d, want 1, 1", len(first), len(second))
	}
	for _, r := range []clickhouse.AccountEntryChange{first[0], second[0]} {
		if !slices.Equal(r.Changed, []string{"home_domain"}) {
			t.Fatalf("changed = %v, want [home_domain] on both duplicates", r.Changed)
		}
	}
}

func TestEntryHistoryProjector_PreImageDoesNotLeakAcrossOperations(t *testing.T) {
	p := newEntryHistoryProjector()
	pre, post := ehAccount(ehAlice, 1000, 7), ehAccount(ehAlice, 900, 7)
	ehProject(t, p, ehRow(t, 0, 0, "state", pre, &pre))
	acc, _ := ehProject(t, p, ehRow(t, 1, 1, "updated", post, &post))
	// With no pre-image in its own op, an update reports every field.
	if len(acc) != 1 || !slices.Contains(acc[0].Changed, "home_domain") || !slices.Contains(acc[0].Changed, "balance") {
		t.Fatalf("changed = %v, want every field (no pre-image in op 1)", acc[0].Changed)
	}
}

func TestEntryHistoryProjector_TrustlineRemovalKeepsOwnerAndHolderAtZero(t *testing.T) {
	tl := ehTrustline(ehBob, 0)
	tl.Data.TrustLine.Limit = 5

	t.Run("with pre-image", func(t *testing.T) {
		p := newEntryHistoryProjector()
		ehProject(t, p, ehRow(t, 0, 0, "state", tl, &tl))
		acc, ast := ehProject(t, p, ehRow(t, 0, 1, "removed", tl, nil))
		ehAssertRemovedTrustline(t, acc, ast)
		var fields map[string]any
		if err := json.Unmarshal([]byte(acc[0].Fields), &fields); err != nil || fields["limit"] != "5" {
			t.Fatalf("removal fields = %s (%v), want the pre-image (limit 5)", acc[0].Fields, err)
		}
	})
	t.Run("key only", func(t *testing.T) {
		acc, ast := ehProject(t, newEntryHistoryProjector(), ehRow(t, 0, 1, "removed", tl, nil))
		ehAssertRemovedTrustline(t, acc, ast)
		if acc[0].Fields != "{}" {
			t.Fatalf("fields = %s, want {} with no image", acc[0].Fields)
		}
	})
}

func ehAssertRemovedTrustline(t *testing.T, acc []clickhouse.AccountEntryChange, ast []clickhouse.AssetEntryChange) {
	t.Helper()
	if len(acc) != 1 || acc[0].Account != ehBob || acc[0].Role != "owner" || acc[0].Asset != ehUSDCID || acc[0].Balance.Sign() != 0 {
		t.Fatalf("account rows = %+v, want one %s owner row on %s at 0", acc, ehBob, ehUSDCID)
	}
	if len(ast) != 1 || ast[0].Asset != ehUSDCID || ast[0].Role != "holder" || ast[0].Account != ehBob || ast[0].Balance.Sign() != 0 {
		t.Fatalf("asset rows = %+v, want one %s holder row for %s at 0", ast, ehUSDCID, ehBob)
	}
}

func TestEntryHistoryProjector_RevokedSponsorStillSeesTheChange(t *testing.T) {
	p := newEntryHistoryProjector()
	pre := ehSponsored(ehTrustline(ehBob, 10), ehAlice)
	post := ehTrustline(ehBob, 10)
	ehProject(t, p, ehRow(t, 0, 0, "state", pre, &pre))
	acc, ast := ehProject(t, p, ehRow(t, 0, 1, "updated", post, &post))
	var sponsorRow *clickhouse.AccountEntryChange
	for i := range acc {
		if acc[i].Account == ehAlice && acc[i].Role == "sponsor" {
			sponsorRow = &acc[i]
		}
	}
	if sponsorRow == nil {
		t.Fatalf("account rows = %+v, want a sponsor row for %s (sponsorship revoked)", acc, ehAlice)
	}
	if !slices.Equal(sponsorRow.Changed, []string{"sponsor"}) {
		t.Fatalf("changed = %v, want [sponsor]", sponsorRow.Changed)
	}
	if len(ast) != 1 || ast[0].Balance.Int64() != 10 {
		t.Fatalf("asset rows = %+v, want one holder row at 10", ast)
	}
}

func TestEntryHistoryProjector_OfferAndPoolFanOutToEveryAsset(t *testing.T) {
	usdc := xdr.MustNewCreditAsset("USDC", ehIssuer)
	offer := xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeOffer, Offer: &xdr.OfferEntry{
		SellerId: xdr.MustAddress(ehAlice), OfferId: 9, Selling: usdc, Buying: xdr.MustNewNativeAsset(),
		Amount: 50, Price: xdr.Price{N: 1, D: 2},
	}}}
	acc, ast := ehProject(t, newEntryHistoryProjector(), ehRow(t, 0, 0, "created", offer, &offer))
	if len(acc) != 1 || acc[0].Role != "owner" || acc[0].Changed != nil {
		t.Fatalf("offer account rows = %+v, want one owner row with no changed list", acc)
	}
	got := map[string]string{}
	for _, a := range ast {
		got[a.Role] = a.Asset
	}
	if got["selling"] != ehUSDCID || got["buying"] != "native" || len(ast) != 2 {
		t.Fatalf("offer asset rows = %v, want selling %s and buying native", got, ehUSDCID)
	}

	pool := xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeLiquidityPool, LiquidityPool: &xdr.LiquidityPoolEntry{
		LiquidityPoolId: xdr.PoolId{0xab},
		Body: xdr.LiquidityPoolEntryBody{Type: xdr.LiquidityPoolTypeLiquidityPoolConstantProduct, ConstantProduct: &xdr.LiquidityPoolEntryConstantProduct{
			Params:   xdr.LiquidityPoolConstantProductParameters{AssetA: xdr.MustNewNativeAsset(), AssetB: usdc, Fee: 30},
			ReserveA: 100, ReserveB: 200, TotalPoolShares: 300,
		}},
	}}}
	acc, ast = ehProject(t, newEntryHistoryProjector(), ehRow(t, 0, 0, "created", pool, &pool))
	if len(acc) != 0 {
		t.Fatalf("pool account rows = %+v, want none", acc)
	}
	bal := map[string]int64{}
	for _, a := range ast {
		bal[a.Role+":"+a.Asset] = a.Balance.Int64()
	}
	poolKey := "pool:pool:ab" + strings.Repeat("0", 62)
	if len(ast) != 3 || bal["reserve_a:native"] != 100 || bal["reserve_b:"+ehUSDCID] != 200 || bal[poolKey] != 300 {
		t.Fatalf("pool asset rows = %v, want reserve_a native 100, reserve_b %s 200, %s 300", bal, ehUSDCID, poolKey)
	}
}

func TestEntryHistoryProjector_ClaimableBalanceReachesClaimantsAndSponsor(t *testing.T) {
	cb := ehSponsored(xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeClaimableBalance, ClaimableBalance: &xdr.ClaimableBalanceEntry{
		BalanceId: xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &xdr.Hash{1}},
		Claimants: []xdr.Claimant{{Type: xdr.ClaimantTypeClaimantTypeV0, V0: &xdr.ClaimantV0{
			Destination: xdr.MustAddress(ehBob), Predicate: xdr.ClaimPredicate{Type: xdr.ClaimPredicateTypeClaimPredicateUnconditional},
		}}},
		Asset: xdr.MustNewCreditAsset("USDC", ehIssuer), Amount: 70,
	}}}, ehAlice)
	acc, ast := ehProject(t, newEntryHistoryProjector(), ehRow(t, 0, 0, "created", cb, &cb))
	roles := map[string]string{}
	for _, a := range acc {
		roles[a.Role] = a.Account
		if a.Role == "claimant" && (a.Asset != ehUSDCID || a.Balance.Int64() != 70) {
			t.Fatalf("claimant row asset/balance = %s/%s, want %s/70", a.Asset, a.Balance, ehUSDCID)
		}
	}
	if len(acc) != 2 || roles["claimant"] != ehBob || roles["sponsor"] != ehAlice {
		t.Fatalf("account roles = %v, want claimant %s and sponsor %s", roles, ehBob, ehAlice)
	}
	if len(ast) != 1 || ast[0].Role != "claimable" || ast[0].Account != ehAlice || ast[0].Balance.Int64() != 70 {
		t.Fatalf("asset rows = %+v, want one claimable row sponsored by %s at 70", ast, ehAlice)
	}
}

func TestEntryHistoryProjector_FeeSeqOnlyDetection(t *testing.T) {
	for _, tc := range []struct {
		changed []string
		want    bool
	}{
		{[]string{"balance"}, true},
		{[]string{"balance", "seq_ledger", "seq_num", "seq_time"}, true},
		{[]string{"balance", "signers"}, false},
		{nil, false},
	} {
		if got := isFeeSeqOnly(tc.changed); got != tc.want {
			t.Errorf("isFeeSeqOnly(%v) = %v, want %v", tc.changed, got, tc.want)
		}
	}
}

// TestRunEntryHistory_WatermarkProofCountsLedgersTheDeriveRead pins the
// coverage proof to the stream itself: snapshot seed rows (empty tx_hash) and
// further rows of an already-counted ledger add nothing.
func TestRunEntryHistory_WatermarkProofCountsLedgersTheDeriveRead(t *testing.T) {
	acct := ehAccount(ehAlice, 100, 1)
	at := func(ledger uint32, tx string, change uint32) clickhouse.EntryHistorySourceRow {
		r := ehRow(t, ehFeeOpIx, change, "updated", acct, &acct)
		r.Ledger, r.TxHash = ledger, tx
		return r
	}
	src := []clickhouse.EntryHistorySourceRow{
		at(ehLedger, "", 7), at(ehLedger, "aa", 0), at(ehLedger, "bb", 0),
		at(ehLedger+1, "", 9),
		at(ehLedger+2, "cc", 0), at(ehLedger+2, "cc", 1),
	}
	origStream, origInsert, origSet := streamEntryHistorySource, insertEntryHistory, setEntryHistoryWatermark
	t.Cleanup(func() {
		streamEntryHistorySource, insertEntryHistory, setEntryHistoryWatermark = origStream, origInsert, origSet
	})
	streamEntryHistorySource = func(_ context.Context, _ string, _, _ uint32, fn func(clickhouse.EntryHistorySourceRow) error) error {
		for _, r := range src {
			if err := fn(r); err != nil {
				return err
			}
		}
		return nil
	}
	insertEntryHistory = func(context.Context, string, []clickhouse.AccountEntryChange, []clickhouse.AssetEntryChange) (int64, error) {
		return 0, nil
	}
	var gotRead uint64
	setEntryHistoryWatermark = func(_ context.Context, _ string, _, _ uint32, readTxLedgers uint64) error {
		gotRead = readTxLedgers
		return nil
	}
	if err := runEntryHistory(context.Background(), "unused", ehLedger, ehLedger+2, 10, false, newEntryHistoryStats()); err != nil {
		t.Fatalf("runEntryHistory: %v", err)
	}
	if gotRead != 2 {
		t.Fatalf("watermark proof = %d tx-bearing ledgers read, want 2 (ledger %d holds only a snapshot row)", gotRead, ehLedger+1)
	}
}
