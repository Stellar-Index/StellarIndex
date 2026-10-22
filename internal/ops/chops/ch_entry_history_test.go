package chops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/entryhistory"
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

func ehProject(t *testing.T, p *entryhistory.Projector, r clickhouse.EntryHistorySourceRow) ([]clickhouse.AccountEntryChange, []clickhouse.AssetEntryChange) {
	t.Helper()
	acc, ast, err := p.Project(r)
	if err != nil {
		t.Fatalf("project %s change %d: %v", r.ChangeType, r.ChangeIndex, err)
	}
	return acc, ast
}

func TestEntryHistoryProjector_FeeChargeIsAChangeWithItsFieldDiff(t *testing.T) {
	p := entryhistory.NewProjector()
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
}

func TestEntryHistoryProjector_UnmergedDuplicatesReDeriveIdenticalRows(t *testing.T) {
	// The source is read without FINAL, so an unmerged duplicate arrives as
	// state, state, updated, updated — both updates must pair with the pre-image.
	p := entryhistory.NewProjector()
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
	p := entryhistory.NewProjector()
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
		p := entryhistory.NewProjector()
		ehProject(t, p, ehRow(t, 0, 0, "state", tl, &tl))
		acc, ast := ehProject(t, p, ehRow(t, 0, 1, "removed", tl, nil))
		ehAssertRemovedTrustline(t, acc, ast)
		var fields map[string]any
		if err := json.Unmarshal([]byte(acc[0].Fields), &fields); err != nil || fields["limit"] != "5" {
			t.Fatalf("removal fields = %s (%v), want the pre-image (limit 5)", acc[0].Fields, err)
		}
	})
	t.Run("key only", func(t *testing.T) {
		acc, ast := ehProject(t, entryhistory.NewProjector(), ehRow(t, 0, 1, "removed", tl, nil))
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
	p := entryhistory.NewProjector()
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
	acc, ast := ehProject(t, entryhistory.NewProjector(), ehRow(t, 0, 0, "created", offer, &offer))
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
	acc, ast = ehProject(t, entryhistory.NewProjector(), ehRow(t, 0, 0, "created", pool, &pool))
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
	acc, ast := ehProject(t, entryhistory.NewProjector(), ehRow(t, 0, 0, "created", cb, &cb))
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

// TestRunEntryHistory_WritesOnlySponsorshipEvents pins the derive's write set: a window's paired images yield
// sponsorship rows alone, and the watermark advances to the window's end.
func TestRunEntryHistory_WritesOnlySponsorshipEvents(t *testing.T) {
	tl := ehSponsored(ehTrustline(ehBob, 5), ehAlice)
	acct := ehAccount(ehAlice, 100, 1)
	src := []clickhouse.EntryHistorySourceRow{
		ehRow(t, 0, 0, "created", tl, &tl),     // a sponsored trustline appears: started
		ehRow(t, 0, 1, "updated", acct, &acct), // an unsponsored account change: nothing
	}
	origStream, origInsert, origSet := streamSponsorshipCandidates, insertSponsorshipEvents, setSponsorshipWatermark
	t.Cleanup(func() {
		streamSponsorshipCandidates, insertSponsorshipEvents, setSponsorshipWatermark = origStream, origInsert, origSet
	})
	streamSponsorshipCandidates = func(_ context.Context, _ string, _, _ uint32, fn func(clickhouse.EntryHistorySourceRow) error) error {
		for _, r := range src {
			if err := fn(r); err != nil {
				return err
			}
		}
		return nil
	}
	var inserted []clickhouse.SponsorshipEvent
	insertSponsorshipEvents = func(_ context.Context, _ string, rows []clickhouse.SponsorshipEvent) (int64, error) {
		inserted = append(inserted, rows...)
		return int64(len(rows)), nil
	}
	var wm [2]uint32
	setSponsorshipWatermark = func(_ context.Context, _ string, from, thru uint32) error {
		wm = [2]uint32{from, thru}
		return nil
	}
	totals := stubSponsorshipTotals(t, 0, nil)
	st := newEntryHistoryStats()
	if err := runEntryHistory(context.Background(), "unused", ehLedger, ehLedger+2, 10, false, st); err != nil {
		t.Fatalf("runEntryHistory: %v", err)
	}
	if len(inserted) != 1 || inserted[0].Sponsor != ehAlice || inserted[0].Sponsored != ehBob ||
		inserted[0].Kind != "trustline" || inserted[0].KeyID != ehUSDCID || inserted[0].Event != entryhistory.SponsorshipStarted {
		t.Fatalf("inserted = %+v, want one started trustline sponsorship of %s by %s", inserted, ehBob, ehAlice)
	}
	if wm != [2]uint32{ehLedger, ehLedger + 2} || st.events[entryhistory.SponsorshipStarted] != 1 {
		t.Fatalf("watermark window = %v, started = %d; want [%d %d] and 1", wm, st.events[entryhistory.SponsorshipStarted], ehLedger, ehLedger+2)
	}
	if want := [][2]uint32{{ehLedger, ehLedger + 2}}; !slices.Equal(totals.refreshed, want) || !slices.Equal(totals.advanced, want) {
		t.Fatalf("totals refreshed %v advanced %v, want both %v", totals.refreshed, totals.advanced, want)
	}
}

type totalsCalls struct {
	refreshed, advanced [][2]uint32
	events              uint32
}

// stubSponsorshipTotals fakes the totals seams: the watermark reads as wm and every advance returns advanceErr.
func stubSponsorshipTotals(t *testing.T, wm uint32, advanceErr error) *totalsCalls {
	t.Helper()
	origWM, origEv, origRefresh, origSet := sponsorshipTotalsWatermark, sponsorshipEventsWatermark, refreshSponsorshipTotals, setSponsorshipTotalsWM
	t.Cleanup(func() {
		sponsorshipTotalsWatermark, sponsorshipEventsWatermark, refreshSponsorshipTotals, setSponsorshipTotalsWM = origWM, origEv, origRefresh, origSet
	})
	c := &totalsCalls{}
	sponsorshipTotalsWatermark = func(context.Context, string) (uint32, error) { return wm, nil }
	sponsorshipEventsWatermark = func(context.Context, string) (uint32, error) { return c.events, nil }
	refreshSponsorshipTotals = func(_ context.Context, _ string, lo, hi uint32) error {
		c.refreshed = append(c.refreshed, [2]uint32{lo, hi})
		return nil
	}
	setSponsorshipTotalsWM = func(_ context.Context, _ string, lo, hi uint32) error {
		if advanceErr != nil {
			return advanceErr
		}
		c.advanced = append(c.advanced, [2]uint32{lo, hi})
		return nil
	}
	return c
}

func TestSponsorshipTotalsFollower_ClosesTheGapThenFollowsEachWindow(t *testing.T) {
	c := stubSponsorshipTotals(t, 100, nil)
	c.events = 130
	f := sponsorshipTotalsFollower{addr: "unused"}
	if err := f.start(context.Background(), 131, 10); err != nil {
		t.Fatal(err)
	}
	if err := f.follow(context.Background(), 131, 140); err != nil {
		t.Fatal(err)
	}
	want := [][2]uint32{{101, 110}, {111, 120}, {121, 130}, {131, 140}}
	if !slices.Equal(c.refreshed, want) || !slices.Equal(c.advanced, want) {
		t.Fatalf("refreshed %v advanced %v, want both %v", c.refreshed, c.advanced, want)
	}
}

func TestSponsorshipTotalsFollower_UnfilledBucketsTurnItOffWithoutFailingTheDerive(t *testing.T) {
	c := stubSponsorshipTotals(t, 0, fmt.Errorf("wrapped: %w", clickhouse.ErrSponsorshipTotalsUnfilled))
	f := sponsorshipTotalsFollower{addr: "unused"}
	if err := f.start(context.Background(), 500, 10); err != nil {
		t.Fatal(err)
	}
	for _, w := range [][2]uint32{{500, 509}, {510, 519}} {
		if err := f.follow(context.Background(), w[0], w[1]); err != nil {
			t.Fatalf("follow %v: %v", w, err)
		}
	}
	if len(c.refreshed) != 1 || len(c.advanced) != 0 {
		t.Fatalf("refreshed %v advanced %v: want one refused attempt, then off", c.refreshed, c.advanced)
	}
}

func TestSponsorshipTotalsFollower_OtherAdvanceErrorsFail(t *testing.T) {
	stubSponsorshipTotals(t, 0, clickhouse.ErrSponsorshipTotalsAheadOfEvents)
	f := sponsorshipTotalsFollower{addr: "unused"}
	if err := f.follow(context.Background(), 1, 10); !errors.Is(err, clickhouse.ErrSponsorshipTotalsAheadOfEvents) {
		t.Fatalf("err = %v, want ErrSponsorshipTotalsAheadOfEvents", err)
	}
}

func TestSponsorshipTotalsFollower_GapStopsAtTheEventsWatermark(t *testing.T) {
	c := stubSponsorshipTotals(t, 100, nil)
	c.events = 105
	f := sponsorshipTotalsFollower{addr: "unused"}
	if err := f.start(context.Background(), 131, 10); err != nil {
		t.Fatal(err)
	}
	if want := [][2]uint32{{101, 105}}; !slices.Equal(c.refreshed, want) {
		t.Fatalf("refreshed %v, want %v", c.refreshed, want)
	}
}
