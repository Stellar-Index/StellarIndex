package clickhouse

import (
	"context"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

type scalarWMConn struct {
	driver.Conn
	wm  uint32
	err error
}

func (c scalarWMConn) QueryRow(context.Context, string, ...any) driver.Row {
	if c.err != nil {
		return &stubRow{err: c.err}
	}
	return &stubRow{data: []any{c.wm}}
}

func TestEntryWatermarkOn_ExportsWatermark(t *testing.T) {
	if _, err := entryWatermarkOn(context.Background(), scalarWMConn{wm: 65_070_430}, "entry_index"); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(obs.DerivedIndexWatermarkLedger.WithLabelValues("entry_index")); got != 65_070_430 {
		t.Fatalf("exported watermark = %v, want 65070430", got)
	}
}

// A missing watermark table exports 0 so the alert pages rather than the gauge going quiet.
func TestEntryWatermarkOn_AbsentTableExportsZero(t *testing.T) {
	obs.DerivedIndexWatermarkLedger.WithLabelValues("sponsorship_events").Set(1)
	absent := &clickhouse.Exception{Code: 60, Name: "UNKNOWN_TABLE", Message: "Table stellar.entry_history_watermark doesn't exist"}
	if _, err := entryWatermarkOn(context.Background(), scalarWMConn{err: absent}, "sponsorship_events"); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(obs.DerivedIndexWatermarkLedger.WithLabelValues("sponsorship_events")); got != 0 {
		t.Fatalf("exported watermark = %v, want 0", got)
	}
}

func TestGenesisWatermark_ExportsWatermark(t *testing.T) {
	r := &ExplorerReader{conn: scalarWMConn{wm: 64_825_049}}
	if _, err := r.genesisWatermark(context.Background(), ContractInstanceChangesTable); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(obs.DerivedIndexWatermarkLedger.WithLabelValues(ContractInstanceChangesTable)); got != 64_825_049 {
		t.Fatalf("exported watermark = %v, want 64825049", got)
	}
}
