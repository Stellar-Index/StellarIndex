package chops

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

const blntFixturePool = "CACSQWCMKG45TUHCQ3LXHOB7YEEJH4W376ZHQPXH2IZ3TE4NJHFZMCY7"

// blntInterestFill is the real BLNT interest-auction fill (auction_type 2) at ledger 65225244.
func blntInterestFill(t *testing.T) events.Event {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "test", "fixtures", "blnt",
		"0e4f8f45b94e6b0d017a496ac5620ad0e3f866533cf45acd2b070bedda1e7356", "fill_auction_pool_65225244_e90acf3a6cb0_3_CACSQW.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		ContractID string   `json:"contract_id"`
		Ledger     uint32   `json:"ledger"`
		TxHash     string   `json:"tx_hash"`
		OpIndex    int      `json:"op_index"`
		EventIndex int      `json:"event_index"`
		ClosedAt   string   `json:"ledger_closed_at"`
		Topics     []string `json:"topics"`
		Value      string   `json:"value"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return events.Event{
		Type: "contract", ContractID: f.ContractID, Ledger: f.Ledger, LedgerClosedAt: f.ClosedAt, TxHash: f.TxHash,
		OperationIndex: f.OpIndex, EventIndex: f.EventIndex, Topic: f.Topics, Value: f.Value, InSuccessfulContractCall: true,
	}
}

// v1LakeFill is a real V1 pool fill_auction row from the r1 lake capture.
func v1LakeFill(t *testing.T, ledger uint32) events.Event {
	t.Helper()
	fh, err := os.Open(filepath.Join("..", "..", "..", "test", "fixtures", "blend", "v1-pool-auctions", "lake_events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var r struct {
			Ledger     uint32   `json:"ledger_seq"`
			CloseTime  string   `json:"close_time"`
			TxHash     string   `json:"tx_hash"`
			OpIndex    int      `json:"op_index"`
			EventIndex int      `json:"event_index"`
			ContractID string   `json:"contract_id"`
			Topics     []string `json:"topics_xdr"`
			Data       string   `json:"data_xdr"`
		}
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		if r.Ledger == ledger {
			return events.Event{
				Type: "contract", ContractID: r.ContractID, Ledger: r.Ledger,
				LedgerClosedAt: strings.Replace(r.CloseTime, " ", "T", 1) + "Z", TxHash: r.TxHash, OperationIndex: r.OpIndex,
				EventIndex: r.EventIndex, Topic: r.Topics, Value: r.Data, InSuccessfulContractCall: true,
			}
		}
	}
	t.Fatalf("no lake row at %d", ledger)
	return events.Event{}
}

// The real BLNT interest fill decodes to one cause event per lot asset, each the negated lot amount; a foreign
// contract's identical event, another auction type and a rolled-back call give nothing.
func TestInterestEventDecoder_BLNTInterestFill(t *testing.T) {
	d := newInterestEventDecoder(map[string]lendinginterest.Protocol{blntFixturePool: lendinginterest.BLNT})
	ev := blntInterestFill(t)
	got, err := d.decode(ev)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA": "-20241787",
		"CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75": "-62201350",
		"CDTKPWPLOURQA2SGTKTUQOWRCBZEORB4BWBOMJ3D3ZTQQSGE5F6JBQLV": "-120688134",
	}
	if len(got) != len(want) {
		t.Fatalf("%d events, want %d: %+v", len(got), len(want), got)
	}
	for _, e := range got {
		if e.Cause != lendinginterest.CauseInterestFill || e.Pool != blntFixturePool || e.Ledger != 65225244 ||
			e.TxHash != ev.TxHash || e.EventIndex != 3 || want[e.Asset] != e.Amount.String() {
			t.Errorf("%+v", e)
		}
	}
	foreign := ev
	foreign.ContractID = "CFOREIGNPOOLWITHTHESAMETOPIC"
	liquidation := ev
	liquidation.Topic = slices.Clone(ev.Topic)
	liquidation.Topic[1] = "AAAAAwAAAAA=" // auction_type 0
	failed := ev
	failed.InSuccessfulContractCall = false
	for name, e := range map[string]events.Event{"foreign contract": foreign, "user liquidation": liquidation, "failed call": failed} {
		if got, err := d.decode(e); err != nil || len(got) != 0 {
			t.Errorf("%s: %+v %v, want nothing", name, got, err)
		}
	}
	// The same bytes on a Blend V1 pool are the other pool version's body: undecodable, never guessed.
	v1 := newInterestEventDecoder(map[string]lendinginterest.Protocol{blntFixturePool: lendinginterest.BlendV1})
	if got, err := v1.decode(ev); !errors.Is(err, errInterestEventUndecodable) || len(got) != 1 ||
		got[0].Cause != lendinginterest.CauseUndecodable || got[0].TxHash != ev.TxHash || got[0].Amount != nil {
		t.Errorf("a V2-shaped fill on a V1 pool: %+v %v, want its undecodable marker", got, err)
	}
}

// A BLNT fill whose auction_type is missing or not a u32 cannot be told from an interest fill: it fails, so its
// transaction gets the undecodable marker, never a silent skip.
func TestBLNTEvent_UnreadableAuctionTypeFails(t *testing.T) {
	base := lendinginterest.Event{Pool: blntFixturePool, Ledger: 9, TxHash: "tx"}
	for name, attrs := range map[string]map[string]any{
		"missing":     {"filled_auction_data": map[string]any{}},
		"wrong typed": {"auction_type": int64(2), "filled_auction_data": map[string]any{}},
	} {
		got, err := blntEvent([]consumer.Event{blnt.Event{EventType: "fill_auction", Attributes: attrs}}, base)
		if err == nil || !strings.Contains(err.Error(), "auction_type") || got != nil {
			t.Errorf("%s: %+v %v, want an error", name, got, err)
		}
	}
	got, err := blntEvent([]consumer.Event{blnt.Event{EventType: "fill_auction", Attributes: map[string]any{"auction_type": uint32(0)}}}, base)
	if err != nil || got != nil {
		t.Errorf("a liquidation fill: %+v %v, want nothing", got, err)
	}
}

// The values below are hand-encoded from the shapes the decoders document (blnt/abi.go gulp and reconcile_loss,
// blend decode_money_market.go gulp), not captured chain bytes.
func scvB64(t *testing.T, v xdr.ScVal) string {
	t.Helper()
	raw, err := v.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func scvI128(hi int64, lo uint64) xdr.ScVal {
	p := xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &p}
}

func scvContract(t *testing.T, fill byte) (xdr.ScVal, string) {
	t.Helper()
	var cid xdr.ContractId
	for i := range cid {
		cid[i] = fill
	}
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr}, strkey.MustEncode(strkey.VersionByteContract, cid[:])
}

func poolEvent(t *testing.T, pool string, ledger uint32, tx string, index int, topics []xdr.ScVal, body xdr.ScVal) events.Event {
	t.Helper()
	enc := make([]string, len(topics))
	for i, v := range topics {
		enc[i] = scvB64(t, v)
	}
	return events.Event{
		Type: "contract", ContractID: pool, Ledger: ledger, LedgerClosedAt: liClose(ledger).Format(time.RFC3339), TxHash: tx,
		EventIndex: index, Topic: enc, Value: scvB64(t, body), InSuccessfulContractCall: true,
	}
}

func scvSym(s string) xdr.ScVal {
	v := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &v}
}

// gulp is +token_delta and BLNT's reconcile_loss −backstop_credit_loss, each for the topic's asset, on all 128 bits;
// a gulp on a V1 pool, which has none, is its transaction's undecodable marker.
func TestInterestEventDecoder_GulpAndReconcileLoss(t *testing.T) {
	asset, assetKey := scvContract(t, 0x11)
	wide := scvI128(1, 5) // 2^64 + 5
	const want = "18446744073709551621"
	vec := func(vs ...xdr.ScVal) xdr.ScVal {
		v := xdr.ScVec(vs)
		pv := &v
		return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pv}
	}
	cases := []struct {
		name  string
		proto lendinginterest.Protocol
		ev    events.Event
		cause lendinginterest.Cause
		amt   string
	}{
		{
			"BLNT gulp", lendinginterest.BLNT,
			poolEvent(t, blntFixturePool, 10, "tx", 0, []xdr.ScVal{scvSym("gulp"), asset}, wide), lendinginterest.CauseGulp, want,
		},
		{
			"BLNT reconcile_loss", lendinginterest.BLNT,
			poolEvent(t, blntFixturePool, 10, "tx", 1, []xdr.ScVal{scvSym("reconcile_loss"), asset},
				vec(scvI128(0, 1), scvI128(0, 2), wide, scvI128(0, 4))), lendinginterest.CauseReconcileLoss, "-" + want,
		},
		{
			"V2 gulp", lendinginterest.BlendV2,
			poolEvent(t, blntFixturePool, 10, "tx", 2, []xdr.ScVal{scvSym("gulp"), asset}, wide), lendinginterest.CauseGulp, want,
		},
	}
	for _, c := range cases {
		d := newInterestEventDecoder(map[string]lendinginterest.Protocol{blntFixturePool: c.proto})
		got, err := d.decode(c.ev)
		if err != nil || len(got) != 1 || got[0].Cause != c.cause || got[0].Asset != assetKey || got[0].Amount.String() != c.amt ||
			got[0].Pool != blntFixturePool || got[0].EventIndex != uint32(c.ev.EventIndex) { //nolint:gosec // small
			t.Errorf("%s: %+v %v, want %s %s of %s", c.name, got, err, c.cause, c.amt, assetKey)
		}
	}
	d := newInterestEventDecoder(map[string]lendinginterest.Protocol{blntFixturePool: lendinginterest.BlendV1})
	if got, err := d.decode(cases[2].ev); !errors.Is(err, errInterestEventUndecodable) || len(got) != 1 ||
		got[0].Cause != lendinginterest.CauseUndecodable {
		t.Errorf("V1 gulp: %+v %v, want the undecodable marker", got, err)
	}
}

// A real V1 interest fill (auction_type 2, no lot in the body) is a CauseInterestFillV1 with no amount; a real V1
// user-liquidation fill is nothing.
func TestInterestEventDecoder_V1InterestFill(t *testing.T) {
	fill := v1LakeFill(t, 52504175)
	d := newInterestEventDecoder(map[string]lendinginterest.Protocol{fill.ContractID: lendinginterest.BlendV1})
	got, err := d.decode(fill)
	if err != nil || len(got) != 1 || got[0].Cause != lendinginterest.CauseInterestFillV1 || got[0].Amount != nil || got[0].Asset != "" {
		t.Fatalf("%+v %v", got, err)
	}
	if got, err := d.decode(v1LakeFill(t, 51612222)); err != nil || len(got) != 0 {
		t.Fatalf("user liquidation: %+v %v", got, err)
	}
}

// The in-line identity refuses a row whose classes do not add up to its residual or its unreconciled count.
func TestCheckResidualClasses(t *testing.T) {
	n := big.NewInt
	ok := lendinginterest.HourRow{
		Pool: "P", Asset: "A", BackstopResidual: n(-10), UnreconciledVersions: 3,
		ResidualExplained: n(-7), ResidualAttributed: n(-4), ResidualUnexplained: n(1),
		ExplainedVersions: 1, AttributedVersions: 1, UnexplainedVersions: 1,
	}
	if err := checkResidualClasses(ok); err != nil {
		t.Fatal(err)
	}
	amount, count := ok, ok
	amount.ResidualUnexplained = n(0)
	count.UnexplainedVersions = 0
	for name, r := range map[string]lendinginterest.HourRow{"amount": amount, "count": count} {
		if err := checkResidualClasses(r); err == nil {
			t.Errorf("%s off: accepted", name)
		}
	}
}

// The window line splits unreconciled versions by class and names each row holding an unexplained one. The V1
// row is the golden interest fill at 53013801, attributed by design; the second residual has no event at all.
func TestReportInterestWindow_ResidualClasses(t *testing.T) {
	n := big.NewInt
	from := time.Date(2024, 8, 13, 0, 0, 0, 0, time.UTC)
	v1Fill := lendinginterest.HourRow{
		Pool: "CV1POOL", Asset: "CUSDC", Protocol: lendinginterest.BlendV1, Hour: from.Add(14 * time.Hour),
		FirstLedger: 53013790, LastLedger: 53013801, Versions: 3, UnreconciledVersions: 1, AttributedVersions: 1,
		BackstopResidual: n(-3096224423), ResidualAttributed: n(-3096224423), ResidualExplained: n(0), ResidualUnexplained: n(0),
	}
	line := func(rows ...lendinginterest.HourRow) string {
		var b strings.Builder
		reportInterestWindow(&b, from, from.Add(24*time.Hour), rows, interestChecked{}, true)
		return b.String()
	}
	out := line(v1Fill)
	if !strings.Contains(out, "unreconciled 1 (b_rate mismatches 0, fee mismatches 0, rows with a backstop residual 1; "+
		"versions explained 0, attributed 1, unexplained 0)") || strings.Contains(out, ": unexplained CV1POOL") {
		t.Fatalf("attributed V1 fill:\n%s", out)
	}
	drift := v1Fill
	drift.Hour, drift.FirstLedger, drift.LastLedger, drift.AttributedVersions = from.Add(15*time.Hour), 53014000, 53014400, 0
	drift.BackstopResidual, drift.ResidualAttributed, drift.UnexplainedVersions, drift.ResidualUnexplained = n(-1234), n(0), 1, n(-1234)
	out = line(v1Fill, drift)
	for _, want := range []string{
		"versions explained 0, attributed 1, unexplained 1)",
		lendingInterestJob + ": hours [2024-08-13T00:00:00Z, 2024-08-14T00:00:00Z): unexplained CV1POOL CUSDC 2024-08-13T15:00:00Z: " +
			"backstop residual -1234 (unexplained -1234); versions explained 0, attributed 0, unexplained 1; ledgers 53014000-53014400\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("window line lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, ": unexplained CV1POOL") != 1 {
		t.Errorf("want one sample, the drift's:\n%s", out)
	}
}

// The unexplained gauge counts neither class the events account for: an attributed V1 fill is not unexplained.
func TestStoredUnexplained(t *testing.T) {
	row := func(unreconciled, explained, attributed uint32) clickhouse.LendingInterestStoredRow {
		return clickhouse.LendingInterestStoredRow{
			Row:               lendinginterest.HourRow{UnreconciledVersions: unreconciled},
			ExplainedVersions: explained, AttributedVersions: attributed,
		}
	}
	for _, c := range []struct {
		row  clickhouse.LendingInterestStoredRow
		want int64
	}{{row(1, 0, 1), 0}, {row(3, 1, 2), 0}, {row(2, 0, 1), 1}, {row(1, 0, 0), 1}} {
		if got := storedUnexplained(c.row); got != c.want {
			t.Errorf("unreconciled %d explained %d attributed %d: %d, want %d", c.row.Row.UnreconciledVersions,
				c.row.ExplainedVersions, c.row.AttributedVersions, got, c.want)
		}
	}
}

// backstopOnly makes the fake's version i its predecessor with backstop_credit moved by d: no accrual, b_rate
// unmoved, so its pair is a backstop residual the pass classifies by its transaction's events. Every other version of
// the fake moves b_rate and is never classified.
func backstopOnly(f *fakeInterestLake, i int, d int64) {
	r := f.versions[i-1].Reserve
	r.BackstopCredit = new(big.Int).Add(r.BackstopCredit, big.NewInt(d))
	f.versions[i].Reserve = r
}

// The pass reads the pools' events with the versions and proves every classified ledger's events before writing: the
// census gets each such ledger with the cause events read there, the carried open hour's included, and a census
// refusal holds the watermark with nothing inserted. A ledger whose residual is never classified is not censused.
func TestInterestPass_EventCensus(t *testing.T) {
	f := newFakeInterestLake()
	backstopOnly(f, 2, 5)
	liq := v1LakeFill(t, 51612222) // decodes to nothing on any pool: read and counted, never a cause
	liq.ContractID, liq.Ledger, liq.TxHash = liPool, 199_998, "tx199998"
	f.events = []events.Event{liq}
	f.install(t)
	if out, err := runInterest(t, lendingRun{}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var reads198 []uint64
	for _, c := range f.censused {
		if n, ok := c[199_998]; ok {
			reads198 = append(reads198, n)
		}
		if _, ok := c[199_994]; ok {
			t.Errorf("census calls %v: ledger 199994 (b_rate moved) censused", f.censused)
		}
	}
	if !slices.Equal(reads198, []uint64{1}) {
		t.Fatalf("census calls %v: ledger 199998 (read in the first window, committed in the second) proved %v, want once with 1 read",
			f.censused, reads198)
	}
	for _, c := range []struct {
		gap     uint32
		refuses bool
	}{{199_994, false}, {199_998, true}} {
		g := newFakeInterestLake()
		backstopOnly(g, 2, 5)
		g.eventGaps = map[uint32]bool{c.gap: true}
		g.install(t)
		_, err := runInterest(t, lendingRun{})
		passed := slices.ContainsFunc(g.marks, func(m [2]uint32) bool { return m[1] >= c.gap })
		if c.refuses != errors.Is(err, clickhouse.ErrLendingInterestEventShortfall) || c.refuses == passed || (!c.refuses && err != nil) {
			t.Fatalf("shortfall at %d: err %v inserted %d marks %v, want refused %v", c.gap, err, len(g.inserted), g.marks, c.refuses)
		}
	}
}

// undecodableFill is a real V1 interest fill moved onto the fake V2 pool: the other pool version's body, so it
// does not decode.
func undecodableFill(t *testing.T, ledger uint32, index int) events.Event {
	t.Helper()
	ev := v1LakeFill(t, 52504175)
	ev.ContractID, ev.Ledger, ev.TxHash, ev.EventIndex = liPool, ledger, fmt.Sprintf("tx%d", ledger), index
	ev.LedgerClosedAt = liClose(ledger).Format(time.RFC3339)
	return ev
}

// An undecodable cause event never refuses: in a window with no classified residual the pass writes and advances,
// logs the event and counts it on its heartbeat once, though the open hour carries it into a second window.
func TestInterestPass_UndecodableEventAdvances(t *testing.T) {
	f := newFakeInterestLake()
	f.events = []events.Event{undecodableFill(t, 199_998, 0)}
	f.install(t)
	hb, read := liHeartbeat(t)
	out, err := runInterest(t, lendingRun{heartbeat: hb})
	if err != nil || len(f.marks) == 0 || len(f.inserted) == 0 {
		t.Fatalf("err %v marks %v inserted %d\n%s", err, f.marks, len(f.inserted), out)
	}
	if !strings.Contains(out, "event 199998 tx199998/0: undecodable") {
		t.Errorf("log does not name the event:\n%s", out)
	}
	if got := metric(read(), lendingInterestUndecodableMetric); got != "1" {
		t.Errorf("%s = %q, want 1", lendingInterestUndecodableMetric, got)
	}
	clean := newFakeInterestLake()
	clean.install(t)
	hb, read = liHeartbeat(t)
	if _, err := runInterest(t, lendingRun{heartbeat: hb}); err != nil || metric(read(), lendingInterestUndecodableMetric) != "0" {
		t.Fatalf("clean run: %v, gauge %q, want a 0 series", err, metric(read(), lendingInterestUndecodableMetric))
	}
}

// A classified residual whose transaction holds an undecodable event is written unexplained, and the proof's fresh
// fold of the same read agrees with the writer's row.
func TestInterestPass_UndecodableEventProves(t *testing.T) {
	p := newProofFixture(t, func(f *fakeInterestLake) {
		backstopOnly(f, 2, 5)
		f.events = []events.Event{undecodableFill(t, 199_998, 3)}
	})
	var explained, unexplained uint32
	for _, r := range p.inserted {
		explained += r.ExplainedVersions + r.AttributedVersions
		unexplained += r.UnexplainedVersions
	}
	if explained != 0 || unexplained == 0 {
		t.Fatalf("explained+attributed %d unexplained %d, want the poisoned residual unexplained", explained, unexplained)
	}
	r := lendingRun{heartbeat: filepath.Join(t.TempDir(), "state.prom")}
	if out, gauge, err := p.prove(t, r); err != nil || gauge != "0" {
		t.Fatalf("proof: %v gauge %s\n%s", err, gauge, out)
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(r.heartbeat), "ops_job_"+r.proveJob()+".prom")) //nolint:gosec // test temp dir
	if err != nil || metric(string(raw), metricInterestUnexplained) != fmt.Sprint(unexplained) {
		t.Fatalf("%s: %v\n%s, want %d", metricInterestUnexplained, err, raw, unexplained)
	}
}
