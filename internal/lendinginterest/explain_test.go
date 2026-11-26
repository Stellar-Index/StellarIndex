package lendinginterest

import (
	"math/big"
	"slices"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend/blendtest"
)

// residuals totals the rows' residual classes.
type residuals struct {
	explained, attributed, unexplained    *big.Int
	nExplained, nAttributed, nUnexplained uint32
}

func totals(rows []HourRow) residuals {
	r := residuals{new(big.Int), new(big.Int), new(big.Int), 0, 0, 0}
	for _, row := range rows {
		r.explained.Add(r.explained, row.ResidualExplained)
		r.attributed.Add(r.attributed, row.ResidualAttributed)
		r.unexplained.Add(r.unexplained, row.ResidualUnexplained)
		r.nExplained += row.ExplainedVersions
		r.nAttributed += row.AttributedVersions
		r.nUnexplained += row.UnexplainedVersions
	}
	return r
}

func versionAt(t *testing.T, vs []Version, ledger uint32, asset string) Version {
	t.Helper()
	for _, v := range vs {
		if v.Ledger == ledger && v.Kind == KindReserve && (asset == "" || v.Asset == asset) {
			return v
		}
	}
	t.Fatalf("no reserve version at %d", ledger)
	return Version{}
}

// classRow is an empty row with the residual-class amounts allocated, for driving explain directly.
func classRow() *HourRow {
	return &HourRow{
		ResidualExplained: new(big.Int), ResidualAttributed: new(big.Int), ResidualUnexplained: new(big.Int),
		InterestDrawn: new(big.Int), Gulped: new(big.Int), ReconcileBackstopLoss: new(big.Int),
	}
}

// parts totals the rows' residual_explained by cause: interest drawn, gulped, reconcile backstop loss.
func parts(rows []HourRow) [3]string {
	var s [3]big.Int
	for _, r := range rows {
		s[0].Add(&s[0], r.InterestDrawn)
		s[1].Add(&s[1], r.Gulped)
		s[2].Add(&s[2], r.ReconcileBackstopLoss)
	}
	return [3]string{s[0].String(), s[1].String(), s[2].String()}
}

// Every Checkable cause sums into exactly one column of residual_explained, and only those do.
func TestCauseColumns_LockstepWithCauseRules(t *testing.T) {
	checkable := map[Cause]bool{}
	for _, r := range CauseRules {
		if r.Checkable {
			checkable[r.Cause] = true
			if _, ok := causeColumns[r.Cause]; !ok {
				t.Errorf("Checkable cause %s has no residual_explained column", r.Cause)
			}
		}
	}
	for c := range causeColumns {
		if !checkable[c] {
			t.Errorf("column for %s, which is not a Checkable cause", c)
		}
	}
}

func eventOf(v Version, c Cause, asset string, amount int64) Event {
	e := Event{Pool: v.Pool, Ledger: v.Ledger, TxHash: v.TxHash, OpIndex: v.OpIndex, CloseTime: v.CloseTime, Cause: c}
	if asset != "" {
		e.Asset, e.Amount = asset, big.NewInt(amount)
	}
	return e
}

// V1's fill_auction carries no lot, so the golden interest-auction fill at 53013801 is attributed, never
// explained; without the event it stays unexplained.
func TestExplain_V1InterestFillAttributed(t *testing.T) {
	vs := v1Auction.versions(t)
	fill := eventOf(versionAt(t, vs, 53013801, ""), CauseInterestFillV1, "", 0)
	opts := v1Auction.opts(0)
	none := totals(fold(t, v1Auction.seed(), vs, opts))
	if none.nUnexplained != 1 || none.unexplained.String() != "-3096224423" || none.nAttributed+none.nExplained != 0 {
		t.Fatalf("no event: %+v", none)
	}
	opts.Events = []Event{fill}
	rows := fold(t, v1Auction.seed(), vs, opts)
	got := totals(rows)
	if got.nAttributed != 1 || got.attributed.String() != "-3096224423" || got.nUnexplained != 0 || got.nExplained != 0 {
		t.Fatalf("with the fill: %+v", got)
	}
	// interest_drawn holds explained lots only, so it misses the V1 draw: a lower bound on a V1 pool.
	if p := parts(rows); p != [3]string{"0", "0", "0"} {
		t.Fatalf("V1 attributed draw reached the per-cause columns: %v", p)
	}
}

// The BLNT draw at 65648345 (USDC residual −112512682) against an interest fill's lot: only the exact amount in
// the version's own transaction, from the version's own pool, for the version's asset explains it. The event is
// synthetic; the versions are the golden ones.
func TestExplain_InterestFillExactOnly(t *testing.T) {
	vs := blntCACSQ.versions(t)
	v := versionAt(t, vs, 65648345, blendtest.BLNT[0].Asset)
	const foreign = "CFOREIGNPOOL"
	other := v
	other.TxHash = strings.Repeat("0", 64)
	otherPool := v
	otherPool.Pool = foreign
	unexplainedParts := [3]string{"0", "0", "0"}
	cases := []struct {
		name      string
		events    []Event
		explained bool
		parts     [3]string // interest drawn, gulped, reconcile backstop loss
	}{
		{"exact lot", []Event{eventOf(v, CauseInterestFill, v.Asset, -112512682)}, true, [3]string{"-112512682", "0", "0"}},
		{"lot one unit off", []Event{eventOf(v, CauseInterestFill, v.Asset, -112512681)}, false, unexplainedParts},
		{"another tx of the ledger", []Event{eventOf(other, CauseInterestFill, v.Asset, -112512682)}, false, unexplainedParts},
		{"another pool's event in the tx", []Event{eventOf(otherPool, CauseInterestFill, v.Asset, -112512682)}, false, unexplainedParts},
		{"lot of another asset", []Event{eventOf(v, CauseInterestFill, "COTHER", -112512682)}, false, unexplainedParts},
		// gulp only ever adds token_delta: a negative one is no gulp the contract emits, whatever it sums to.
		{"a gulp of the residual's sign", []Event{eventOf(v, CauseGulp, v.Asset, -112512682)}, false, unexplainedParts},
		{"lot split across two causes", []Event{
			eventOf(v, CauseInterestFill, v.Asset, -112512700),
			func() Event { e := eventOf(v, CauseGulp, v.Asset, 18); e.EventIndex = 1; return e }(),
		}, true, [3]string{"-112512700", "18", "0"}},
		{"lot, gulp and reconcile loss", []Event{
			eventOf(v, CauseInterestFill, v.Asset, -112512690),
			func() Event { e := eventOf(v, CauseGulp, v.Asset, 13); e.EventIndex = 1; return e }(),
			func() Event { e := eventOf(v, CauseReconcileLoss, v.Asset, -5); e.EventIndex = 2; return e }(),
		}, true, [3]string{"-112512690", "13", "-5"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := blntCACSQ.opts(0)
			opts.Protocols[foreign] = BLNT
			opts.Events = c.events
			rows := fold(t, blntCACSQ.seed(), vs, opts)
			if p := parts(rows); p != c.parts {
				t.Fatalf("by cause %v, want %v", p, c.parts)
			}
			got := totals(rows)
			if c.explained != (got.nExplained == 1) || got.nExplained+got.nUnexplained != 1 || got.nAttributed != 0 {
				t.Fatalf("%+v", got)
			}
			want := got.unexplained
			if c.explained {
				want = got.explained
			}
			if want.String() != "-112512682" {
				t.Fatalf("residual %s, want -112512682 in its class", want)
			}
		})
	}
}

// An undecodable event anywhere in the version's transaction leaves it unexplained even where the decoded events
// match exactly or would attribute it, and its ledger needs no census; in another transaction it changes nothing.
func TestExplain_UndecodableEventPoisonsItsTransaction(t *testing.T) {
	vs := blntCACSQ.versions(t)
	v := versionAt(t, vs, 65648345, blendtest.BLNT[0].Asset)
	poison := eventOf(v, CauseUndecodable, "", 0)
	poison.OpIndex, poison.EventIndex = v.OpIndex+1, 7
	elsewhere := poison
	elsewhere.TxHash = strings.Repeat("0", 64)
	for name, c := range map[string]struct {
		poison    Event
		explained bool
	}{"same tx": {poison, false}, "another tx": {elsewhere, true}} {
		t.Run(name, func(t *testing.T) {
			opts := blntCACSQ.opts(0)
			opts.Events = []Event{eventOf(v, CauseInterestFill, v.Asset, -112512682), c.poison}
			rows := fold(t, blntCACSQ.seed(), vs, opts)
			got := totals(rows)
			if c.explained != (got.nExplained == 1) || got.nAttributed != 0 {
				t.Fatalf("%+v", got)
			}
			censused := false
			for _, r := range rows {
				censused = censused || slices.Contains(r.ResidualLedgers, v.Ledger)
			}
			if censused != c.explained {
				t.Fatalf("ledger %d in the census: %v, want %v", v.Ledger, censused, c.explained)
			}
		})
	}
	v1 := v1Auction.versions(t)
	fill := eventOf(versionAt(t, v1, 53013801, ""), CauseInterestFillV1, "", 0)
	bad := fill
	bad.Cause, bad.EventIndex = CauseUndecodable, 1
	opts := v1Auction.opts(0)
	opts.Events = []Event{fill, bad}
	if got := totals(fold(t, v1Auction.seed(), v1, opts)); got.nAttributed != 0 || got.nUnexplained != 1 {
		t.Fatalf("an attribution beside an undecodable event: %+v", got)
	}
}

// Only a classified version's ledger is in the census: a b_rate or fee mismatch, a zero residual, a version spanning
// transactions, an already-used event set and a poisoned transaction need no proof of their events.
func TestExplain_CensusOnlyClassifiedLedgers(t *testing.T) {
	v := Version{Pool: "P", Asset: "A", Ledger: 1, TxHash: "t"}
	other := v
	other.TxHash = "u"
	k := txKey{"P", "t", 1, 0}
	cases := []struct {
		name         string
		nexts        []Version
		residual     int64
		onlyBackstop bool
		used         bool
		poisoned     bool
		censused     bool
	}{
		{"classified", []Version{v}, -3, true, false, false, true},
		{"b_rate moved", []Version{v}, -3, false, false, false, false},
		{"zero residual", []Version{v}, 0, true, false, false, false},
		{"several txs", []Version{v, other}, -3, true, false, false, false},
		{"events used", []Version{v}, -3, true, true, false, false},
		{"poisoned", []Version{v}, -3, true, false, true, false},
	}
	for _, c := range cases {
		st := &state{
			used: map[eventID]bool{}, events: map[txKey][]Event{k: {{Pool: "P", Asset: "A", Cause: CauseInterestFill, Amount: big.NewInt(-3)}}},
			poisoned: map[txRef]bool{},
		}
		st.used[eventID{txKey: k, asset: "A"}] = c.used
		st.poisoned[txRef{"P", "t", 1}] = c.poisoned
		row := classRow()
		st.explain(row, c.nexts, big.NewInt(c.residual), c.onlyBackstop, nil)
		if got := len(row.ResidualLedgers) == 1; got != c.censused || (row.ExplainedVersions == 1) != c.censused {
			t.Errorf("%s: census %v explained %d, want %v", c.name, row.ResidualLedgers, row.ExplainedVersions, c.censused)
		}
	}
}

// A cause is accepted only for the protocols whose contract emits it as the rule reads it.
func TestExplain_RefusesCauseOfAnotherProtocol(t *testing.T) {
	vs := blntCACSQ.versions(t)
	v := versionAt(t, vs, 65648345, blendtest.BLNT[0].Asset)
	opts := blntCACSQ.opts(0)
	opts.Events = []Event{eventOf(v, CauseInterestFillV1, "", 0)}
	if _, _, err := Fold(blntCACSQ.seed(), vs, lastHour(vs), opts); err == nil || !strings.Contains(err.Error(), "is not one for") {
		t.Fatalf("err = %v", err)
	}
	opts.Events = []Event{eventOf(v, CauseInterestFill, "", 0)}
	if _, _, err := Fold(blntCACSQ.seed(), vs, lastHour(vs), opts); err == nil {
		t.Fatal("a checkable cause without an amount was accepted")
	}
}

// shiftCredit moves one golden version's backstop_credit by d, a draw or deposit accrual cannot explain; the
// next version's pair then carries −d.
func shiftCredit(vs []Version, ledger uint32, d int64) {
	for i := range vs {
		if vs[i].Ledger == ledger && vs[i].Kind == KindReserve {
			vs[i].Reserve.BackstopCredit = new(big.Int).Add(vs[i].Reserve.BackstopCredit, big.NewInt(d))
		}
	}
}

// gulp adds token_delta and BLNT's reconcile_loss subtracts backstop_credit_loss, each exactly: the shifted
// version is explained, the next one's opposite residual has no event and stays unexplained.
func TestExplain_GulpAndReconcileLoss(t *testing.T) {
	cases := []struct {
		d      dataset
		ledger uint32
		cause  Cause
		amount int64
		parts  [3]string // interest drawn, gulped, reconcile backstop loss
	}{
		{v2Supplies, 65641374, CauseGulp, 7, [3]string{"0", "7", "0"}},
		{blntCACSQ, 65648134, CauseReconcileLoss, -5, [3]string{"0", "0", "-5"}},
	}
	for _, c := range cases {
		t.Run(string(c.cause), func(t *testing.T) {
			vs := c.d.versions(t)
			shiftCredit(vs, c.ledger, c.amount)
			v := versionAt(t, vs, c.ledger, "")
			opts := c.d.opts(100)
			opts.Events = []Event{eventOf(v, c.cause, v.Asset, c.amount)}
			rows := fold(t, c.d.seed(), vs, opts)
			got := totals(rows)
			if got.nExplained != 1 || got.explained.Int64() != c.amount {
				t.Fatalf("%+v", got)
			}
			if p := parts(rows); p != c.parts {
				t.Fatalf("by cause %v, want %v", p, c.parts)
			}
			if got.nUnexplained == 0 {
				t.Fatalf("the next version's opposite residual was not left unexplained: %+v", got)
			}
		})
	}
}

// One transaction's events explain one version per asset: a second residual version of the asset in the same
// transaction does not reuse them.
func TestExplain_EventsExplainOnce(t *testing.T) {
	st := &state{used: map[eventID]bool{}, events: map[txKey][]Event{}}
	v := Version{Pool: "P", Asset: "A", Ledger: 1, TxHash: "t"}
	st.events[txKey{"P", "t", 1, 0}] = []Event{{Pool: "P", Asset: "A", Cause: CauseInterestFill, Amount: big.NewInt(-3)}}
	row := classRow()
	st.explain(row, []Version{v}, big.NewInt(-3), true, nil)
	st.explain(row, []Version{v}, big.NewInt(-3), true, nil)
	if row.ExplainedVersions != 1 || row.UnexplainedVersions != 1 || len(row.ResidualLedgers) != 1 || row.InterestDrawn.Int64() != -3 {
		t.Fatalf("%+v", row)
	}
	// A b_rate or fee mismatch leaves the version unexplained whatever its events.
	st = &state{used: map[eventID]bool{}, events: st.events}
	st.explain(row, []Version{v}, big.NewInt(-3), false, nil)
	if row.UnexplainedVersions != 2 {
		t.Fatalf("%+v", row)
	}
}
