package clickhouse

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// emptyProbeConn answers every probe with an existing-but-empty object.
type emptyProbeConn struct{ driver.Conn }

func (emptyProbeConn) Query(context.Context, string, ...any) (driver.Rows, error) {
	return &probeRows{consumed: true}, nil
}

// TestProbeSchema_LatchedAbsenceIsExported pins T403: an API that starts
// before the lake DDL latches the probe false for its whole lifetime and
// serves the slow fallback — the gauge is the only place that shows.
func TestProbeSchema_LatchedAbsenceIsExported(t *testing.T) {
	conn := &probeConn{results: []error{
		&clickhouse.Exception{Code: 60, Name: "UNKNOWN_TABLE", Message: "Table stellar.tx_hash_index does not exist"},
	}}
	gauge := obs.CHSchemaProbePresent.WithLabelValues("tx_hash_index")
	gauge.Set(-1) // a fresh gauge reads 0 already; -1 proves the probe wrote it
	r := newExplorerReader(conn)
	ctx := context.Background()

	if r.txHashIndexAvailable(ctx) || r.txHashIndexAvailable(ctx) {
		t.Fatal("tx_hash_index reported available after UNKNOWN_TABLE")
	}
	if conn.calls != 1 {
		t.Fatalf("conn.Query called %d times, want 1 (absence latches)", conn.calls)
	}
	if got := testutil.ToFloat64(gauge); got != 0 {
		t.Errorf("stellarindex_ch_schema_probe_present{probe=tx_hash_index} = %v, want 0", got)
	}
}

// TestProbeSchema_PresentAndEmptyAreExported — the gauge follows every
// ANSWER, including a row-requiring probe revoked by an emptied table.
func TestProbeSchema_PresentAndEmptyAreExported(t *testing.T) {
	gauge := obs.CHSchemaProbePresent.WithLabelValues("contracts_census_daily")
	gauge.Set(-1)
	r := newExplorerReader(&probeConn{})
	if !r.censusAvailable(context.Background()) {
		t.Fatal("census probe = false on a table with rows")
	}
	if got := testutil.ToFloat64(gauge); got != 1 {
		t.Fatalf("present gauge after a row = %v, want 1", got)
	}

	r = newExplorerReader(emptyProbeConn{})
	if r.censusAvailable(context.Background()) {
		t.Fatal("census probe = true on an empty table")
	}
	if got := testutil.ToFloat64(gauge); got != 0 {
		t.Errorf("present gauge after an empty answer = %v, want 0", got)
	}
}

// TestProbeSchema_NonAnswerIsCountedNotExported — a transport failure says
// nothing about the object: it must not move the gauge, and must count.
func TestProbeSchema_NonAnswerIsCountedNotExported(t *testing.T) {
	const probe = "ledger_entries_current_version"
	gauge := obs.CHSchemaProbePresent.WithLabelValues(probe)
	gauge.Set(1)
	unanswered := obs.CHSchemaProbeUnansweredTotal.WithLabelValues(probe)
	before := testutil.ToFloat64(unanswered)

	r := newExplorerReader(&probeConn{results: []error{
		&net.OpError{Op: "read", Err: errors.New("connection reset by peer")},
	}})
	if r.ledgerEntriesVersioned(context.Background()) {
		t.Fatal("probe = true with no answer and no prior verdict")
	}
	if got := testutil.ToFloat64(unanswered) - before; got != 1 {
		t.Errorf("unanswered counter delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(gauge); got != 1 {
		t.Errorf("present gauge moved on a non-answer: %v, want 1", got)
	}
}

// TestNewExplorerReader_EverySchemaProbeIsNamed — an unnamed probe would
// export under probe="" and collide with every other unnamed one.
func TestNewExplorerReader_EverySchemaProbeIsNamed(t *testing.T) {
	v := reflect.ValueOf(newExplorerReader(nil)).Elem()
	probeType := reflect.TypeOf(schemaProbe{})
	seen := map[string]string{}
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if f.Type != probeType {
			continue
		}
		name := v.Field(i).FieldByName("name").String()
		if name == "" {
			t.Errorf("schemaProbe field %s has no name", f.Name)
			continue
		}
		if other, dup := seen[name]; dup {
			t.Errorf("schemaProbe fields %s and %s share name %q", other, f.Name, name)
		}
		seen[name] = f.Name
	}
	if len(seen) == 0 {
		t.Fatal("found no schemaProbe fields — the reflection walk is broken")
	}
}
