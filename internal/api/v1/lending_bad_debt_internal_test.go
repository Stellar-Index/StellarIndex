package v1

import (
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const bdAsset = "CBADDEBTRESERVEASSET"

var bdNow = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

// bdEvent is one event an hour before bdNow.
func bdEvent(kind, asset, d string, ledger uint32) timescale.LendingBadDebtEvent {
	return timescale.LendingBadDebtEvent{Kind: kind, Asset: asset, DTokens: d, Ledger: ledger, TxHash: "TX" + kind, Ts: bdNow.Add(-time.Hour)}
}

// bdMarks prices bdAsset at 0.50 (7 decimals) from a bucket closed two hours before bdNow, with d_rate 1.25 at every event's
// transaction unless rates is false.
func bdMarks(events []timescale.LendingBadDebtEvent, rates bool) map[string]*liqMarks {
	m := &liqMarks{scale: 7, prices: []liqPrice{{close: bdNow.Add(-2 * time.Hour), price: "0.50", traded: true}}, rates: map[clickhouse.LendingTxRef]blend.ReserveData{}}
	if rates {
		for _, e := range events {
			m.rates[clickhouse.LendingTxRef{Ledger: e.Ledger, TxHash: "tx" + e.Kind}] = blend.ReserveData{Version: blend.PoolV2, DRate: big.NewInt(1_250_000_000_000)}
		}
	}
	return map[string]*liqMarks{bdAsset: m}
}

func bdInputs(events []timescale.LendingBadDebtEvent, borrow string, posLedger uint32) badDebtInputs {
	pos := clickhouse.LendingAccountPositions{Readable: true, Borrow: map[string]*big.Int{}, Ledger: posLedger}
	if borrow != "" {
		b, _ := new(big.Int).SetString(borrow, 10)
		pos.Borrow[bdAsset] = b
	}
	return badDebtInputs{
		assets: []string{bdAsset}, events: events, position: pos, now: bdNow, marks: bdMarks(events, true),
		current: map[string]blend.ReserveData{bdAsset: {Version: blend.PoolV2, DRate: big.NewInt(2_000_000_000_000)}},
	}
}

func bdStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func TestFoldBadDebt_IdentityHolds(t *testing.T) {
	ev := []timescale.LendingBadDebtEvent{
		bdEvent(timescale.BadDebtKindMoved, bdAsset, "1000", 10),
		bdEvent(timescale.BadDebtKindFill, bdAsset, "300", 11),
		bdEvent(timescale.BadDebtKindDefaulted, bdAsset, "500", 12),
		bdEvent(timescale.BadDebtKindRepay, bdAsset, "50", 13),
	}
	res, tot, _ := foldBadDebt(bdInputs(ev, "150", 13))
	r := res[0]
	if r.Reconciled == nil || !*r.Reconciled || bdStr(r.ResidualDTokens) != "0" || r.ReconcileReason != nil {
		t.Fatalf("reconciled %v residual %s reason %s", r.Reconciled, bdStr(r.ResidualDTokens), bdStr(r.ReconcileReason))
	}
	// 1000 d-tokens × 1.25 = 1250 base units = 0.000125 tokens at 0.50.
	if bdStr(r.MovedToBackstop.Underlying) != "1250" || r.MovedToBackstop.Events != 1 || bdStr(r.RepaidByBackstop.DTokens) != "50" {
		t.Errorf("moved %+v repaid %+v", r.MovedToBackstop, r.RepaidByBackstop)
	}
	// Outstanding is at the current d_rate (2.0), not the event's.
	if bdStr(r.Outstanding.Underlying) != "300" || tot.Outstanding.LowerBound || tot.MovedToBackstop.USD == nil {
		t.Errorf("outstanding %+v totals %+v", r.Outstanding, tot)
	}
}

func TestFoldBadDebt_IdentityFailsWithResidual(t *testing.T) {
	ev := []timescale.LendingBadDebtEvent{bdEvent(timescale.BadDebtKindMoved, bdAsset, "1000", 10)}
	r := firstReserve(foldBadDebt(bdInputs(ev, "999", 10)))
	if r.Reconciled == nil || *r.Reconciled || bdStr(r.ResidualDTokens) != "1" {
		t.Fatalf("reconciled %v residual %s, want false with residual 1", r.Reconciled, bdStr(r.ResidualDTokens))
	}
}

// The position was last written at a ledger no recorded event is at: the event tables lag the lake or an untracked path wrote it.
func TestFoldBadDebt_AsOfMismatchIsNotChecked(t *testing.T) {
	ev := []timescale.LendingBadDebtEvent{bdEvent(timescale.BadDebtKindMoved, bdAsset, "1000", 10)}
	r := firstReserve(foldBadDebt(bdInputs(ev, "1000", 11)))
	if r.Reconciled != nil || bdStr(r.ReconcileReason) != badDebtReconcileAsOf || r.ResidualDTokens != nil {
		t.Fatalf("reconciled %v reason %s", r.Reconciled, bdStr(r.ReconcileReason))
	}
}

// Events after the position's ledger count in the parts but not in the identity.
func TestFoldBadDebt_EventsAfterPositionAreClipped(t *testing.T) {
	ev := []timescale.LendingBadDebtEvent{
		bdEvent(timescale.BadDebtKindMoved, bdAsset, "1000", 10),
		bdEvent(timescale.BadDebtKindMoved, bdAsset, "7", 20),
	}
	res, _, facts := foldBadDebt(bdInputs(ev, "1000", 10))
	r := res[0]
	if bdStr(r.MovedToBackstop.DTokens) != "1007" || facts.eventsAfterPosition != 1 || r.Reconciled == nil || !*r.Reconciled {
		t.Fatalf("moved %s after %d reconciled %v", bdStr(r.MovedToBackstop.DTokens), facts.eventsAfterPosition, r.Reconciled)
	}
}

// The three mainnet V2 fills stored with a block and a NULL bid (fill ledger, auction block). The writer stores NULL beside a block
// only for an empty bid map (timescale.encodeBlendAssetAmounts), so each covered nothing; each is also at or past block+400, where
// the bid modifier has reached zero, which is why the map was empty.
func TestFoldBadDebt_V2ZeroBidFillsCoverNothing(t *testing.T) {
	mainnet := []struct{ fill, block uint32 }{{61341315, 61340914}, {61769199, 61768799}, {61769601, 61769201}}
	ev := []timescale.LendingBadDebtEvent{bdEvent(timescale.BadDebtKindMoved, bdAsset, "1000", 61340000)}
	for _, m := range mainnet {
		if m.fill-m.block < 400 {
			t.Fatalf("fixture %d: %d blocks after start, want >= 400", m.fill, m.fill-m.block)
		}
		ev = append(ev, bdEvent(timescale.BadDebtKindFillZeroBid, "", "", m.fill))
	}
	ev = append(ev, bdEvent(timescale.BadDebtKindDefaulted, bdAsset, "1000", 61769601))
	res, tot, facts := foldBadDebt(bdInputs(ev, "", 61769601))
	r := res[0]
	if bdStr(r.CoveredByBackstop.DTokens) != "0" || r.CoveredByBackstop.Events != 0 || facts.zeroBidFills != 3 {
		t.Fatalf("covered %+v zero-bid fills %d", r.CoveredByBackstop, facts.zeroBidFills)
	}
	if r.Reconciled == nil || !*r.Reconciled || bdStr(tot.CoveredByBackstop.USD) != "0.00" || tot.CoveredByBackstop.LowerBound {
		t.Errorf("reconciled %v covered total %+v", r.Reconciled, tot.CoveredByBackstop)
	}
}

// A V1 fill states no bid, and the V1 bid-modifier schedule is not cited in the repo, so covered is not derived from the new
// auction's bid: every reserve's covered part is null and the total names each.
func TestFoldBadDebt_V1FillBidIsNotGuessed(t *testing.T) {
	ev := []timescale.LendingBadDebtEvent{
		bdEvent(timescale.BadDebtKindMoved, bdAsset, "8565694", 52428304),
		bdEvent(timescale.BadDebtKindFillBidUnrecorded, "", "", 52431207),
	}
	res, tot, facts := foldBadDebt(bdInputs(ev, "", 52431207))
	r := res[0]
	if r.CoveredByBackstop.DTokens != nil || bdStr(r.CoveredByBackstop.DTokensReason) != badDebtReasonBidNotRecorded ||
		r.CoveredByBackstop.USD != nil || facts.unrecordedBidFills != 1 {
		t.Fatalf("covered %+v", r.CoveredByBackstop)
	}
	if r.Reconciled != nil || bdStr(r.ReconcileReason) != badDebtReconcilePartMissing {
		t.Errorf("reconciled %v reason %s", r.Reconciled, bdStr(r.ReconcileReason))
	}
	if tot.CoveredByBackstop.USD != nil || len(tot.CoveredByBackstop.Excluded) != 1 || tot.CoveredByBackstop.Excluded[0].Reason != badDebtReasonBidNotRecorded {
		t.Errorf("covered total %+v", tot.CoveredByBackstop)
	}
}

// A partial fill's filled_auction_data is the filled portion (wasm-audits/blend.md), so two half fills sum as stored, not rescaled.
func TestFoldBadDebt_PartialFillsSumAsStored(t *testing.T) {
	ev := []timescale.LendingBadDebtEvent{
		bdEvent(timescale.BadDebtKindMoved, bdAsset, "1000", 10),
		bdEvent(timescale.BadDebtKindFill, bdAsset, "400", 11),
		bdEvent(timescale.BadDebtKindFill, bdAsset, "600", 12),
	}
	r := firstReserve(foldBadDebt(bdInputs(ev, "0", 12)))
	if bdStr(r.CoveredByBackstop.DTokens) != "1000" || r.CoveredByBackstop.Events != 2 || r.Reconciled == nil || !*r.Reconciled {
		t.Fatalf("covered %+v reconciled %v", r.CoveredByBackstop, r.Reconciled)
	}
}

func TestFoldBadDebt_EachNullRule(t *testing.T) {
	moved := []timescale.LendingBadDebtEvent{bdEvent(timescale.BadDebtKindMoved, bdAsset, "1000", 10)}
	cases := []struct {
		name  string
		in    badDebtInputs
		check func(LendingBadDebtReserve, LendingBadDebtTotals) bool
	}{
		{
			"rate unrecorded", func() badDebtInputs { in := bdInputs(moved, "1000", 10); in.marks = bdMarks(moved, false); return in }(),
			func(r LendingBadDebtReserve, t LendingBadDebtTotals) bool {
				return bdStr(r.MovedToBackstop.DTokens) == "1000" && r.MovedToBackstop.Underlying == nil &&
					bdStr(r.MovedToBackstop.USDReason) == liqReasonRateUnrecorded && t.MovedToBackstop.USD == nil &&
					r.Reconciled != nil && *r.Reconciled
			},
		},
		{
			"no price", func() badDebtInputs { in := bdInputs(moved, "1000", 10); in.marks[bdAsset].prices = nil; return in }(),
			func(r LendingBadDebtReserve, _ LendingBadDebtTotals) bool {
				return bdStr(r.MovedToBackstop.Underlying) == "1250" && r.MovedToBackstop.USD == nil &&
					bdStr(r.MovedToBackstop.USDReason) == liqReasonNoPrice
			},
		},
		{
			"amount not recorded", bdInputs([]timescale.LendingBadDebtEvent{bdEvent(timescale.BadDebtKindDefaulted, bdAsset, "", 10)}, "", 10),
			func(r LendingBadDebtReserve, _ LendingBadDebtTotals) bool {
				return r.SocialisedToSuppliers.DTokens == nil && bdStr(r.SocialisedToSuppliers.DTokensReason) == badDebtReasonAmountNotRecorded &&
					bdStr(r.ReconcileReason) == badDebtReconcilePartMissing
			},
		},
		{
			"backstop unresolved", func() badDebtInputs {
				in := bdInputs(moved, "", 10)
				in.backstopReason = badDebtReasonBackstopUnresolved
				return in
			}(),
			func(r LendingBadDebtReserve, _ LendingBadDebtTotals) bool {
				return r.Outstanding.DTokens == nil && bdStr(r.RepaidByBackstop.DTokensReason) == badDebtReasonBackstopUnresolved &&
					bdStr(r.MovedToBackstop.DTokens) == "1000" && r.Reconciled == nil
			},
		},
		{"BLNT backstop role", func() badDebtInputs {
			in := bdInputs(moved, "", 10)
			in.backstopReason = badDebtReasonBackstopRoleUnverified
			return in
		}(), func(r LendingBadDebtReserve, _ LendingBadDebtTotals) bool {
			return bdStr(r.Outstanding.DTokensReason) == badDebtReasonBackstopRoleUnverified && bdStr(r.ReconcileReason) == badDebtReconcilePartMissing
		}},
		{
			"current rate missing", func() badDebtInputs { in := bdInputs(moved, "1000", 10); in.current = nil; return in }(),
			func(r LendingBadDebtReserve, t LendingBadDebtTotals) bool {
				return bdStr(r.Outstanding.DTokens) == "1000" && bdStr(r.Outstanding.USDReason) == badDebtReasonCurrentRate &&
					t.Outstanding.USD == nil && len(t.Outstanding.Excluded) == 1 && r.Reconciled != nil && *r.Reconciled
			},
		},
	}
	for _, c := range cases {
		res, tot, _ := foldBadDebt(c.in)
		if !c.check(res[0], tot) {
			t.Errorf("%s: reserve %+v totals %+v", c.name, res[0], tot)
		}
	}
}

// A pool total over one valued and one unvalued reserve is the valued sum, flagged lower_bound, naming the other.
func TestFoldBadDebt_TotalIsLowerBoundNamingExcluded(t *testing.T) {
	const other = "CBADDEBTOTHERASSET"
	ev := []timescale.LendingBadDebtEvent{bdEvent(timescale.BadDebtKindMoved, bdAsset, "10000000", 10), bdEvent(timescale.BadDebtKindMoved, other, "5", 10)}
	in := bdInputs(ev, "", 10)
	in.assets = []string{bdAsset, other}
	_, tot, _ := foldBadDebt(in)
	m := tot.MovedToBackstop
	// 10^7 d-tokens × 1.25 = 1.25 tokens at 0.50.
	if bdStr(m.USD) != "0.63" || !m.LowerBound || len(m.Excluded) != 1 || m.Excluded[0].Asset != other || m.Excluded[0].Reason != auctionUSDRateUnread {
		t.Fatalf("moved total %+v", m)
	}
}

// A pool with no bad debt serves true zeros: every part "0" over 0 events, the identity checked and holding.
func TestFoldBadDebt_NoBadDebtIsTrueZero(t *testing.T) {
	res, tot, _ := foldBadDebt(bdInputs(nil, "", 0))
	r := res[0]
	for _, p := range []LendingBadDebtPart{r.MovedToBackstop, r.CoveredByBackstop, r.SocialisedToSuppliers, r.RepaidByBackstop, r.Outstanding} {
		if bdStr(p.DTokens) != "0" || p.Events != 0 || bdStr(p.USD) != "0.00" {
			t.Errorf("part %+v, want 0 over 0 events", p)
		}
	}
	if r.Reconciled == nil || !*r.Reconciled || bdStr(tot.MovedToBackstop.USD) != "0.00" || tot.MovedToBackstop.LowerBound {
		t.Errorf("reconciled %v totals %+v", r.Reconciled, tot.MovedToBackstop)
	}
}

// Worked example from OBSERVED mainnet sums (r1 census, CCCCIQSD pool, CAS3J7GY reserve): a fixture of two sums, not a claim
// about the full chain. moved 605552822530218 and defaulted 594591748765629 leave 10961073764589 d-tokens that, for the identity
// to hold, the backstop's fills, repays and current position must account for exactly.
func TestFoldBadDebt_WorkedExampleFromMainnetSums(t *testing.T) {
	const moved, defaulted, remainder = "605552822530218", "594591748765629", "10961073764589"
	ev := []timescale.LendingBadDebtEvent{
		bdEvent(timescale.BadDebtKindMoved, bdAsset, moved, 10),
		bdEvent(timescale.BadDebtKindDefaulted, bdAsset, defaulted, 11),
	}
	if r := firstReserve(foldBadDebt(bdInputs(ev, remainder, 11))); r.Reconciled == nil || !*r.Reconciled {
		t.Fatalf("remainder outstanding: reconciled %v residual %s", r.Reconciled, bdStr(r.ResidualDTokens))
	}
	covered := append(append([]timescale.LendingBadDebtEvent(nil), ev...), bdEvent(timescale.BadDebtKindFill, bdAsset, remainder, 11))
	if r := firstReserve(foldBadDebt(bdInputs(covered, "", 11))); r.Reconciled == nil || !*r.Reconciled {
		t.Fatalf("remainder covered: reconciled %v residual %s", r.Reconciled, bdStr(r.ResidualDTokens))
	}
	if r := firstReserve(foldBadDebt(bdInputs(ev, "", 11))); bdStr(r.ResidualDTokens) != remainder {
		t.Fatalf("nothing else recorded: residual %s, want %s", bdStr(r.ResidualDTokens), remainder)
	}
}

func TestLendingBadDebtReasonEnumsMatchHandler(t *testing.T) {
	doc := loadSpecDoc(t)
	for _, tc := range []struct {
		path []string
		want []string
	}{
		{[]string{"LendingBadDebtPart", "properties", "d_tokens_reason"}, []string{
			badDebtReasonBidNotRecorded, badDebtReasonAmountNotRecorded, badDebtReasonAssetNotRecorded, badDebtReasonBackstopUnresolved,
			badDebtReasonBackstopRoleUnverified, badDebtReasonLakeUnavailable, badDebtReasonPositionUnread,
		}},
		{[]string{"LendingBadDebtReserve", "properties", "reconcile_reason"}, []string{badDebtReconcilePartMissing, badDebtReconcileAsOf}},
	} {
		got := slices.DeleteFunc(specEnumAt(t, doc, append([]string{"components", "schemas"}, tc.path...)...), func(s string) bool { return s == "" })
		slices.Sort(got)
		slices.Sort(tc.want)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%v enum = %v, want what the handler emits %v", tc.path, got, tc.want)
		}
	}
}

func firstReserve(res []LendingBadDebtReserve, _ LendingBadDebtTotals, _ badDebtPoolFacts) LendingBadDebtReserve {
	return res[0]
}
