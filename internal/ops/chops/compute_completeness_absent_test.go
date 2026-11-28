package chops

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
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

var emptyServed = []servedFloor{{}}

func TestJudgeDeclaredAbsence_ZeroLakeReadApplies(t *testing.T) {
	st := &stubRead{}
	got, err := judgeDeclaredAbsence(context.Background(), absentTestSource(), emptyServed, true, 2, 26107, st.read)
	if err != nil {
		t.Fatal(err)
	}
	if !got.applied || got.present {
		t.Fatalf("got %+v, want applied and not present", got)
	}
	const want = "projection: absent by design on this network (declared); lake holds no trades in [2,26107]"
	if got.detail != want {
		t.Errorf("detail = %q, want %q", got.detail, want)
	}
	if len(st.calls) != 1 || st.calls[0] != [2]uint32{2, 26107} {
		t.Errorf("lake reads = %v, want exactly one over [2,26107]", st.calls)
	}
}

// An empty served tier is exactly the state in which a served-scoped reconcile skips the lake and returns 0. The
// declaration must still read the lake: a lake holding rows is a present source, never an applied declaration.
func TestJudgeDeclaredAbsence_EmptyServedStillReadsTheLake(t *testing.T) {
	st := &stubRead{expected: 3}
	got, err := judgeDeclaredAbsence(context.Background(), absentTestSource(), emptyServed, true, 2, 26107, st.read)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.calls) != 1 || st.calls[0] != [2]uint32{2, 26107} {
		t.Fatalf("lake reads = %v, want one over [2,26107] — the declaration was judged without reading the lake", st.calls)
	}
	if got.applied || !got.present {
		t.Fatalf("got %+v, want present and not applied", got)
	}
	if !strings.Contains(got.detail, "lake holds 3 expected trades row(s) in [2,26107]") {
		t.Errorf("detail = %q, want the lake count named", got.detail)
	}
}

func TestJudgeDeclaredAbsence_ServedRowsProvePresence(t *testing.T) {
	st := &stubRead{}
	got, err := judgeDeclaredAbsence(context.Background(), absentTestSource(), []servedFloor{{min: 900, present: true}}, true, 2, 26107, st.read)
	if err != nil {
		t.Fatal(err)
	}
	if got.applied || !got.present {
		t.Fatalf("got %+v, want present and not applied", got)
	}
	if len(st.calls) != 0 {
		t.Errorf("lake reads = %v, want none: served rows already prove the network has the source", st.calls)
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
			got, err := judgeDeclaredAbsence(context.Background(), absentTestSource(), emptyServed, tc.verified, 2, 26107, tc.read.read)
			if err != nil {
				t.Fatal(err)
			}
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
	st := &stubRead{err: errors.New("ch down")}
	if _, err := judgeDeclaredAbsence(context.Background(), absentTestSource(), emptyServed, true, 2, 26107, st.read); err == nil {
		t.Fatal("want an error: a failed lake read must not be judged as an empty lake")
	}
}

func TestParseAbsentByDesign(t *testing.T) {
	cat := []reconSource{{name: "sdex"}, {name: "soroswap"}}
	got, err := parseAbsentByDesign(" sdex, ,soroswap", cat)
	if err != nil || !got["sdex"] || !got["soroswap"] || len(got) != 2 {
		t.Fatalf("got %v, %v; want sdex and soroswap", got, err)
	}
	if got, err := parseAbsentByDesign("", cat); err != nil || len(got) != 0 {
		t.Fatalf("empty flag: got %v, %v; want none", got, err)
	}
	for _, bad := range []string{"sdx", "sep1/issuers", "sdex,blend"} {
		if _, err := parseAbsentByDesign(bad, cat); err == nil {
			t.Errorf("%q: want an error naming the unknown source", bad)
		}
	}
}
