package phoenix

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Replay captured Phoenix swaps (8 events each, grouped by tx_hash + op_index) into the RawSwap
// collator and real decoder, validating the mainnet wire format. Fixtures live at
// test/fixtures/phoenix/<wasm_hash>/swap_*.json, one file per complete swap.

type phoenixSwapFixture struct {
	WasmHash       string `json:"wasm_hash"`
	Ledger         uint32 `json:"ledger"`
	TxHash         string `json:"tx_hash"`
	OpIndex        uint32 `json:"op_index"`
	ContractID     string `json:"contract_id"`
	LedgerClosedAt string `json:"ledger_closed_at"`
	Events         []struct {
		Topics []string `json:"topics"`
		Value  string   `json:"value"`
	} `json:"events"`
}

func TestRealMainnetFixtures_phoenix(t *testing.T) {
	root := filepath.Join("..", "..", "..", "test", "fixtures", "phoenix")
	dirs, err := os.ReadDir(root)
	if err != nil {
		t.Skipf("fixtures root unreadable: %v", err)
	}
	total := 0
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		wasmHash := d.Name()
		dir := filepath.Join(root, wasmHash)
		files, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if filepath.Ext(f.Name()) != ".json" {
				continue
			}
			total++
			t.Run(wasmHash+"/"+f.Name(), func(t *testing.T) {
				runPhoenixFixture(t, filepath.Join(dir, f.Name()))
			})
		}
	}
	if total == 0 {
		t.Skip("no fixtures present")
	}
}

func runPhoenixFixture(t *testing.T, path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var fx phoenixSwapFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(fx.Events) != SwapFieldCount {
		t.Fatalf("fixture has %d events, want %d", len(fx.Events), SwapFieldCount)
	}

	closedAt, err := time.Parse(time.RFC3339, fx.LedgerClosedAt)
	if err != nil {
		t.Fatalf("parse closed_at: %v", err)
	}

	// Same classify + assign path as processPage: classify() must match real topic[0] bytes, each
	// of the 8 topic[1] field names must route to the right RawSwap slot, and decodeSwap must
	// assemble the final canonical.Trade.
	var raw_ RawSwap
	raw_.Ledger = fx.Ledger
	raw_.TxHash = fx.TxHash
	raw_.OpIndex = fx.OpIndex
	raw_.Pool = fx.ContractID
	raw_.ClosedAt = closedAt

	for i, ev := range fx.Events {
		e := &events.Event{
			Topic: ev.Topics,
			Value: ev.Value,
		}
		field, isSwap := classify(e)
		if !isSwap {
			t.Fatalf("event %d: classify rejected a real Phoenix swap event (topic[0] bytes wrong?)", i)
		}
		if err := raw_.assign(e, field); err != nil {
			t.Fatalf("event %d: assign(%q): %v", i, field, err)
		}
	}
	if !raw_.Complete() {
		t.Fatalf("collation incomplete: %d/8 fields populated", raw_.fieldsPresent())
	}

	trade, err := decodeSwap(&raw_)
	if err != nil {
		t.Fatalf("decodeSwap: %v", err)
	}
	if trade.BaseAmount.Sign() <= 0 {
		t.Errorf("base amount not positive: %s", trade.BaseAmount)
	}
	if trade.QuoteAmount.Sign() <= 0 {
		t.Errorf("quote amount not positive: %s", trade.QuoteAmount)
	}
	if trade.Pair.Base.Type != canonical.AssetSoroban {
		t.Errorf("base type = %q, want soroban", trade.Pair.Base.Type)
	}
	if trade.Pair.Quote.Type != canonical.AssetSoroban {
		t.Errorf("quote type = %q, want soroban", trade.Pair.Quote.Type)
	}
	if trade.Taker == "" {
		t.Error("taker address empty")
	}
	// Sanity: i128 amounts shouldn't be > 2^100 for real tokens.
	maxSane := new(big.Int).Lsh(big.NewInt(1), 100)
	if trade.BaseAmount.BigInt().Cmp(maxSane) > 0 {
		t.Errorf("base unreasonably large — i128 misalign? %s", trade.BaseAmount)
	}
	if trade.QuoteAmount.BigInt().Cmp(maxSane) > 0 {
		t.Errorf("quote unreasonably large — i128 misalign? %s", trade.QuoteAmount)
	}
	// Timestamp carries through.
	if !trade.Timestamp.Equal(closedAt) {
		t.Errorf("timestamp = %v, want %v", trade.Timestamp, closedAt)
	}
}
