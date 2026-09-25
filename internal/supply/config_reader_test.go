package supply

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

// newDatedConfigReader builds a reader whose snapshot is dated today,
// well inside its max age, for tests about the balance arithmetic.
func newDatedConfigReader(m map[string]string) (*ConfigReserveBalanceReader, error) {
	return NewConfigReserveBalanceReader(m, time.Now(), 7*24*time.Hour)
}

// The static map is a hand-entered point-in-time snapshot: undated or
// past its max age it must refuse, never re-stamp the old balances as
// the current reserve.
func TestConfigReserveBalanceReader_RefusesUndatedOrExpiredSnapshot(t *testing.T) {
	asOf := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	balances := map[string]string{"GA1": "100"}
	cases := []struct {
		name    string
		asOf    time.Time
		now     time.Time
		wantErr bool
	}{
		{"undated", time.Time{}, asOf, true},
		{"85 days old", asOf, asOf.Add(85 * 24 * time.Hour), true},
		{"one hour past max age", asOf, asOf.Add(7*24*time.Hour + time.Hour), true},
		{"inside max age", asOf, asOf.Add(6 * 24 * time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewConfigReserveBalanceReader(balances, tc.asOf, 7*24*time.Hour)
			if err != nil {
				t.Fatalf("construct: %v", err)
			}
			r.now = func() time.Time { return tc.now }
			got, err := r.ReserveBalanceTotal(context.Background(), []string{"GA1"}, 1)
			if tc.wantErr {
				if !errors.Is(err, ErrStaticReserveSnapshotExpired) {
					t.Fatalf("err = %v, want ErrStaticReserveSnapshotExpired (got total %v)", err, got)
				}
				return
			}
			if err != nil || got.Cmp(big.NewInt(100)) != 0 {
				t.Fatalf("got (%v, %v), want (100, nil)", got, err)
			}
		})
	}
}

func TestConfigReserveBalanceReader_RejectsNonPositiveMaxAge(t *testing.T) {
	if _, err := NewConfigReserveBalanceReader(nil, time.Now(), 0); err == nil {
		t.Fatal("max age 0 accepted; want a construction error")
	}
}

func TestConfigReserveBalanceReader_HappyPath(t *testing.T) {
	r, err := newDatedConfigReader(map[string]string{
		"GA1": "100",
		"GA2": "200",
	})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	got, err := r.ReserveBalanceTotal(context.Background(), []string{"GA1", "GA2"}, 0)
	if err != nil {
		t.Fatalf("ReserveBalanceTotal: %v", err)
	}
	if got.Cmp(big.NewInt(300)) != 0 {
		t.Errorf("total=%s want 300", got.String())
	}
}

func TestConfigReserveBalanceReader_LargeStroops(t *testing.T) {
	// 5e9 XLM × 1e7 stroops/XLM = 5e16, well above int64 range.
	want, _ := new(big.Int).SetString("50000000000000000", 10)
	r, err := newDatedConfigReader(map[string]string{
		"GA1": "50000000000000000",
	})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	got, err := r.ReserveBalanceTotal(context.Background(), []string{"GA1"}, 0)
	if err != nil {
		t.Fatalf("ReserveBalanceTotal: %v", err)
	}
	if got.Cmp(want) != 0 {
		t.Errorf("total=%s want %s", got, want)
	}
}

func TestConfigReserveBalanceReader_RejectsEmptyKey(t *testing.T) {
	_, err := newDatedConfigReader(map[string]string{
		"": "100",
	})
	if err == nil {
		t.Fatal("want error on empty account key")
	}
}

func TestConfigReserveBalanceReader_RejectsMalformed(t *testing.T) {
	_, err := newDatedConfigReader(map[string]string{
		"GA1": "not-a-number",
	})
	if err == nil {
		t.Fatal("want error on non-decimal balance")
	}
	if !strings.Contains(err.Error(), "GA1") {
		t.Errorf("err=%v should mention the failing account", err)
	}
}

func TestConfigReserveBalanceReader_RejectsNegative(t *testing.T) {
	_, err := newDatedConfigReader(map[string]string{
		"GA1": "-100",
	})
	if err == nil {
		t.Fatal("want error on negative balance")
	}
}

// TestConfigReserveBalanceReader_MissingAccountErrors — silently
// treating an unknown account as zero would yield an over-stated
// circulating supply (the exact failure mode ADR-0011 prohibits).
// The reader must error.
func TestConfigReserveBalanceReader_MissingAccountErrors(t *testing.T) {
	r, err := newDatedConfigReader(map[string]string{
		"GA1": "100",
	})
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	_, err = r.ReserveBalanceTotal(context.Background(), []string{"GA1", "GA_UNKNOWN"}, 0)
	if err == nil {
		t.Fatal("want error when account missing from config")
	}
	if !strings.Contains(err.Error(), "GA_UNKNOWN") {
		t.Errorf("err=%v should name the missing account", err)
	}
}

// TestConfigReserveBalanceReader_EmptyConstructionAllowed — an empty
// balance map is a legal config (operator hasn't enumerated any
// reserves yet). The reader returns 0 for any empty account list.
func TestConfigReserveBalanceReader_EmptyConstructionAllowed(t *testing.T) {
	r, err := newDatedConfigReader(map[string]string{})
	if err != nil {
		t.Fatalf("construct empty: %v", err)
	}
	got, err := r.ReserveBalanceTotal(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("ReserveBalanceTotal nil accounts: %v", err)
	}
	if got.Sign() != 0 {
		t.Errorf("empty config + empty accounts = %s, want 0", got)
	}
}
