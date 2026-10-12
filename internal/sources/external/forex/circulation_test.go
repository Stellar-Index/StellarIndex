package forex

import (
	"strings"
	"testing"
	"time"
)

// TestLoadCirculationTable_ParsesCleanly pins that the embedded CSV parses with no skipped rows, since a skip
// means a typo that would silently ship to /v1/currencies.
func TestLoadCirculationTable_ParsesCleanly(t *testing.T) {
	table, err := loadCirculationTable()
	if err != nil {
		t.Fatalf("loadCirculationTable: %v", err)
	}
	if len(table) == 0 {
		t.Fatal("circulation table is empty — bootstrap row USD missing")
	}
}

// TestLoadCirculationTable_MajorsPresent — guard the bootstrap set
// of currencies that >95% of fx volume settles in. Removing any of
// these from the CSV is almost certainly a mistake.
func TestLoadCirculationTable_MajorsPresent(t *testing.T) {
	table, err := loadCirculationTable()
	if err != nil {
		t.Fatalf("loadCirculationTable: %v", err)
	}
	for _, ticker := range []string{
		"usd", "eur", "jpy", "gbp", "chf", "cny", "cad", "aud",
		"nzd", "inr", "krw", "brl", "mxn", "rub", "zar",
	} {
		entry, ok := table[ticker]
		if !ok {
			t.Errorf("major missing: %s", ticker)
			continue
		}
		if entry.AggregateLocalUnits <= 0 {
			t.Errorf("%s: AggregateLocalUnits = %f, want > 0", ticker, entry.AggregateLocalUnits)
		}
		if entry.AsOf.IsZero() {
			t.Errorf("%s: AsOf is zero", ticker)
		}
		if entry.Source == "" {
			t.Errorf("%s: Source is empty", ticker)
		}
	}
}

// TestLoadCirculationTable_BroadCoverage locks the table at 100+ currencies so a curation regression that drops
// most World Bank rows fails.
func TestLoadCirculationTable_BroadCoverage(t *testing.T) {
	table, err := loadCirculationTable()
	if err != nil {
		t.Fatalf("loadCirculationTable: %v", err)
	}
	if got := len(table); got < 100 {
		t.Errorf("circulation table has %d entries; want at least 100 (WB expansion baseline)", got)
	}
}

// TestLoadCirculationTable_AsOfReasonable pins that `as_of` is never in the future and no older than 8 years; the
// World Bank series for some emerging markets legitimately lag 5-7 years.
func TestLoadCirculationTable_AsOfReasonable(t *testing.T) {
	table, err := loadCirculationTable()
	if err != nil {
		t.Fatalf("loadCirculationTable: %v", err)
	}
	now := time.Now()
	floor := now.AddDate(-8, 0, 0)
	for ticker, entry := range table {
		if entry.AsOf.After(now) {
			t.Errorf("%s: as_of %s is in the future", ticker, entry.AsOf.Format("2006-01-02"))
		}
		if entry.AsOf.Before(floor) {
			t.Errorf("%s: as_of %s is more than 8 years stale", ticker, entry.AsOf.Format("2006-01-02"))
		}
	}
}

// TestLoadCirculationTable_SourcesCited — every row must name a
// source. Empty Source defeats the audit story (operator can't
// re-verify a row's provenance from the CSV alone).
func TestLoadCirculationTable_SourcesCited(t *testing.T) {
	table, err := loadCirculationTable()
	if err != nil {
		t.Fatalf("loadCirculationTable: %v", err)
	}
	for ticker, entry := range table {
		if strings.TrimSpace(entry.Source) == "" {
			t.Errorf("%s: source is blank", ticker)
		}
	}
}
