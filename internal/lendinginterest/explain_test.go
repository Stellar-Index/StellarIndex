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
	got := totals(fold(t, v1Auction.seed(), vs, opts))
	if got.nAttributed != 1 || got.attributed.String() != "-3096224423" || got.nUnexplained != 0 || got.nExplained != 0 {
		t.Fatalf("with the fill: %+v", got)
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
	cases := []struct {
		name      string
		events    []Event
		explained bool
	}{
		{"exact lot", []Event{eventOf(v, CauseInterestFill, v.Asset, -112512682)}, true},
		{"lot one unit off", []Event{eventOf(v, CauseInterestFill, v.Asset, -112512681)}, false},
		{"another tx of the ledger", []Event{eventOf(other, CauseInterestFill, v.Asset, -112512682)}, false},
		{"another pool's event in the tx", []Event{eventOf(otherPool, CauseInterestFill, v.Asset, -112512682)}, false},
		{"lot of another asset", []Event{eventOf(v, CauseInterestFill, "COTHER", -112512682)}, false},
		{"lot split across two causes", []Event{
			eventOf(v, CauseInterestFill, v.Asset, -112512700),
			func() Event { e := eventOf(v, CauseGulp, v.Asset, 18); e.EventIndex = 1; return e }(),
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := blntCACSQ.opts(0)
			opts.Protocols[foreign] = BLNT
			opts.Events = c.events
			got := totals(fold(t, blntCACSQ.seed(), vs, opts))
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
			used: map[eventID]bool{}, events: map[txKey][]Event{k: {{Pool: "P", Asset: "A", Amount: big.NewInt(-3)}}},
			poisoned: map[txRef]bool{},
		}
		st.used[eventID{txKey: k, asset: "A"}] = c.used
		st.poisoned[txRef{"P", "t", 1}] = c.poisoned
		row := &HourRow{ResidualExplained: new(big.Int), ResidualAttributed: new(big.Int), ResidualUnexplained: new(big.Int)}
		st.explain(row, c.nexts, big.NewInt(c.residual), c.onlyBackstop)
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
	}{
		{v2Supplies, 65641374, CauseGulp, 7},
		{blntCACSQ, 65648134, CauseReconcileLoss, -5},
	}
	for _, c := range cases {
		t.Run(string(c.cause), func(t *testing.T) {
			vs := c.d.versions(t)
			shiftCredit(vs, c.ledger, c.amount)
			v := versionAt(t, vs, c.ledger, "")
			opts := c.d.opts(100)
			opts.Events = []Event{eventOf(v, c.cause, v.Asset, c.amount)}
			got := totals(fold(t, c.d.seed(), vs, opts))
			if got.nExplained != 1 || got.explained.Int64() != c.amount {
				t.Fatalf("%+v", got)
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
	st.events[txKey{"P", "t", 1, 0}] = []Event{{Pool: "P", Asset: "A", Amount: big.NewInt(-3)}}
	row := &HourRow{ResidualExplained: new(big.Int), ResidualAttributed: new(big.Int), ResidualUnexplained: new(big.Int)}
	st.explain(row, []Version{v}, big.NewInt(-3), true)
	st.explain(row, []Version{v}, big.NewInt(-3), true)
	if row.ExplainedVersions != 1 || row.UnexplainedVersions != 1 || len(row.ResidualLedgers) != 1 {
		t.Fatalf("%+v", row)
	}
	// A b_rate or fee mismatch leaves the version unexplained whatever its events.
	st = &state{used: map[eventID]bool{}, events: st.events}
	st.explain(row, []Version{v}, big.NewInt(-3), false)
	if row.UnexplainedVersions != 2 {
		t.Fatalf("%+v", row)
	}
}
