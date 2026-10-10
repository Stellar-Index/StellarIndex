package clickhouse

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	chproto "github.com/ClickHouse/ch-go/proto"
	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/column"
	chblock "github.com/ClickHouse/clickhouse-go/v2/lib/proto"
)

// fakeCHServer answers the three queries openSink issues (the HTTP-protocol
// hello, Ping's SELECT 1, and the schema check) with Native-format blocks
// encoded by the driver's own encoder, so the test exercises the real
// clickhouse-go decode and Scan path rather than a hand-rolled driver.Conn.
type fakeCHServer struct {
	tables []string // rows returned for the stellar database's system.tables
	// noColumns holds "table.column" entries withheld from system.columns.
	noColumns []string

	mu      sync.Mutex
	queries []string
}

func (f *fakeCHServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	q := strings.TrimSpace(string(body))
	f.mu.Lock()
	f.queries = append(f.queries, q)
	f.mu.Unlock()

	var (
		names []string
		types []column.Type
		rows  [][]any
	)
	switch {
	case strings.Contains(q, "displayName()"):
		names = []string{"displayName()", "version()", "revision()", "timezone()"}
		types = []column.Type{"String", "String", "UInt32", "String"}
		rows = [][]any{{"fake", "24.8.1.1", uint32(clickhouse.ClientTCPProtocolVersion), "UTC"}}
	case q == "SELECT 1":
		names, types, rows = []string{"1"}, []column.Type{"UInt8"}, [][]any{{uint8(1)}}
	case strings.Contains(q, "system.tables"):
		names, types = []string{"name"}, []column.Type{"String"}
		for _, name := range f.tables {
			rows = append(rows, []any{name})
		}
	case strings.Contains(q, "system.columns"):
		names, types = []string{"name"}, []column.Type{"String"}
		skip := make(map[string]bool, len(f.noColumns))
		for _, c := range f.noColumns {
			skip[c] = true
		}
		for table, cols := range sinkColumns {
			for _, c := range cols {
				if !skip[table+"."+c] {
					rows = append(rows, []any{table + "." + c})
				}
			}
		}
	default:
		http.Error(w, "fakeCHServer: unexpected query "+q, http.StatusInternalServerError)
		return
	}
	buf, err := encodeNative(names, types, rows)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(buf)
}

func (f *fakeCHServer) sawQuery(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, q := range f.queries {
		if strings.Contains(q, substr) {
			return true
		}
	}
	return false
}

// encodeNative encodes one Native-format block at the revision the fake
// advertises in its hello.
func encodeNative(names []string, types []column.Type, rows [][]any) ([]byte, error) {
	b := chblock.NewBlock()
	for i, name := range names {
		if err := b.AddColumn(name, types[i]); err != nil {
			return nil, err
		}
	}
	for _, row := range rows {
		if err := b.Append(row...); err != nil {
			return nil, err
		}
	}
	buf := new(chproto.Buffer)
	if err := b.Encode(buf, clickhouse.ClientTCPProtocolVersion); err != nil {
		return nil, err
	}
	return buf.Buf, nil
}

func openAgainstFake(t *testing.T, tables []string, noColumns ...string) (*Sink, *fakeCHServer, error) {
	t.Helper()
	fake := &fakeCHServer{tables: tables, noColumns: noColumns}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := openSink(ctx, &clickhouse.Options{
		Addr:        []string{srv.Listener.Addr().String()},
		Protocol:    clickhouse.HTTP,
		Auth:        clickhouse.Auth{Database: "stellar"},
		DialTimeout: 5 * time.Second,
	}, 10)
	if s != nil {
		t.Cleanup(func() { _ = s.Close(context.Background()) })
	}
	return s, fake, err
}

// TestOpenSinkAcceptsMigratedDatabase: a database holding every table Flush
// writes to (plus unrelated ones) opens cleanly, and the schema check really
// ran through the driver.
func TestOpenSinkAcceptsMigratedDatabase(t *testing.T) {
	tables := append([]string{"tx_hash_index", "ledger_entries_current"}, sinkTables...)
	s, fake, err := openAgainstFake(t, tables)
	if err != nil {
		t.Fatalf("openSink against a fully migrated database: %v", err)
	}
	if s == nil {
		t.Fatal("openSink returned nil Sink with nil error")
	}
	if !fake.sawQuery("system.tables") {
		t.Fatal("openSink never queried system.tables; the schema check is not wired into Open")
	}
}

// TestOpenSinkRefusesIncompleteSchema: Ping succeeds against an un-migrated
// endpoint, so Open must refuse a missing table or additive-ALTER column by
// name instead of returning a Sink whose first Flush (or INSERT) fails.
func TestOpenSinkRefusesIncompleteSchema(t *testing.T) {
	var unmigrated []string
	for _, name := range sinkTables {
		if name != "supply_flows" && name != "ledgers" {
			unmigrated = append(unmigrated, name)
		}
	}
	cases := []struct {
		name      string
		tables    []string
		noColumns []string
		want      []string
		present   string
	}{
		{
			"missing tables", unmigrated, nil,
			[]string{"stellar.supply_flows", "stellar.ledgers"},
			"stellar.transactions",
		},
		{
			"missing operator columns", sinkTables,
			[]string{"transactions.fee_bump_fee", "ledger_entry_changes.intra_ledger_seq"},
			[]string{"stellar.transactions.fee_bump_fee", "stellar.ledger_entry_changes.intra_ledger_seq"},
			"stellar.transactions.fee_account",
		},
		{"empty database", nil, nil, []string{"stellar.ledgers"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, err := openAgainstFake(t, tc.tables, tc.noColumns...)
			if err == nil {
				t.Fatalf("openSink against a database with %s: want error, got nil", tc.name)
			}
			if s != nil {
				t.Fatal("openSink returned a Sink alongside its error")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name missing %s", err, want)
				}
			}
			if tc.present != "" && strings.Contains(err.Error(), tc.present) {
				t.Errorf("error %q names %s, which is present", err, tc.present)
			}
		})
	}
}

// TestSinkColumnsMatchFlushInserts keeps sinkColumns in step with the INSERT
// column lists for the tables it covers.
func TestSinkColumnsMatchFlushInserts(t *testing.T) {
	conn := &fakeOrderConn{}
	s := &Sink{conn: conn}
	s.ledgers = []LedgerRow{{LedgerSeq: 1}}
	s.txs = []TransactionRow{{}}
	s.changes = []LedgerEntryChangeRow{{}}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	for table, cols := range sinkColumns {
		var stmt string
		for _, q := range conn.queries {
			if strings.HasPrefix(q, "INSERT INTO stellar."+table+" ") {
				stmt = q
			}
		}
		if stmt == "" {
			t.Fatalf("no INSERT captured for stellar.%s", table)
		}
		for _, c := range cols {
			if !strings.Contains(stmt, " "+c) && !strings.Contains(stmt, "("+c) {
				t.Errorf("sinkColumns lists %s.%s, absent from its INSERT", table, c)
			}
		}
	}
}

// TestSinkTablesMatchFlushTargets keeps the schema check's table list in
// lockstep with the tables Flush actually inserts into, so a new flush target
// cannot be added without Open also requiring it.
func TestSinkTablesMatchFlushTargets(t *testing.T) {
	conn := &fakeOrderConn{}
	s := &Sink{conn: conn}
	s.ledgers = []LedgerRow{{LedgerSeq: 1}}
	s.txs = []TransactionRow{{}}
	s.ops = []OperationRow{{}}
	s.results = []OperationResultRow{{}}
	s.participants = []OperationParticipantRow{{}}
	s.events = []ContractEventRow{{}}
	s.changes = []LedgerEntryChangeRow{{}}
	s.supplyFlows = []SupplyFlowRow{{}}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	want := make(map[string]bool, len(sinkTables))
	for _, name := range sinkTables {
		want[name] = true
	}
	got := make(map[string]bool, len(conn.tables))
	for _, name := range conn.tables {
		got[name] = true
		if !want[name] {
			t.Errorf("Flush inserts into stellar.%s, which sinkTables (the Open schema check) omits", name)
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("sinkTables requires stellar.%s, which Flush never inserts into", name)
		}
	}
}
