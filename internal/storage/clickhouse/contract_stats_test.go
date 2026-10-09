package clickhouse

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/strkey"
)

type contractStatsConn struct {
	stubConn
	watermark uint32
}

func (c *contractStatsConn) QueryRow(_ context.Context, query string, _ ...any) driver.Row {
	switch {
	case strings.Contains(query, "entry_history_watermark"):
		return &stubRow{data: []any{c.watermark}}
	case strings.Contains(query, "contracts_census_daily"):
		return &stubRow{data: []any{int64(42)}}
	}
	return &stubRow{data: nil}
}

func newContractStatsConn(t *testing.T, watermark uint32, census bool) *contractStatsConn {
	t.Helper()
	m := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	c := &contractStatsConn{watermark: watermark}
	c.respond = func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "contracts_census_daily LIMIT 1"):
			if !census {
				return &stubRows{}, nil
			}
			return &stubRows{data: [][]any{{m}}}, nil
		case strings.Contains(q, "toStartOfMonth"):
			return &stubRows{data: [][]any{{m, int64(3), int64(5)}, {m.AddDate(0, 1, 0), int64(0), int64(7)}}}, nil
		}
		t.Fatalf("unexpected query: %s", q)
		return nil, nil
	}
	return c
}

func TestContractStats_CompleteHistory(t *testing.T) {
	r := &ExplorerReader{conn: newContractStatsConn(t, 62_000_000, true)}
	got, err := r.ContractStats(context.Background())
	if err != nil {
		t.Fatalf("ContractStats: %v", err)
	}
	if len(got.Deployments) != 2 || got.Deployments[0].SAC != 3 || got.Deployments[1].Wasm != 7 {
		t.Fatalf("deployments = %+v", got.Deployments)
	}
	if got.Active90d == nil || *got.Active90d != 42 {
		t.Fatalf("active90d = %v, want 42", got.Active90d)
	}
	if !got.HistoryComplete || got.ThruLedger != 62_000_000 {
		t.Fatalf("history = %v/%d, want complete at 62000000", got.HistoryComplete, got.ThruLedger)
	}
	if q := r.conn.(*contractStatsConn).queries[0]; !strings.Contains(q, "max_execution_time = 120") {
		t.Fatalf("deployments query lacks the execution cap: %s", q)
	}
}

// Without a census or a genesis watermark the totals are partial and the active count unknown.
func TestContractStats_PartialIsNotZero(t *testing.T) {
	r := &ExplorerReader{conn: newContractStatsConn(t, 0, false)}
	got, err := r.ContractStats(context.Background())
	if err != nil {
		t.Fatalf("ContractStats: %v", err)
	}
	if got.Active90d != nil {
		t.Fatalf("active90d = %d, want nil when census unavailable", *got.Active90d)
	}
	if got.HistoryComplete {
		t.Fatal("HistoryComplete = true without a watermark")
	}
}

func TestContractTypes(t *testing.T) {
	var raw [32]byte
	raw[0] = 7
	id := strkey.MustEncode(strkey.VersionByteContract, raw[:])
	h := hex.EncodeToString(raw[:])
	conn := &stubConn{}
	conn.respond = func(string) (driver.Rows, error) {
		return &stubRows{data: [][]any{{h, uint8(1)}}}, nil
	}
	r := &ExplorerReader{conn: conn}
	got, err := r.ContractTypes(context.Background(), []string{id, "not-a-contract"})
	if err != nil {
		t.Fatalf("ContractTypes: %v", err)
	}
	if sac, ok := got[id]; !ok || !sac || len(got) != 1 {
		t.Fatalf("types = %v, want {%s: true}", got, id)
	}
	if hs, _ := conn.args[0][0].([]string); len(hs) != 1 || hs[0] != h {
		t.Fatalf("bound ids = %v, want [%s]", conn.args[0], h)
	}
}
