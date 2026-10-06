package timescale

import (
	"math/big"
	"math/bits"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestSpectraKindSpecsMatchMigration keeps the Go field sets in lockstep
// with migration 0210's per-kind column count, so the validator and the
// CHECK cannot disagree about which columns a kind carries.
func TestSpectraKindSpecsMatchMigration(t *testing.T) {
	body, err := os.ReadFile(findRepoRoot(t) + "/migrations/0210_create_spectra_events.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, countCase, ok := strings.Cut(string(body), "CONSTRAINT spectra_events_kind_column_count")
	if !ok {
		t.Fatal("spectra_events_kind_column_count not found in 0210")
	}
	countCase, _, _ = strings.Cut(countCase, "CONSTRAINT")
	want := map[SpectraEventKind]int{}
	for _, m := range regexp.MustCompile(`WHEN '([a-z_]+)'\s+THEN (\d+)`).FindAllStringSubmatch(countCase, -1) {
		n, _ := strconv.Atoi(m[2])
		want[SpectraEventKind(m[1])] = n
	}
	if len(want) != len(spectraKindSpecs) {
		t.Errorf("migration counts %d kinds, Go specs %d", len(want), len(spectraKindSpecs))
	}
	for kind, spec := range spectraKindSpecs {
		if got := bits.OnesCount16(uint16(spec.fields)); got != want[kind] {
			t.Errorf("%s: Go carries %d fields, migration counts %d", kind, got, want[kind])
		}
	}
}

func amountOf(n int64) *canonical.Amount {
	a := canonical.NewAmount(big.NewInt(n))
	return &a
}

func TestSpectraEventArgs_Validation(t *testing.T) {
	const pt = "CDRK5SWZ7DQJ4BZUAZQABPSP7MZS4NO4PPW7LW6CVD5THZDVE63MVLJJ"
	ok := SpectraEvent{
		ContractID: pt, TxHash: strings.Repeat("a", 64), Kind: SpectraPTMinted, Role: SpectraRolePT,
		MarketPT: pt, Caller: "G1", Receiver: "G2", Shares: amountOf(5),
	}
	if _, err := spectraEventArgs(ok); err != nil {
		t.Fatalf("valid pt_minted refused: %v", err)
	}
	cases := map[string]func(*SpectraEvent){
		"unknown kind":       func(e *SpectraEvent) { e.Kind = "mint" },
		"wrong role":         func(e *SpectraEvent) { e.Role = SpectraRoleIBT },
		"PT names other mkt": func(e *SpectraEvent) { e.MarketPT = "COTHER" },
		"missing receiver":   func(e *SpectraEvent) { e.Receiver = "" },
		"extra maker":        func(e *SpectraEvent) { e.Maker = "G3" },
		"extra amount":       func(e *SpectraEvent) { e.Amount = amountOf(1) },
		"extra duration":     func(e *SpectraEvent) { e.DurationSeconds = 1 },
		"negative shares":    func(e *SpectraEvent) { e.Shares = amountOf(-1) },
		"unset shares":       func(e *SpectraEvent) { e.Shares = nil },
		"uppercase order id": func(e *SpectraEvent) { e.OrderID = strings.Repeat("A", 64) },
		"missing tx hash":    func(e *SpectraEvent) { e.TxHash = "" },
		"duration overflows": func(e *SpectraEvent) {
			e.Kind, e.Role, e.DurationSeconds = SpectraPTDeployed, SpectraRoleFactory, 1<<63
		},
		"order id wrong size": func(e *SpectraEvent) { e.OrderID = "ab" },
	}
	for name, mutate := range cases {
		e := ok
		mutate(&e)
		if _, err := spectraEventArgs(e); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
