package diagnostics

import (
	"bytes"
	"log/slog"
	"math/big"
	"strings"
	"testing"
)

// A range where neither side returned a row compared nothing; the verdict
// must fail rather than log "every ledger matches" and exit 0.
func TestHubbleCountVerdict_EmptyBothSidesIsError(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	err := hubbleCountVerdict(logger, 90_000_000, 90_000_100, map[uint32]int{}, map[uint32]int{}, 50)
	if err == nil {
		t.Fatalf("empty comparison returned nil; log: %s", buf.String())
	}
	if !strings.Contains(err.Error(), "nothing was compared") {
		t.Errorf("error %q does not name the empty comparison", err)
	}
	if strings.Contains(buf.String(), "OK") {
		t.Errorf("empty comparison logged an OK verdict: %s", buf.String())
	}
}

func TestHubbleStatVerdict_EmptyBothSidesIsError(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	if err := hubbleStatVerdict(logger, 100, 200, nil, nil, 50, 0); err == nil {
		t.Fatal("empty -with-amounts comparison returned nil")
	}
}

// ledgers_checked counts distinct ledgers, not ours+theirs rows: two
// ledgers present on both sides is 2.
func TestHubbleCountVerdict_LedgersCheckedIsDistinct(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	both := map[uint32]int{100: 3, 101: 1}
	if err := hubbleCountVerdict(logger, 100, 200, both, map[uint32]int{100: 3, 101: 1}, 50); err != nil {
		t.Fatalf("matching counts returned %v", err)
	}
	if !strings.Contains(buf.String(), "ledgers_checked=2") {
		t.Errorf("want ledgers_checked=2, log: %s", buf.String())
	}

	buf.Reset()
	st := map[uint32]ledgerStats{100: {Count: 1, SumSell: big.NewInt(5), SumBuy: big.NewInt(7)}}
	st2 := map[uint32]ledgerStats{100: {Count: 1, SumSell: big.NewInt(5), SumBuy: big.NewInt(7)}}
	if err := hubbleStatVerdict(logger, 100, 200, st, st2, 50, 0); err != nil {
		t.Fatalf("matching stats returned %v", err)
	}
	if !strings.Contains(buf.String(), "ledgers_checked=1") {
		t.Errorf("want ledgers_checked=1, log: %s", buf.String())
	}
}
