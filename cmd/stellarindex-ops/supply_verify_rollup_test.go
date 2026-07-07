package main

import (
	"bytes"
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/StellarIndex/stellar-index/internal/storage/timescale"
)

func kt(mint, burn, clawback int64) timescale.SEP41KindTotals {
	return timescale.SEP41KindTotals{
		Mint:     big.NewInt(mint),
		Burn:     big.NewInt(burn),
		Clawback: big.NewInt(clawback),
	}
}

// fakeRollupReader is a DB-free rollupTruthReader: it serves preset
// checkpoints and a preset re-sum ("truth") per contract, so the
// drift-computation core can be exercised without Postgres.
type fakeRollupReader struct {
	checkpoints []timescale.SEP41RollupCheckpoint
	// truth[contractID] is the authoritative re-sum the fake returns for
	// that contract (regardless of asOfLedger — the test controls it).
	truth map[string]timescale.SEP41KindTotals

	// gotContracts records the contractIDs filter passed to List, and
	// gotResumTimeout records the statement timeout passed to the re-sum,
	// so plumbing can be asserted.
	gotContracts    []string
	gotResumTimeout time.Duration
	gotResumLedgers map[string]uint32
}

func (f *fakeRollupReader) ListSEP41RollupCheckpoints(_ context.Context, contractIDs []string) ([]timescale.SEP41RollupCheckpoint, error) {
	f.gotContracts = contractIDs
	if len(contractIDs) == 0 {
		return f.checkpoints, nil
	}
	want := make(map[string]struct{}, len(contractIDs))
	for _, c := range contractIDs {
		want[c] = struct{}{}
	}
	var out []timescale.SEP41RollupCheckpoint
	for _, cp := range f.checkpoints {
		if _, ok := want[cp.ContractID]; ok {
			out = append(out, cp)
		}
	}
	return out, nil
}

func (f *fakeRollupReader) SEP41SupplyEventKindResum(_ context.Context, contractID string, asOfLedger uint32, statementTimeout time.Duration) (timescale.SEP41KindTotals, error) {
	f.gotResumTimeout = statementTimeout
	if f.gotResumLedgers == nil {
		f.gotResumLedgers = map[string]uint32{}
	}
	f.gotResumLedgers[contractID] = asOfLedger
	return f.truth[contractID], nil
}

// TestVerifyRollupDrifts_Agreement: checkpoint fold == authoritative
// re-sum for every contract ⇒ zero drift, and the check reports the full
// contract count.
func TestVerifyRollupDrifts_Agreement(t *testing.T) {
	r := &fakeRollupReader{
		checkpoints: []timescale.SEP41RollupCheckpoint{
			{ContractID: "C_A", Fold: kt(1000, 5, 0), LastLedger: 100},
			{ContractID: "C_B", Fold: kt(42, 0, 7), LastLedger: 250},
		},
		truth: map[string]timescale.SEP41KindTotals{
			"C_A": kt(1000, 5, 0),
			"C_B": kt(42, 0, 7),
		},
	}
	drifts, checked, err := verifyRollupDrifts(context.Background(), r, nil, big.NewInt(0), 5*time.Minute)
	if err != nil {
		t.Fatalf("verifyRollupDrifts: %v", err)
	}
	if len(drifts) != 0 {
		t.Fatalf("expected no drift on agreement, got %+v", drifts)
	}
	if checked != 2 {
		t.Fatalf("checked = %d, want 2", checked)
	}
	// Plumbing: the re-sum is bounded at each checkpoint's own last_ledger.
	if r.gotResumLedgers["C_A"] != 100 || r.gotResumLedgers["C_B"] != 250 {
		t.Fatalf("re-sum ledgers = %v, want C_A@100 C_B@250 (must bound at checkpoint last_ledger)", r.gotResumLedgers)
	}
	if r.gotResumTimeout != 5*time.Minute {
		t.Fatalf("statement timeout not threaded through: got %s", r.gotResumTimeout)
	}
}

// TestVerifyRollupDrifts_KaleDoubleFold: the incident signature — a
// re-derive re-folded history below the checkpoint, so checkpoint =
// 2×truth. The reconcile must flag it with Delta = +truth.
func TestVerifyRollupDrifts_KaleDoubleFold(t *testing.T) {
	r := &fakeRollupReader{
		checkpoints: []timescale.SEP41RollupCheckpoint{
			{ContractID: "KALE", Fold: kt(2000, 10, 0), LastLedger: 500},
		},
		truth: map[string]timescale.SEP41KindTotals{
			"KALE": kt(1000, 5, 0),
		},
	}
	drifts, checked, err := verifyRollupDrifts(context.Background(), r, nil, big.NewInt(0), time.Minute)
	if err != nil {
		t.Fatalf("verifyRollupDrifts: %v", err)
	}
	if checked != 1 {
		t.Fatalf("checked = %d, want 1", checked)
	}
	if len(drifts) != 2 {
		t.Fatalf("expected 2 drifts (mint+burn), got %+v", drifts)
	}
	if drifts[0].Kind != "mint" || drifts[0].Delta.Cmp(big.NewInt(1000)) != 0 {
		t.Fatalf("mint drift = %+v, want Delta +1000 (2× signature)", drifts[0])
	}
	if drifts[1].Kind != "burn" || drifts[1].Delta.Sign() <= 0 {
		t.Fatalf("burn drift = %+v, want positive over-count", drifts[1])
	}
}

// TestVerifyRollupDrifts_ToleranceAbsorbs: a small in-flight advance
// within -tolerance is not reported.
func TestVerifyRollupDrifts_ToleranceAbsorbs(t *testing.T) {
	r := &fakeRollupReader{
		checkpoints: []timescale.SEP41RollupCheckpoint{
			{ContractID: "C_A", Fold: kt(1005, 0, 0), LastLedger: 100},
		},
		truth: map[string]timescale.SEP41KindTotals{"C_A": kt(1000, 0, 0)},
	}
	if drifts, _, err := verifyRollupDrifts(context.Background(), r, nil, big.NewInt(5), time.Minute); err != nil || len(drifts) != 0 {
		t.Fatalf("drift 5 == tolerance must be absorbed, got drifts=%+v err=%v", drifts, err)
	}
	if drifts, _, err := verifyRollupDrifts(context.Background(), r, nil, big.NewInt(4), time.Minute); err != nil || len(drifts) != 1 {
		t.Fatalf("drift 5 > tolerance 4 must be reported, got drifts=%+v err=%v", drifts, err)
	}
}

// TestVerifyRollupDrifts_ContractsFilterPlumbed: the -contracts subset is
// forwarded to the reader unchanged.
func TestVerifyRollupDrifts_ContractsFilterPlumbed(t *testing.T) {
	r := &fakeRollupReader{
		checkpoints: []timescale.SEP41RollupCheckpoint{
			{ContractID: "C_A", Fold: kt(1, 0, 0), LastLedger: 1},
			{ContractID: "C_B", Fold: kt(2, 0, 0), LastLedger: 2},
		},
		truth: map[string]timescale.SEP41KindTotals{"C_A": kt(1, 0, 0), "C_B": kt(2, 0, 0)},
	}
	_, checked, err := verifyRollupDrifts(context.Background(), r, []string{"C_B"}, big.NewInt(0), time.Minute)
	if err != nil {
		t.Fatalf("verifyRollupDrifts: %v", err)
	}
	if checked != 1 {
		t.Fatalf("checked = %d, want 1 (only C_B)", checked)
	}
	if len(r.gotContracts) != 1 || r.gotContracts[0] != "C_B" {
		t.Fatalf("contracts filter not forwarded: got %v", r.gotContracts)
	}
}

func TestReportRollupDrifts_OKAndDrift(t *testing.T) {
	var okBuf bytes.Buffer
	reportRollupDrifts(&okBuf, nil, 3, big.NewInt(0))
	if got := okBuf.String(); !bytes.Contains([]byte(got), []byte("OK: 3")) {
		t.Fatalf("clean report missing OK line: %q", got)
	}

	var driftBuf bytes.Buffer
	drifts, _, _ := verifyRollupDrifts(context.Background(), &fakeRollupReader{
		checkpoints: []timescale.SEP41RollupCheckpoint{{ContractID: "KALE", Fold: kt(2000, 0, 0), LastLedger: 1}},
		truth:       map[string]timescale.SEP41KindTotals{"KALE": kt(1000, 0, 0)},
	}, nil, big.NewInt(0), time.Minute)
	reportRollupDrifts(&driftBuf, drifts, 1, big.NewInt(0))
	out := driftBuf.String()
	for _, want := range []string{"DRIFT", "KALE", "mint", "1000"} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Fatalf("drift report missing %q:\n%s", want, out)
		}
	}
}

func TestParseContractsCSV(t *testing.T) {
	if got := parseContractsCSV(""); got != nil {
		t.Fatalf("empty → nil, got %v", got)
	}
	if got := parseContractsCSV("  , ,"); got != nil {
		t.Fatalf("all-blank → nil, got %v", got)
	}
	got := parseContractsCSV(" C_A , C_B ,C_C")
	want := []string{"C_A", "C_B", "C_C"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseTolerance(t *testing.T) {
	if v, err := parseTolerance(""); err != nil || v.Sign() != 0 {
		t.Fatalf("empty → 0, got %v err %v", v, err)
	}
	// i128-scale value the KALE delta can reach — must not truncate.
	big128 := "34200000000000000000"
	v, err := parseTolerance(big128)
	if err != nil || v.String() != big128 {
		t.Fatalf("large tolerance mis-parsed: got %v err %v", v, err)
	}
	if _, err := parseTolerance("-1"); err == nil {
		t.Fatal("negative tolerance must error")
	}
	if _, err := parseTolerance("abc"); err == nil {
		t.Fatal("non-numeric tolerance must error")
	}
}
