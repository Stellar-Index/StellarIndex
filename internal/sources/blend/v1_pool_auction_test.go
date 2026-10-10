package blend

import (
	"bufio"
	"encoding/json"
	"errors"
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
