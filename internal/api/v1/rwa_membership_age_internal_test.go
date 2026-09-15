package v1

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The membership set's own age, on the wire.
//
// These tests pin what the response could not say on 2026-09-15. The
// envelope's `as_of` is the RESPONSE's instant and moves sub-second
// between requests; read as the set's build time it says the set is
// refreshing continuously, and the conclusion drawn from it — that
// rebuilds were running and seeing rows — was the exact opposite of
// what was happening. The set was ten minutes old and nothing had
// looked at its source since before the sync landed.
//
// `membership.built_at` is a different event from `as_of` and is named
// so it cannot be substituted for one: a set is BUILT, a response is AS
// OF. The two tests below fix the distinction in place — that the field
// tracks the REBUILD and not the request, and that a set serving past
// its lifetime says so.

func rwaAgeTestServer() *Server {
	return &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// built_at must date the REBUILD, and must not move when the set is
// read again. A field that advanced on every read would be the
// envelope's `as_of` under another name — the substitution this block
// exists to prevent — and would report a frozen cache as continuously
// fresh.
func TestRWAMembershipSet_DatesTheRebuildNotTheRead(t *testing.T) {
	s := rwaAgeTestServer()
	built := time.Now().UTC().Add(-3 * time.Minute)
	s.rwaCache = &rwaMembership{available: true, refusals: map[string]int{}, builtAt: built}
	s.rwaAt = built

	first := rwaMembershipSetOf(s.cachedRWAMembership(context.Background()))
	if first == nil {
		t.Fatal("a built set published no date: the response cannot say how old the thing it describes is")
	}
	if got := first.BuiltAt.Time(); !got.Equal(built) {
		t.Errorf("built_at = %s, want the rebuild's instant %s", got, built)
	}

	time.Sleep(20 * time.Millisecond)
	second := rwaMembershipSetOf(s.cachedRWAMembership(context.Background()))
	if !second.BuiltAt.Time().Equal(first.BuiltAt.Time()) {
		t.Errorf("built_at moved between two reads of ONE set (%s then %s) — "+
			"a field that advances per request is the envelope's as_of under "+
			"another name, and reports a frozen cache as continuously fresh",
			first.BuiltAt.Time(), second.BuiltAt.Time())
	}
	if first.Stale {
		t.Error("a set three minutes into a ten-minute lifetime reported itself stale")
	}
}

// The serve states, which are the point: a lapsed set is served either
// way, and only one of the two needs anybody to do anything.
func TestRWAMembershipSet_SeparatesARebuildComingFromNothingComing(t *testing.T) {
	for _, tc := range []struct {
		name        string
		age         time.Duration
		failedAgo   time.Duration // 0 = no failure recorded
		wantStale   bool
		wantFailure bool
		reading     string
	}{
		{
			name:    "inside its lifetime",
			age:     time.Minute,
			reading: "fresh: nothing to say and nothing to do",
		},
		{
			name:      "lapsed, a rebuild is coming",
			age:       rwaMembershipTTL + time.Minute,
			wantStale: true,
			reading: "expected and transient — the cache serves the lapsed set " +
				"rather than making a request wait for the rescan",
		},
		{
			name:        "lapsed, and nothing is replacing it",
			age:         rwaMembershipTTL + time.Minute,
			failedAgo:   30 * time.Second,
			wantStale:   true,
			wantFailure: true,
			reading:     "the last attempt to replace this set FAILED: the state to act on",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rwaAgeTestServer()
			built := time.Now().UTC().Add(-tc.age)
			s.rwaCache = &rwaMembership{available: true, refusals: map[string]int{}, builtAt: built}
			s.rwaAt = built
			// Gapped out, so the read under test serves rather than
			// kicking a rebuild that would move the clock underneath it.
			s.rwaAttemptAt = time.Now()
			if tc.failedAgo != 0 {
				s.rwaFailedAt = time.Now().UTC().Add(-tc.failedAgo)
			}

			got := rwaMembershipSetOf(s.cachedRWAMembership(context.Background()))
			if got == nil {
				t.Fatalf("no date published; a reader cannot reach %q", tc.reading)
			}
			if got.Stale != tc.wantStale {
				t.Errorf("stale = %v, want %v — %s", got.Stale, tc.wantStale, tc.reading)
			}
			if (got.RebuildFailedAt != nil) != tc.wantFailure {
				t.Errorf("rebuild_failed_at present = %v, want %v — without it a set "+
					"nothing is replacing renders identically to one a rebuild is "+
					"about to replace, and only the first needs anybody to act",
					got.RebuildFailedAt != nil, tc.wantFailure)
			}
		})
	}
}

// A successful rebuild must CLEAR the failure marker. The field answers
// "is the latest thing to have happened a failure", not "has one ever
// happened" — an alarm that never clears is one nobody reads twice.
func TestRWAMembershipSet_ASuccessfulRebuildClearsTheFailure(t *testing.T) {
	s := rwaAgeTestServer()
	s.sep1Cache = &stubAgeSep1Reader{}
	s.rwaFailedAt = time.Now().UTC().Add(-time.Minute)

	s.refreshRWAMembership(make(chan struct{}))

	set := rwaMembershipSetOf(s.cachedRWAMembership(context.Background()))
	if set == nil {
		t.Fatal("the rebuild completed and left the set undated")
	}
	if set.RebuildFailedAt != nil {
		t.Errorf("a successful rebuild left rebuild_failed_at at %s: the marker "+
			"reports the LATEST outcome, not the worst one ever seen",
			set.RebuildFailedAt.Time())
	}
	if set.Stale {
		t.Error("a set built moments ago reported itself stale")
	}
	if set.BuiltAt.IsZero() {
		t.Error("the rebuild did not stamp the set with its own instant")
	}
}

// And a FAILED rebuild must record one, so the loud state is reachable
// from the path that actually produces it rather than only from a
// hand-set field.
func TestRWAMembershipSet_AFailedRebuildRecordsIt(t *testing.T) {
	s := rwaAgeTestServer()
	s.sep1Cache = &stubAgeSep1Reader{err: context.DeadlineExceeded}
	built := time.Now().UTC().Add(-rwaMembershipTTL - time.Minute)
	s.rwaCache = &rwaMembership{available: true, refusals: map[string]int{}, builtAt: built}
	s.rwaAt = built

	s.refreshRWAMembership(make(chan struct{}))
	s.rwaAttemptAt = time.Now() // gap the read below, not the rebuild above

	set := rwaMembershipSetOf(s.cachedRWAMembership(context.Background()))
	if set == nil {
		t.Fatal("the last good set was served undated")
	}
	if !set.BuiltAt.Time().Equal(built) {
		t.Errorf("built_at = %s, want the LAST GOOD set's instant %s — a failed "+
			"rebuild must not re-date the set it failed to replace",
			set.BuiltAt.Time(), built)
	}
	if set.RebuildFailedAt == nil {
		t.Fatal("a failed rebuild published nothing: the set is lapsed, nothing " +
			"is replacing it, and the response says only that it is stale")
	}
	if !set.Stale {
		t.Error("the lapsed set did not report itself stale")
	}
}

// stubAgeSep1Reader is the smallest reader that makes a rebuild either
// succeed or fail on demand. The set it produces is empty, which is
// fine here: these tests are about WHEN the set was built, never what
// is in it.
type stubAgeSep1Reader struct{ err error }

func (s *stubAgeSep1Reader) GetIssuerSep1Cached(
	context.Context, string,
) (*timescale.IssuerSep1Cached, error) {
	return nil, nil
}

func (s *stubAgeSep1Reader) BoundSep1Currencies(
	context.Context, timescale.Sep1CurrencyFilter,
) ([]timescale.Sep1BoundCurrency, timescale.Sep1BoundCensus, error) {
	if s.err != nil {
		return nil, timescale.Sep1BoundCensus{}, s.err
	}
	return nil, timescale.Sep1BoundCensus{}, nil
}
