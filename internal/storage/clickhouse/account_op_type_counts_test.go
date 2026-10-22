package clickhouse

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

const mL = opTypePartitionWidth

func TestPlanOpTypeCounts(t *testing.T) {
	src := map[uint32]uint64{58: 100, 59: 200, 60: 300, 61: 50}
	full := func(p uint32, n uint64) opTypeBuilt { return opTypeBuilt{through: opTypePartitionEnd(p), srcRows: n} }
	cases := []struct {
		name  string
		built map[uint32]opTypeBuilt
		want  opTypeCountsPlan
		ok    bool
	}{
		{name: "nothing built", built: map[uint32]opTypeBuilt{}},
		{
			name:  "all closed valid, open head served below its mark without a row match",
			built: map[uint32]opTypeBuilt{58: full(58, 100), 59: full(59, 200), 60: full(60, 300), 61: {through: 61*mL + 5000, srcRows: 1}},
			want:  opTypeCountsPlan{valid: []uint32{58, 59, 60, 61}, tailFrom: 61*mL + 5000},
			ok:    true,
		},
		{
			name:  "moved fingerprint, missing and partial closed partitions are read from source",
			built: map[uint32]opTypeBuilt{59: full(59, 199), 60: {through: 60*mL + 10}, 61: {through: 61*mL + 5000}},
			want:  opTypeCountsPlan{valid: []uint32{61}, stale: []uint32{58, 59, 60}, tailFrom: 61*mL + 5000},
			ok:    true,
		},
		{
			name:  "complete head whose fingerprint moved is stale; tail starts after it",
			built: map[uint32]opTypeBuilt{59: full(59, 200), 60: full(60, 299)},
			want:  opTypeCountsPlan{valid: []uint32{59}, stale: []uint32{58, 60}, tailFrom: 61 * mL},
			ok:    true,
		},
		{
			name:  "only an empty head is no plan",
			built: map[uint32]opTypeBuilt{61: {through: 61 * mL}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := planOpTypeCounts(tc.built, src)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (plan %+v)", ok, tc.ok, got)
			}
			if !ok {
				return
			}
			if !slices.Equal(got.valid, tc.want.valid) || !slices.Equal(got.stale, tc.want.stale) || got.tailFrom != tc.want.tailFrom {
				t.Fatalf("plan = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Every source ledger must be counted by exactly one arm: valid partitions from the rollup, the rest from account_op_types.
func TestPlanOpTypeCounts_ArmsPartitionTheLedgers(t *testing.T) {
	src := map[uint32]uint64{0: 1, 1: 2, 2: 3, 3: 4}
	built := map[uint32]opTypeBuilt{0: {through: mL, srcRows: 1}, 1: {through: 2 * mL, srcRows: 9}, 3: {through: 3*mL + 77}}
	plan, ok := planOpTypeCounts(built, src)
	if !ok {
		t.Fatal("no plan")
	}
	for _, l := range []uint32{0, mL - 1, mL, 2*mL - 1, 2 * mL, 3 * mL, 3*mL + 76, 3*mL + 77, 9 * mL} {
		rollup := slices.Contains(plan.valid, l/mL) && (l/mL != 3 || l < plan.tailFrom)
		source := l >= plan.tailFrom || slices.Contains(plan.stale, l/mL)
		if rollup == source {
			t.Errorf("ledger %d: rollup=%v source=%v, want exactly one", l, rollup, source)
		}
	}
}

func TestOpTypeRollupWork(t *testing.T) {
	src := map[uint32]uint64{57: 1, 58: 100, 59: 200, 60: 300, 61: 50}
	built := map[uint32]opTypeBuilt{
		58: {through: opTypePartitionEnd(58), srcRows: 100},
		59: {through: 59*mL + 3, srcRows: 200},
		60: {through: opTypePartitionEnd(60), srcRows: 299},
		61: {through: 61*mL + 100},
	}
	parts := func(w []OpTypeRollupPartition) []uint32 {
		var out []uint32
		for _, x := range w {
			out = append(out, x.Part)
		}
		return out
	}
	tip := uint32(61*mL + 5000)
	w := opTypeRollupWork(built, src, 61, tip, 0)
	if got := parts(w); !slices.Equal(got, []uint32{60, 59, 57, 61}) {
		t.Fatalf("parts = %v, want newest-first closed then the open head", got)
	}
	if last := w[len(w)-1]; last.Through != tip+1-opTypeRollupTipMargin {
		t.Errorf("open through = %d, want tip+1-margin", last.Through)
	}
	if got := parts(opTypeRollupWork(built, src, 61, tip, 1)); !slices.Equal(got, []uint32{60, 61}) {
		t.Errorf("maxClosed 1: parts = %v, want [60 61]", got)
	}
	if got := parts(opTypeRollupWork(built, src, 61, 61*mL+opTypeRollupTipMargin-1, 1)); !slices.Equal(got, []uint32{60}) {
		t.Errorf("tip inside the margin: parts = %v, want the open head skipped", got)
	}
	if got := parts(opTypeRollupWork(built, src, 61, 61*mL+opTypeRollupTipMargin+99, 1)); !slices.Equal(got, []uint32{60}) {
		t.Errorf("open head already built that far: parts = %v, want it skipped", got)
	}
}

func TestOpTypeCountsRollupQuery(t *testing.T) {
	q := opTypeCountsRollupQuery(opTypeCountsPlan{valid: []uint32{58, 61}, stale: []uint32{59}, tailFrom: 61*mL + 5000})
	for _, want := range []string{
		"part IN (58, 61)",
		"ledger_seq >= 61005000 OR (ledger_seq >= 59000000 AND ledger_seq < 60000000)",
		"do_not_merge_across_partitions_select_final = 1",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %q:\n%s", want, q)
		}
	}
	if n := strings.Count(q, "?"); n != 2 {
		t.Errorf("query binds %d params, want 2 (account per arm)", n)
	}
}

// execStubConn adds Exec recording to stubConn for the build path.
type execStubConn struct {
	stubConn
	execs [][2]any
}

func (c *execStubConn) Exec(_ context.Context, query string, args ...any) error {
	c.execs = append(c.execs, [2]any{query, args})
	return nil
}

func (c *execStubConn) execd(sub string) []int {
	var out []int
	for i, e := range c.execs {
		if strings.Contains(e[0].(string), sub) {
			out = append(out, i)
		}
	}
	return out
}

func buildStub(newOps, liveOps uint64) *execStubConn {
	return &execStubConn{stubConn: stubConn{respond: func(q string) (driver.Rows, error) {
		if strings.Contains(q, "system.parts") {
			return &stubRows{data: [][]any{{uint32(60), uint64(785)}}}, nil
		}
		return &stubRows{data: [][]any{{newOps, liveOps}}}, nil
	}}}
}

func TestRunOpTypeCountsPartition_SwapsAndRecordsFingerprint(t *testing.T) {
	conn := buildStub(10, 9)
	w := OpTypeRollupPartition{Part: 60, Through: opTypePartitionEnd(60), Reason: "test"}
	if err := OpTypeCountsRollup.runPartitionConn(context.Background(), conn, w, false, t.Logf); err != nil {
		t.Fatal(err)
	}
	sentinel := conn.execd("VALUES (?, '', '', ?, ?)")
	if len(sentinel) != 1 {
		t.Fatalf("sentinel inserts = %d, want 1", len(sentinel))
	}
	if args := conn.execs[sentinel[0]][1].([]any); args[1] != uint64(785) || args[2] != w.Through {
		t.Errorf("sentinel args = %v, want fingerprint 785 and through %d", args, w.Through)
	}
	replace := conn.execd("REPLACE PARTITION ID '60'")
	drop := conn.execd("DROP TABLE IF EXISTS")
	if len(replace) != 1 || len(drop) != 1 || drop[0] < replace[0] || replace[0] < sentinel[0] {
		t.Fatalf("exec order wrong: %v", conn.execs)
	}
}

func TestRunOpTypeCountsPartition_ShrinkRefusesReplace(t *testing.T) {
	conn := buildStub(8, 9)
	w := OpTypeRollupPartition{Part: 60, Through: opTypePartitionEnd(60)}
	err := OpTypeCountsRollup.runPartitionConn(context.Background(), conn, w, false, t.Logf)
	if !errors.Is(err, ErrCountsShrink) {
		t.Fatalf("err = %v, want ErrCountsShrink", err)
	}
	if n := len(conn.execd("REPLACE PARTITION")); n != 0 {
		t.Errorf("replaced %d time(s) after a shrink", n)
	}
	if n := len(conn.execd("DROP TABLE IF EXISTS")); n != 1 {
		t.Errorf("staging drops = %d, want 1", n)
	}
	conn = buildStub(8, 9)
	if err := OpTypeCountsRollup.runPartitionConn(context.Background(), conn, w, true, t.Logf); err != nil {
		t.Fatalf("shrinkOK: %v", err)
	}
}

func TestAccountOperationTypeCounts_ServesRollupWhenPlanned(t *testing.T) {
	conn := &stubConn{respond: func(q string) (driver.Rows, error) {
		switch {
		case isOpTypesCoverageProbe(q):
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		case strings.Contains(q, "WHERE account = ''"):
			return &stubRows{data: [][]any{{uint32(0), uint32(mL), uint64(5)}, {uint32(1), uint32(mL + 9), uint64(0)}}}, nil
		case strings.Contains(q, "system.parts"):
			return &stubRows{data: [][]any{{uint32(0), uint64(5)}, {uint32(1), uint64(3)}}}, nil
		}
		return &stubRows{}, nil
	}}
	r := &ExplorerReader{conn: conn, opTypesCoverageProbe: schemaProbe{now: newFakeClock().now}}
	if _, err := r.AccountOperationTypeCounts(context.Background(), "GTESTOPTYPES"); err != nil {
		t.Fatal(err)
	}
	last := conn.queries[len(conn.queries)-1]
	if !strings.Contains(last, "stellar.account_op_type_counts") || !strings.Contains(last, "part IN (0, 1)") {
		t.Fatalf("ran %q, want the rollup query over parts 0,1", last)
	}
	if args := conn.args[len(conn.args)-1]; len(args) != 2 {
		t.Errorf("bound %d args, want 2", len(args))
	}
}

// With a plan, the participant arms read only the account's partitions, stale ones and the tail; without one they stay unfiltered.
func TestAccountListings_ParticipantReadIsPartitionScoped(t *testing.T) {
	planned := true
	conn := &stubConn{respond: func(q string) (driver.Rows, error) {
		switch {
		case isOpTypesCoverageProbe(q):
			return &stubRows{data: [][]any{{uint32(1)}}}, nil
		case strings.Contains(q, "WHERE account = ''"):
			if !planned {
				return &stubRows{}, nil
			}
			return &stubRows{data: [][]any{{uint32(0), uint32(mL), uint64(5)}, {uint32(1), uint32(mL), uint64(4)}, {uint32(2), uint32(2*mL + 9), uint64(0)}}}, nil
		case strings.Contains(q, "system.parts"):
			return &stubRows{data: [][]any{{uint32(0), uint64(5)}, {uint32(1), uint64(3)}, {uint32(2), uint64(1)}}}, nil
		case strings.Contains(q, "SELECT DISTINCT part FROM stellar.account_op_type_counts"):
			return &stubRows{data: [][]any{{uint32(0)}}}, nil
		}
		return (&opsBySourceLake{rows: true}).respond(q)
	}}
	participantQuery := func() string {
		for i := len(conn.queries) - 1; i >= 0; i-- {
			if strings.Contains(conn.queries[i], "FROM stellar.operation_participants") {
				return conn.queries[i]
			}
		}
		t.Fatal("no participant read")
		return ""
	}
	clk := newFakeClock()
	r := &ExplorerReader{conn: conn, opTypesCoverageProbe: schemaProbe{now: clk.now}}
	ctx := context.Background()
	want := "AND (ledger_seq >= 2000000 OR intDiv(ledger_seq, 1000000) IN (0, 1))"
	if _, _, err := r.AccountTransactions(ctx, "GTESTPARTS", 50, ExplorerCursor{}); err != nil {
		t.Fatal(err)
	}
	if q := participantQuery(); !strings.Contains(q, want) {
		t.Errorf("tx participant read lacks %q:\n%s", want, q)
	}
	if _, _, err := r.AccountOperations(ctx, "GTESTPARTS", 50, ExplorerCursor{}); err != nil {
		t.Fatal(err)
	}
	if q := participantQuery(); !strings.Contains(q, want) {
		t.Errorf("op participant read lacks %q:\n%s", want, q)
	}

	planned = false
	r.opTypeRollup = opTypeRollupCache{}
	if _, _, err := r.AccountTransactions(ctx, "GTESTPARTS", 50, ExplorerCursor{}); err != nil {
		t.Fatal(err)
	}
	if q := participantQuery(); strings.Contains(q, "intDiv") {
		t.Errorf("no plan: participant read is still partition-scoped:\n%s", q)
	}
}

// The movements listing reads only the partitions account_movement_counts has the address in, plus stale ones and the tail's whole
// partition; without a plan it reads every partition.
func TestAccountMovements_ReadIsPartitionScoped(t *testing.T) {
	planned := true
	conn := &stubConn{respond: func(q string) (driver.Rows, error) {
		switch {
		case strings.Contains(q, "FROM stellar.account_movement_counts WHERE account = ''"):
			if !planned {
				return &stubRows{}, nil
			}
			return &stubRows{data: [][]any{{uint32(0), uint32(mL), uint64(5)}, {uint32(1), uint32(mL), uint64(4)}, {uint32(2), uint32(2*mL + 9), uint64(0)}}}, nil
		case strings.Contains(q, "system.parts"):
			return &stubRows{data: [][]any{{uint32(0), uint64(5)}, {uint32(1), uint64(3)}, {uint32(2), uint64(1)}}}, nil
		case strings.Contains(q, "SELECT DISTINCT part FROM stellar.account_movement_counts"):
			return &stubRows{data: [][]any{{uint32(0)}}}, nil
		}
		return &stubRows{}, nil
	}}
	movementsQuery := func() string {
		for i := len(conn.queries) - 1; i >= 0; i-- {
			if strings.Contains(conn.queries[i], "FROM stellar.account_movements WHERE") {
				return conn.queries[i]
			}
		}
		t.Fatal("no movements read")
		return ""
	}
	r := &ExplorerReader{conn: conn}
	ctx := context.Background()
	if _, err := r.AccountMovements(ctx, "GTESTMOVES", 25, AccountMovementCursor{}, AccountMovementFilter{}); err != nil {
		t.Fatal(err)
	}
	want := "WHERE address = ? AND (ledger >= 2000000 OR intDiv(ledger, 1000000) IN (0, 1))"
	if q := movementsQuery(); !strings.Contains(q, want) {
		t.Errorf("movements read lacks %q:\n%s", want, q)
	}

	planned = false
	r.movementRollup = opTypeRollupCache{}
	if _, err := r.AccountMovements(ctx, "GTESTMOVES", 25, AccountMovementCursor{}, AccountMovementFilter{}); err != nil {
		t.Fatal(err)
	}
	if q := movementsQuery(); strings.Contains(q, "intDiv") {
		t.Errorf("no plan: movements read is still partition-scoped:\n%s", q)
	}
}

// An address in several partitions is first read from its newest one alone; only a full page from it is served, since an older
// partition can still hold the rest of a short one.
func TestAccountMovements_NewestPartitionFirst(t *testing.T) {
	const limit = 3
	row := func(ledger uint32) []any { return movementRow(ledger, "c", time.Unix(1_000, 0).UTC()) }
	cases := []struct {
		name        string
		cur         AccountMovementCursor
		bounded     [][]any
		wantReads   int
		wantBounded bool
	}{
		{"full page from the newest partition", AccountMovementCursor{}, [][]any{row(2*mL + 5), row(2*mL + 4), row(mL + 7)}, 1, true},
		{"short page widens to every partition", AccountMovementCursor{}, [][]any{row(mL + 7)}, 2, true},
		{"cursor below the newest partition", AccountMovementCursor{Ledger: 500, TxHash: "tx"}, nil, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := &stubConn{respond: func(q string) (driver.Rows, error) {
				switch {
				case strings.Contains(q, "FROM stellar.account_movement_counts WHERE account = ''"):
					return &stubRows{data: [][]any{{uint32(0), uint32(mL), uint64(5)}, {uint32(1), uint32(mL), uint64(4)}, {uint32(2), uint32(2*mL + 9), uint64(0)}}}, nil
				case strings.Contains(q, "system.parts"):
					return &stubRows{data: [][]any{{uint32(0), uint64(5)}, {uint32(1), uint64(3)}, {uint32(2), uint64(1)}}}, nil
				case strings.Contains(q, "SELECT DISTINCT part FROM stellar.account_movement_counts"):
					return &stubRows{data: [][]any{{uint32(0)}}}, nil
				case strings.Contains(q, "ledger >= 1000000 ORDER BY"):
					return &stubRows{data: tc.bounded}, nil
				}
				return &stubRows{}, nil
			}}
			r := &ExplorerReader{conn: conn}
			page, err := r.AccountMovements(context.Background(), "GTESTMOVES", limit, tc.cur, AccountMovementFilter{})
			if err != nil {
				t.Fatal(err)
			}
			var reads []string
			for _, q := range conn.queries {
				if strings.Contains(q, "FROM stellar.account_movements WHERE") {
					reads = append(reads, q)
				}
			}
			if len(reads) != tc.wantReads {
				t.Fatalf("got %d movements reads, want %d: %v", len(reads), tc.wantReads, reads)
			}
			if got := strings.Contains(reads[0], "ledger >= 1000000 ORDER BY"); got != tc.wantBounded {
				t.Errorf("first read bounded to the newest partition = %v, want %v:\n%s", got, tc.wantBounded, reads[0])
			}
			if tc.wantBounded && tc.wantReads == 1 && len(page) != limit {
				t.Errorf("page = %d rows, want %d", len(page), limit)
			}
			for _, q := range reads[1:] {
				if strings.Contains(q, "ledger >= 1000000 ORDER BY") {
					t.Errorf("a widened read is still bounded:\n%s", q)
				}
			}
		})
	}
}

func TestRunPartition_MovementsCountsItsOwnColumns(t *testing.T) {
	conn := buildStub(10, 9)
	w := OpTypeRollupPartition{Part: 60, Through: opTypePartitionEnd(60)}
	if err := MovementCountsRollup.runPartitionConn(context.Background(), conn, w, false, t.Logf); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"SELECT toUInt32(?), address, movement_kind, count(), toUInt32(?)",
		"FROM stellar.account_movements FINAL",
		"WHERE ledger >= ? AND ledger < ?",
		"ALTER TABLE stellar.account_movement_counts REPLACE PARTITION ID '60'",
	} {
		if len(conn.execd(want)) != 1 {
			t.Errorf("no exec containing %q: %v", want, conn.execs)
		}
	}
}

type missingRollupConn struct{ driver.Conn }

func (missingRollupConn) Query(context.Context, string, ...any) (driver.Rows, error) {
	return nil, errors.New("code: 60, Table stellar.account_movement_counts does not exist")
}

// A missing rollup table sends every account read to the source scan; the reader must say so on the plan gauge, not fall back silently.
func TestPlanFor_ExportsUnusablePlan(t *testing.T) {
	obs.CountsRollupPlanOK.WithLabelValues(MovementCountsRollup.Name).Set(1)
	obs.CountsRollupStalePartitions.WithLabelValues(MovementCountsRollup.Name).Set(3)
	r := &ExplorerReader{conn: missingRollupConn{}}
	if _, ok := r.planFor(context.Background(), MovementCountsRollup); ok {
		t.Fatal("planFor ok with the rollup table missing")
	}
	if got := testutil.ToFloat64(obs.CountsRollupPlanOK.WithLabelValues(MovementCountsRollup.Name)); got != 0 {
		t.Errorf("plan_ok = %v, want 0", got)
	}
	if got := testutil.ToFloat64(obs.CountsRollupStalePartitions.WithLabelValues(MovementCountsRollup.Name)); got != 0 {
		t.Errorf("stale_partitions = %v, want 0", got)
	}
}
