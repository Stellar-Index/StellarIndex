package clickhouse

import (
	"context"
	"errors"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

type absentTableConn struct{ driver.Conn }

func (absentTableConn) Query(context.Context, string, ...any) (driver.Rows, error) {
	return nil, &clickhouse.Exception{Code: 60, Name: "UNKNOWN_TABLE", Message: "Table stellar.account_entry_ledgers doesn't exist"}
}

// An unbuilt index reads as "no entry changes"; the counter is what tells an operator the empty page is not true.
func TestAccountEntryLedgers_AbsentTableCountsEmptyRead(t *testing.T) {
	c := obs.ExplorerSchemaAbsentEmptyTotal.WithLabelValues("account_entry_ledgers")
	before := testutil.ToFloat64(c)
	r := &ExplorerReader{conn: absentTableConn{}}
	got, err := r.AccountEntryLedgers(context.Background(), "k", []string{"owner"}, 100, 10)
	if err != nil || got != nil {
		t.Fatalf("AccountEntryLedgers = %v, %v; want nil, nil", got, err)
	}
	if d := testutil.ToFloat64(c) - before; d != 1 {
		t.Fatalf("schema-absent empty counter moved by %v, want 1", d)
	}
}

func TestAbsentReadsEmpty_IgnoresOtherErrors(t *testing.T) {
	c := obs.ExplorerSchemaAbsentEmptyTotal.WithLabelValues("sponsorship_events")
	before := testutil.ToFloat64(c)
	if absentReadsEmpty("sponsorship_events", errors.New("timeout")) {
		t.Fatal("a transport error must not read as an absent table")
	}
	if testutil.ToFloat64(c) != before {
		t.Fatal("counter moved on a non-schema error")
	}
}
