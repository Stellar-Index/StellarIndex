package blend

import (
	"bufio"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Real r1 lake rows from the four V1 pools (WASM baf978f1…); see
// test/fixtures/blend/v1-pool-auctions/README.md.
type v1LakeRow struct {
	LedgerSeq  uint32   `json:"ledger_seq"`
	CloseTime  string   `json:"close_time"`
	TxHash     string   `json:"tx_hash"`
	OpIndex    int      `json:"op_index"`
	EventIndex int      `json:"event_index"`
	ContractID string   `json:"contract_id"`
	TopicsXDR  []string `json:"topics_xdr"`
	DataXDR    string   `json:"data_xdr"`
}

func loadV1PoolAuctionEvents(t *testing.T) map[uint32]events.Event {
	t.Helper()
	path := filepath.Join("..", "..", "..", "test", "fixtures", "blend", "v1-pool-auctions", "lake_events.jsonl")
	f, err := os.Open(path) //nolint:gosec // repo-relative, test-only
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()
	out := map[uint32]events.Event{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var r v1LakeRow
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("fixture row: %v", err)
		}
		if _, dup := out[r.LedgerSeq]; dup {
			t.Fatalf("fixture holds two rows at ledger %d; key the map wider", r.LedgerSeq)
		}
		out[r.LedgerSeq] = events.Event{
			Type:                     "contract",
			Ledger:                   r.LedgerSeq,
			LedgerClosedAt:           strings.Replace(r.CloseTime, " ", "T", 1) + "Z",
			ContractID:               r.ContractID,
			TxHash:                   r.TxHash,
			OperationIndex:           r.OpIndex,
			EventIndex:               r.EventIndex,
			InSuccessfulContractCall: true,
			Topic:                    r.TopicsXDR,
			Value:                    r.DataXDR,
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan fixture: %v", err)
	}
	if len(out) != 19 {
		t.Fatalf("fixture rows = %d, want 19", len(out))
	}
	return out
}

func decodeOne(t *testing.T, ev events.Event) any {
	t.Helper()
	outs, err := NewDecoder().Decode(ev)
	if err != nil {
		t.Fatalf("ledger %d: Decode: %v", ev.Ledger, err)
	}
	if len(outs) != 1 {
		t.Fatalf("ledger %d: %d outputs, want 1", ev.Ledger, len(outs))
	}
	return outs[0]
}

func wantAmounts(t *testing.T, label string, got []AssetAmount, want map[string]int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d entries, want %d (%+v)", label, len(got), len(want), got)
	}
	for _, a := range got {
		w, ok := want[a.Asset.String()]
		if !ok || a.Amount.Cmp(big.NewInt(w)) != 0 {
			t.Errorf("%s: %s=%v, want %v", label, a.Asset.String(), a.Amount, w)
		}
	}
}

const (
	v1PoolCDVQ = "CDVQVKOY2YSXS2IC7KN6MNASSHPAO7UN2UR2ON4OI2SKMFJNVAMDX6DP"
	v1PoolCDE6 = "CDE65QK2ROZ32V2LVLBOKYPX47TYMYO37Z6ASQTBRTBNK53C7C6QF4Y7"
	v1USDC     = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	v1XLM      = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	v1BLNDLP   = "CAS3FL6TLZKDGGSISDBWGGPXT3NRR4DYTZD7YOD3HMYO6LTJUVGRVEAM"
)

// Every captured row decodes through the real dispatcher, and each event
// kind lands in the struct its table expects.
func TestV1PoolAuctions_AllFixtureRowsDecode(t *testing.T) {
	t.Parallel()
	kinds := map[string]int{}
	for _, ev := range loadV1PoolAuctionEvents(t) {
		switch o := decodeOne(t, ev).(type) {
		case NewAuctionEvent:
			kinds["new"]++
		case FillAuctionEvent:
			kinds["fill"]++
			if o.Data != nil {
				t.Errorf("ledger %d: V1 fill Data = %+v, want nil (no auction data on the wire)", ev.Ledger, o.Data)
			}
		case EmissionEvent:
			kinds[o.Kind]++
		default:
			t.Errorf("ledger %d: unexpected output %T", ev.Ledger, o)
		}
	}
	want := map[string]int{"new": 6, "fill": 10, EventBadDebt: 3}
	for k, n := range want {
		if kinds[k] != n {
			t.Errorf("%s outputs = %d, want %d (all: %v)", k, kinds[k], n, kinds)
		}
	}
}

// Ledger 52,504,175: [fill_auction, Address(user), u32(2)], body
// (filler, i128 100). User is the V1 backstop (Interest auction).
func TestGolden_FillAuctionV1(t *testing.T) {
	t.Parallel()
	evs := loadV1PoolAuctionEvents(t)
	cases := []struct {
		ledger   uint32
		pool     string
		typ      uint32
		user     string
		filler   string
		pct      int64
		evIndex  uint32
		txPrefix string
	}{
		{51_612_222, v1PoolCDVQ, AuctionTypeUserLiquidation, "GDTZSZTGBXPBV4GC7AUYNWUWCAT56CNYSKR7IOIZYNAOPIWCWWHMTIPS", "GASND6BBFGDGWDLP2DJCFDAKL7GHHZAYQ6PENFHSHMLEMFKAVLZCDQXJ", 100, 0, "2a55240c"},
		{52_430_677, v1PoolCDVQ, AuctionTypeBadDebt, MainnetBackstopV1, "GAWL2CH2JH5APO5GJZ5OH6SILW4XUFW7Z72FZDAAUOLK5QZTS363QDDC", 100, 2, "868b7762"},
		{52_504_175, v1PoolCDVQ, AuctionTypeInterest, MainnetBackstopV1, "GAWL2CH2JH5APO5GJZ5OH6SILW4XUFW7Z72FZDAAUOLK5QZTS363QDDC", 100, 4, "61864915"},
		{55_458_184, "CAQF5KNOFIGRI24NQRRGUPD46Q45MGMXZMRTQFXS25Y4NZVNPT34GM6S", AuctionTypeUserLiquidation, "GAUPKZVR6HID4CUJ5SF3VCV4VRZTXAL2XLJTCYTTDPMHAYBTPXQW6IQO", "GAVG3ODZ4SAVK2WJL3F3RT265RL7P6QNOMA6NL3XDAREKSX3OWWMXF4R", 3, 0, "fb5acf38"},
		{62_625_124, v1PoolCDE6, AuctionTypeUserLiquidation, "GA3COWJGKAWTO3TOWLBKN6CHWJCSRTPASFZVNN3X3MRR6W4SAUEOYEYX", "GBPUFOPDVPGM56MIO4KYSNV2N4XLYBZAXL6QMD75JWG7GC5L6ANFSYBL", 100, 0, "2b87c1e9"},
	}
	for _, c := range cases {
		out, ok := decodeOne(t, evs[c.ledger]).(FillAuctionEvent)
		if !ok {
			t.Fatalf("ledger %d: not a FillAuctionEvent", c.ledger)
		}
		if out.Pool != c.pool || out.AuctionType != c.typ || out.User != c.user || out.Filler != c.filler {
			t.Errorf("ledger %d: got pool=%s type=%d user=%s filler=%s, want %s %d %s %s",
				c.ledger, out.Pool, out.AuctionType, out.User, out.Filler, c.pool, c.typ, c.user, c.filler)
		}
		if out.FillPercent == nil || out.FillPercent.Cmp(big.NewInt(c.pct)) != 0 {
			t.Errorf("ledger %d: FillPercent=%v want %d", c.ledger, out.FillPercent, c.pct)
		}
		if out.Ledger != c.ledger || out.EventIndex != c.evIndex || !strings.HasPrefix(out.TxHash, c.txPrefix) {
			t.Errorf("ledger %d: coords ledger=%d ei=%d tx=%s", c.ledger, out.Ledger, out.EventIndex, out.TxHash)
		}
		if out.Data != nil {
			t.Errorf("ledger %d: Data=%+v, want nil", c.ledger, out.Data)
		}
	}
}

// V1 new_auction topics are [Symbol, u32(auction_type)] with no user: the
// user is the V1 backstop and percent is 100. The bad_debt rows one ledger
// earlier move exactly the bid's dTokens onto the backstop, which is the
// evidence that the auction covers the whole liability.
func TestGolden_NewAuctionV1(t *testing.T) {
	t.Parallel()
	evs := loadV1PoolAuctionEvents(t)
	cases := []struct {
		ledger uint32
		pool   string
		typ    uint32
		bid    map[string]int64
		lot    map[string]int64
		block  uint32
	}{
		{
			52_428_305, v1PoolCDVQ, AuctionTypeBadDebt,
			map[string]int64{v1USDC: 8_565_694},
			map[string]int64{v1BLNDLP: 31_596_056},
			52_428_306,
		},
		{
			52_503_940, v1PoolCDVQ, AuctionTypeInterest,
			map[string]int64{v1BLNDLP: 270_358_315_554},
			map[string]int64{v1XLM: 351_343_973, v1USDC: 59_928_017_284},
			52_503_941,
		},
		{
			55_570_396, v1PoolCDE6, AuctionTypeBadDebt,
			map[string]int64{"CBCO65UOWXY2GR66GOCMCN6IU3Y45TXCPBY3FLUNL4AOUMOCKVIVV6JC": 11_642_263},
			map[string]int64{v1BLNDLP: 39_552_757},
			55_570_397,
		},
	}
	for _, c := range cases {
		out, ok := decodeOne(t, evs[c.ledger]).(NewAuctionEvent)
		if !ok {
			t.Fatalf("ledger %d: not a NewAuctionEvent", c.ledger)
		}
		if out.Pool != c.pool || out.AuctionType != c.typ || out.User != MainnetBackstopV1 || out.Percent != 100 {
			t.Errorf("ledger %d: got pool=%s type=%d user=%s percent=%d", c.ledger, out.Pool, out.AuctionType, out.User, out.Percent)
		}
		if out.Data.Block != c.block {
			t.Errorf("ledger %d: Block=%d want %d", c.ledger, out.Data.Block, c.block)
		}
		wantAmounts(t, "bid", out.Data.Bid, c.bid)
		wantAmounts(t, "lot", out.Data.Lot, c.lot)
	}
}

// V1 bad_debt: [Symbol, Address(user)], body (asset: Address, d_tokens: i128).
func TestGolden_BadDebtV1(t *testing.T) {
	t.Parallel()
	evs := loadV1PoolAuctionEvents(t)
	cases := []struct {
		ledger uint32
		pool   string
		user   string
		asset  string
		amount int64
	}{
		{52_428_304, v1PoolCDVQ, "GDTEIX6BJJCXUGUMIZDCLM5XMJOJZKU7HIGXVM37XYNOXLDICEFRURGK", v1USDC, 8_565_694},
		{55_570_394, v1PoolCDE6, "GDRRWCHXH5XMB54UAAD6TTYNHZTQ5QY7IKXMXZHGP2XA6P3FG75GJTUV", "CBCO65UOWXY2GR66GOCMCN6IU3Y45TXCPBY3FLUNL4AOUMOCKVIVV6JC", 4_071_398},
		{55_570_395, v1PoolCDE6, "GCNILL7CSIEG5THCOUXZK6FYFKVRPGMPLTINSZE6MOK7BK7Q4CN4OWTZ", "CBCO65UOWXY2GR66GOCMCN6IU3Y45TXCPBY3FLUNL4AOUMOCKVIVV6JC", 7_570_865},
	}
	for _, c := range cases {
		out, ok := decodeOne(t, evs[c.ledger]).(EmissionEvent)
		if !ok {
			t.Fatalf("ledger %d: not an EmissionEvent", c.ledger)
		}
		if out.Kind != EventBadDebt || out.Pool != c.pool || out.User != c.user || out.Asset != c.asset {
			t.Errorf("ledger %d: got kind=%s pool=%s user=%s asset=%s", c.ledger, out.Kind, out.Pool, out.User, out.Asset)
		}
		if out.Amount == nil || out.Amount.Cmp(big.NewInt(c.amount)) != 0 {
			t.Errorf("ledger %d: Amount=%v want %d", c.ledger, out.Amount, c.amount)
		}
	}
}

// The derived new_auction user is only sound while every V1 BadDebt /
// Interest fill names the V1 backstop.
func TestV1PoolAuctions_BackstopAuctionsKeyOnV1Backstop(t *testing.T) {
	t.Parallel()
	n := 0
	for _, ev := range loadV1PoolAuctionEvents(t) {
		fill, ok := decodeOne(t, ev).(FillAuctionEvent)
		if !ok || fill.AuctionType == AuctionTypeUserLiquidation {
			continue
		}
		n++
		if fill.User != MainnetBackstopV1 {
			t.Errorf("ledger %d: backstop-auction fill user %s, want %s", ev.Ledger, fill.User, MainnetBackstopV1)
		}
	}
	if n == 0 {
		t.Fatal("fixture holds no BadDebt/Interest fills")
	}
}

// A 2-topic new_auction naming UserLiquidation is not a V1 shape (V1 uses
// new_liquidation_auction); attributing it to the backstop would be wrong.
func TestNewAuctionV1_RejectsUserLiquidation(t *testing.T) {
	t.Parallel()
	ev := loadV1PoolAuctionEvents(t)[52_428_305]
	ev.Topic = []string{ev.Topic[0], "AAAAAwAAAAA="} // u32(0)
	if _, err := NewDecoder().Decode(ev); !errors.Is(err, ErrMalformedPayload) {
		t.Fatalf("err = %v, want ErrMalformedPayload", err)
	}
}
