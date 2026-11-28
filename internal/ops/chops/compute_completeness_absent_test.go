package chops

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func absentTestSource() reconSource {
	return reconSource{name: "sdex", census: true, targets: []reconTarget{{table: "trades", whereFilter: sdexTradesFilter}}}
}

// stubRead records every call so a test can prove the lake was actually asked over the whole range.
type stubRead struct {
	expected, served int
	blind            completeness.BlindSpots
	err              error
	calls            [][2]uint32
}

func (s *stubRead) read(_ context.Context, lo, hi uint32) (int, int, completeness.BlindSpots, error) {
	s.calls = append(s.calls, [2]uint32{lo, hi})
	return s.expected, s.served, s.blind, s.err
}

// stubRaw is the raw-table census: the tables it reports short, or an error.
type stubRaw struct {
	short []string
	err   error
	calls int
}

func (s *stubRaw) check(context.Context, uint32, uint32) ([]string, error) {
	s.calls++
	return s.short, s.err
}

var emptyServed = []servedFloor{{}}

func judgeWith(t *testing.T, served []servedFloor, verified bool, rd *stubRead, raw *stubRaw) declaredAbsence {
	t.Helper()
	got, err := judgeDeclaredAbsence(context.Background(), absentTestSource(), served, verified, 2, 26107, rd.read, raw.check)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestJudgeDeclaredAbsence_ZeroLakeReadOverWholeTablesApplies(t *testing.T) {
	rd, raw := &stubRead{}, &stubRaw{}
	got := judgeWith(t, emptyServed, true, rd, raw)
	if !got.applied || got.present {
		t.Fatalf("got %+v, want applied and not present", got)
	}
	const want = "projection: absent by design on this network (declared); lake holds no trades in [2,26107] and its raw tables are whole"
	if got.detail != want {
		t.Errorf("detail = %q, want %q", got.detail, want)
	}
	if len(rd.calls) != 1 || rd.calls[0] != [2]uint32{2, 26107} || raw.calls != 1 {
		t.Errorf("lake reads = %v, census calls = %d; want one read over [2,26107] and one census", rd.calls, raw.calls)
	}
}

// Ledger headers declare operations the lake does not hold: the join under the census returns nothing, which is not absence.
func TestJudgeDeclaredAbsence_ShortRawTablesRefuse(t *testing.T) {
	got := judgeWith(t, emptyServed, true, &stubRead{}, &stubRaw{short: []string{"stellar.operations partition 0 holds 0 of the 39 rows stellar.ledgers declares"}})
	if got.applied || got.present {
		t.Fatalf("got %+v, want not applied and not present", got)
	}
	if !strings.Contains(got.detail, "stellar.operations partition 0 holds 0 of the 39 rows") {
		t.Errorf("detail = %q, want the short table named", got.detail)
	}
}

func TestJudgeDeclaredAbsence_CensusErrorFails(t *testing.T) {
	_, err := judgeDeclaredAbsence(context.Background(), absentTestSource(), emptyServed, true, 2, 26107, (&stubRead{}).read, (&stubRaw{err: errors.New("ch down")}).check)
	if err == nil {
		t.Fatal("want an error: an unread census must never let the declaration apply")
	}
}

// An empty served tier is exactly the state in which a served-scoped reconcile skips the lake and returns 0. The
// declaration must still read the lake: a lake holding rows is a present source, never an applied declaration.
func TestJudgeDeclaredAbsence_EmptyServedStillReadsTheLake(t *testing.T) {
	rd := &stubRead{expected: 3}
	got := judgeWith(t, emptyServed, true, rd, &stubRaw{})
	if len(rd.calls) != 1 || rd.calls[0] != [2]uint32{2, 26107} {
		t.Fatalf("lake reads = %v, want one over [2,26107] — the declaration was judged without reading the lake", rd.calls)
	}
	if got.applied || !got.present {
		t.Fatalf("got %+v, want present and not applied", got)
	}
	if !strings.Contains(got.detail, "lake holds 3 expected trades row(s) in [2,26107]") {
		t.Errorf("detail = %q, want the lake count named", got.detail)
	}
}

func TestJudgeDeclaredAbsence_ServedRowsProvePresence(t *testing.T) {
	rd := &stubRead{}
	got := judgeWith(t, []servedFloor{{min: 900, present: true}}, true, rd, &stubRaw{})
	if got.applied || !got.present {
		t.Fatalf("got %+v, want present and not applied", got)
	}
	if len(rd.calls) != 0 {
		t.Errorf("lake reads = %v, want none: served rows already prove the network has the source", rd.calls)
	}
}

func TestJudgeDeclaredAbsence_RefusesWithoutProof(t *testing.T) {
	bt := completeness.NewBlindTracker()
	bt.Undecodable(40)
	for name, tc := range map[string]struct {
		verified bool
		read     *stubRead
	}{
		"blind lake read":          {verified: true, read: &stubRead{blind: bt.Result()}},
		"substrate not verified":   {verified: false, read: &stubRead{}},
		"served rows only in read": {verified: true, read: &stubRead{served: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			got := judgeWith(t, emptyServed, tc.verified, tc.read, &stubRaw{})
			if got.applied {
				t.Fatalf("got %+v, want the declaration not applied", got)
			}
			if got.detail == "" {
				t.Error("no detail: a declaration that is not applied must say why")
			}
		})
	}
}

func TestJudgeDeclaredAbsence_ReadErrorFails(t *testing.T) {
	if _, err := judgeDeclaredAbsence(context.Background(), absentTestSource(), emptyServed, true, 2, 26107, (&stubRead{err: errors.New("ch down")}).read, (&stubRaw{}).check); err == nil {
		t.Fatal("want an error: a failed lake read must not be judged as an empty lake")
	}
}

// The reconcile's own whole-range read is reused, not repeated (a nil store would panic on a second read).
func TestLakeAbsenceReader_ReusesWholeRangeReconcile(t *testing.T) {
	done := projectionRead{lo: 2, hi: 26107, ran: true, uniform: true}
	exp, served, _, err := lakeAbsenceReader(nil, nil, "", absentTestSource(), done)(context.Background(), 2, 26107)
	if err != nil || exp != 0 || served != 0 {
		t.Fatalf("got %d/%d/%v, want the reconcile's zero counts reused", exp, served, err)
	}
}

func TestParseAbsentByDesign(t *testing.T) {
	cat := []reconSource{{name: "sdex", census: true}, {name: "soroswap"}}
	got, err := parseAbsentByDesign(" sdex, ", cat)
	if err != nil || !got["sdex"] || len(got) != 1 {
		t.Fatalf("got %v, %v; want sdex", got, err)
	}
	if got, err := parseAbsentByDesign("", cat); err != nil || len(got) != 0 {
		t.Fatalf("empty flag: got %v, %v; want none", got, err)
	}
	for _, bad := range []string{"sdx", "sep1/issuers", "sdex,blend"} {
		if _, err := parseAbsentByDesign(bad, cat); err == nil || !strings.Contains(err.Error(), "matches no reconciliation source") {
			t.Errorf("%q: err = %v, want an unknown-source error", bad, err)
		}
	}
	if _, err := parseAbsentByDesign("sdex,soroswap", cat); err == nil || !strings.Contains(err.Error(), "gated on configured contract identity") {
		t.Errorf("identity-gated soroswap: err = %v, want a refusal naming why", err)
	}
}

func TestPriorDeclaredPresence(t *testing.T) {
	snaps := []timescale.CompletenessSnapshot{
		{Source: "sdex", AbsentDeclaredButPresent: true},
		{Source: "band", AbsentDeclaredButPresent: true},
		{Source: "soroswap"},
	}
	got := priorDeclaredPresence(snaps, map[string]bool{"sdex": true, "soroswap": true})
	if !got["sdex"] || got["band"] || got["soroswap"] {
		t.Fatalf("got %v, want only declared sdex carried", got)
	}
}
