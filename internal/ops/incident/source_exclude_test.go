package incident

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type recordingAudit struct {
	entries []platform.AuditEntry
	err     error
	log     *[]string
}

func (r *recordingAudit) Append(_ context.Context, e platform.AuditEntry) error {
	*r.log = append(*r.log, "audit")
	if r.err != nil {
		return r.err
	}
	r.entries = append(r.entries, e)
	return nil
}

// fakeExclusionStore mirrors *timescale.Store's lift semantics. raceLift stamps every row lifted right after
// UnliftedSourceExclusions returns, standing in for a concurrent lift.
type fakeExclusionStore struct {
	rows     []timescale.SourceExclusion
	log      *[]string
	raceLift bool
}

func (f *fakeExclusionStore) InsertSourceExclusion(_ context.Context, e timescale.SourceExclusion) (int64, error) {
	*f.log = append(*f.log, "insert")
	e.ID = int64(len(f.rows) + 1)
	f.rows = append(f.rows, e)
	return e.ID, nil
}

func (f *fakeExclusionStore) LiftSourceExclusion(_ context.Context, id int64, by, reason string) error {
	*f.log = append(*f.log, "lift")
	for i := range f.rows {
		if f.rows[i].ID != id {
			continue
		}
		if f.rows[i].Lifted() {
			return fmt.Errorf("source exclusion %d: %w", id, timescale.ErrSourceExclusionLifted)
		}
		f.rows[i].LiftedAt, f.rows[i].LiftedBy, f.rows[i].LiftReason = time.Now(), by, reason
		return nil
	}
	return fmt.Errorf("source exclusion %d: %w", id, timescale.ErrNotFound)
}

func (f *fakeExclusionStore) UnliftedSourceExclusions(context.Context) ([]timescale.SourceExclusion, error) {
	var out []timescale.SourceExclusion
	for _, r := range f.rows {
		if !r.Lifted() {
			out = append(out, r)
		}
	}
	if f.raceLift {
		for i := range f.rows {
			f.rows[i].LiftedAt, f.rows[i].LiftedBy, f.rows[i].LiftReason = time.Now(), "other", "raced"
		}
	}
	return out, nil
}

var testNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func resolveArgs(t *testing.T, args ...string) (sourceExcludeRequest, error) {
	t.Helper()
	fs, gate := opsutil.NewMutatingFlagSet("source-exclude")
	f := registerSourceExcludeFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return f.resolve(testNow, gate.Enabled())
}

func TestSourceExcludeFlags_Validation(t *testing.T) {
	ok := []string{"-source", "binance", "-from", "2026-11-17T09:00:00Z", "-reason", "venue halted", "-actor", "ops"}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"unknown source lists valid names", []string{"-source", "binanse", "-from", "2026-11-17T09:00:00Z", "-reason", "r", "-actor", "a"}, "valid: "},
		{"missing source", []string{"-from", "2026-11-17T09:00:00Z", "-reason", "r", "-actor", "a"}, "-source is required"},
		{"missing reason", []string{"-source", "binance", "-from", "2026-11-17T09:00:00Z", "-actor", "a"}, "-reason is required"},
		{"blank reason", []string{"-source", "binance", "-from", "2026-11-17T09:00:00Z", "-reason", "  ", "-actor", "a"}, "-reason is required"},
		{"missing from", []string{"-source", "binance", "-reason", "r", "-actor", "a"}, "-from is required"},
		{"bad from", []string{"-source", "binance", "-from", "2026-11-17", "-reason", "r", "-actor", "a"}, "want RFC3339"},
		{"until not after from", []string{"-source", "binance", "-from", "2026-11-17T09:00:00Z", "-until", "2026-11-17T09:00:00Z", "-reason", "r", "-actor", "a"}, "must be after"},
		{"base without quote", append([]string{"-base", "native"}, ok...), "go together"},
		{"quote without base", append([]string{"-quote", "native"}, ok...), "go together"},
		{"bad asset", append([]string{"-base", "not an asset", "-quote", "native"}, ok...), "-base"},
		{"same asset", append([]string{"-base", "native", "-quote", "native"}, ok...), "same asset"},
		{"lift without id", []string{"-lift", "-reason", "r", "-actor", "a"}, "-lift requires -id"},
		{"lift with scope flags", []string{"-lift", "-id", "3", "-source", "binance", "-reason", "r", "-actor", "a"}, "-lift takes only"},
		{"lift without reason", []string{"-lift", "-id", "3", "-actor", "a"}, "-reason is required"},
		{"id without lift", append([]string{"-id", "3"}, ok...), "only valid with -lift"},
		{"list and lift", []string{"-list", "-lift"}, "mutually exclusive"},
		{"lift with allow-overlap", []string{"-lift", "-id", "3", "-allow-overlap", "-reason", "r", "-actor", "a"}, "-lift takes only"},
		{"from more than 24h ahead", []string{"-source", "binance", "-from", "2062-11-17T09:00:00Z", "-reason", "r", "-actor", "a"}, "-allow-future"},
		{"from just over 24h ahead", []string{"-source", "binance", "-from", "2026-11-18T12:00:01Z", "-reason", "r", "-actor", "a"}, "-allow-future"},
	}
	for _, flagArgs := range [][]string{
		{"-source", "binance"},
		{"-base", "native"},
		{"-quote", "native"},
		{"-from", "2026-11-17T09:00:00Z"},
		{"-until", "2026-11-17T09:00:00Z"},
		{"-reason", "r"},
		{"-id", "3"},
		{"-write"},
		{"-allow-overlap"},
		{"-allow-future"},
	} {
		cases = append(cases, struct {
			name string
			args []string
			want string
		}{"list with " + flagArgs[0], append([]string{"-list"}, flagArgs...), "-list only reads"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveArgs(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}

	req, err := resolveArgs(t, ok...)
	if err != nil {
		t.Fatalf("valid source-wide request refused: %v", err)
	}
	if req.mode != modeCreate || req.source != "binance" || req.base != "" || req.quote != "" || !req.until.IsZero() {
		t.Fatalf("source-wide request = %+v", req)
	}
	if req, err := resolveArgs(t, "-list", "-actor", "a"); err != nil || req.mode != modeList || !req.now.Equal(testNow) {
		t.Fatalf("-list = %+v, %v; want list mode with no reason required", req, err)
	}
	for _, from := range []string{"2026-11-18T12:00:00Z", "2020-01-01T00:00:00Z"} {
		if _, err := resolveArgs(t, "-source", "binance", "-from", from, "-reason", "r", "-actor", "a"); err != nil {
			t.Fatalf("-from %s refused: %v", from, err)
		}
	}
	if _, err := resolveArgs(t, "-source", "binance", "-from", "2062-11-17T09:00:00Z", "-allow-future", "-reason", "r", "-actor", "a"); err != nil {
		t.Fatalf("-allow-future did not admit a far-future -from: %v", err)
	}
}

func TestSourceExcludeFlags_PairStoredInOrientOrder(t *testing.T) {
	base := []string{"-source", "binance", "-from", "2026-11-17T09:00:00Z", "-reason", "r", "-actor", "a"}
	fwd, err := resolveArgs(t, append([]string{"-base", "crypto:BTC", "-quote", "native"}, base...)...)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := resolveArgs(t, append([]string{"-base", "native", "-quote", "crypto:BTC"}, base...)...)
	if err != nil {
		t.Fatal(err)
	}
	if fwd.base != "crypto:BTC" || fwd.quote != "native" || fwd.flipped {
		t.Fatalf("crypto:BTC/native = %s/%s flipped=%t, want unchanged", fwd.base, fwd.quote, fwd.flipped)
	}
	if rev.base != fwd.base || rev.quote != fwd.quote || !rev.flipped {
		t.Fatalf("native/crypto:BTC stored as %s/%s flipped=%t, want crypto:BTC/native flipped", rev.base, rev.quote, rev.flipped)
	}
}

func createReq() sourceExcludeRequest {
	return sourceExcludeRequest{
		mode: modeCreate, source: "binance", from: time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC),
		reason: "venue halted", actor: "ops",
	}
}

func TestCreateSourceExclusion_DryRunWritesNothing(t *testing.T) {
	var log []string
	audit, store := &recordingAudit{log: &log}, &fakeExclusionStore{log: &log}
	if err := createSourceExclusion(context.Background(), audit, store, createReq(), true); err != nil {
		t.Fatal(err)
	}
	if len(log) != 0 {
		t.Fatalf("dry run called %v, want nothing", log)
	}
}

func TestCreateSourceExclusion_AuditPrecedesInsert(t *testing.T) {
	var log []string
	audit, store := &recordingAudit{log: &log}, &fakeExclusionStore{log: &log}
	if err := createSourceExclusion(context.Background(), audit, store, createReq(), false); err != nil {
		t.Fatal(err)
	}
	if strings.Join(log, ",") != "audit,insert,audit" {
		t.Fatalf("call order = %v, want audit, insert, then the id-linking audit", log)
	}
	if got := audit.entries[0].Action; got != "source.exclude" {
		t.Fatalf("audit action = %q", got)
	}
	if e := audit.entries[1]; e.Action != "source.exclude.recorded" || e.TargetID != "id:1" || e.TargetKind != "source_exclusion" {
		t.Fatalf("linking audit row = %s %s:%s, want source.exclude.recorded source_exclusion:id:1", e.Action, e.TargetKind, e.TargetID)
	}
	if len(store.rows) != 1 || store.rows[0].Source != "binance" || store.rows[0].Reason != "venue halted" {
		t.Fatalf("rows = %+v", store.rows)
	}
}

func TestCreateSourceExclusion_FailedAuditChangesNothing(t *testing.T) {
	var log []string
	audit := &recordingAudit{log: &log, err: errors.New("audit down")}
	store := &fakeExclusionStore{log: &log}
	err := createSourceExclusion(context.Background(), audit, store, createReq(), false)
	if err == nil || !strings.Contains(err.Error(), "NOTHING was changed") {
		t.Fatalf("err = %v, want the audit failure", err)
	}
	if len(store.rows) != 0 || strings.Join(log, ",") != "audit" {
		t.Fatalf("store touched after a failed audit: log=%v rows=%d", log, len(store.rows))
	}
}

func TestLiftSourceExclusion_AuditPrecedesLiftAndKeepsRow(t *testing.T) {
	var log []string
	store := &fakeExclusionStore{log: &log, rows: []timescale.SourceExclusion{{ID: 1, Source: "binance"}}}
	req := sourceExcludeRequest{mode: modeLift, id: 1, reason: "venue back", actor: "ops"}

	failing := &recordingAudit{log: &log, err: errors.New("audit down")}
	if err := liftSourceExclusion(context.Background(), failing, store, req, false); err == nil {
		t.Fatal("lift succeeded with a failing audit")
	}
	if store.rows[0].Lifted() || strings.Join(log, ",") != "audit" {
		t.Fatalf("row lifted after a failed audit: log=%v", log)
	}

	log = nil
	if err := liftSourceExclusion(context.Background(), &recordingAudit{log: &log}, store, req, true); err != nil || len(log) != 0 {
		t.Fatalf("dry-run lift: err=%v calls=%v, want no calls", err, log)
	}

	audit := &recordingAudit{log: &log}
	if err := liftSourceExclusion(context.Background(), audit, store, req, false); err != nil {
		t.Fatal(err)
	}
	if strings.Join(log, ",") != "audit,lift" || audit.entries[0].Action != "source.exclude.lift" {
		t.Fatalf("call order = %v", log)
	}
	if len(store.rows) != 1 || !store.rows[0].Lifted() || store.rows[0].LiftReason != "venue back" {
		t.Fatalf("row after lift = %+v, want kept and stamped", store.rows)
	}

	log = nil
	if err := liftSourceExclusion(context.Background(), &recordingAudit{log: &log}, store, req, false); err == nil || len(log) != 0 {
		t.Fatalf("second lift: err=%v calls=%v, want refused before any audit", err, log)
	}
}

func TestLiftSourceExclusion_LostRaceSaysAnotherLiftWon(t *testing.T) {
	var log []string
	store := &fakeExclusionStore{log: &log, raceLift: true, rows: []timescale.SourceExclusion{{ID: 1, Source: "binance"}}}
	req := sourceExcludeRequest{mode: modeLift, id: 1, reason: "venue back", actor: "ops"}
	err := liftSourceExclusion(context.Background(), &recordingAudit{log: &log}, store, req, false)
	if !errors.Is(err, timescale.ErrSourceExclusionLifted) || !strings.Contains(err.Error(), "another lift won") {
		t.Fatalf("err = %v, want ErrSourceExclusionLifted saying another lift won", err)
	}
	if store.rows[0].LiftedBy != "other" {
		t.Fatalf("row = %+v, want the winning lift's stamp kept", store.rows[0])
	}
}

func TestCreateSourceExclusion_OverlapRefusedUnlessAllowed(t *testing.T) {
	from := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	existing := []timescale.SourceExclusion{
		{ID: 1, Source: "binance", Base: "crypto:BTC", Quote: "native", From: from, Until: from.Add(time.Hour)},
		{ID: 2, Source: "kraken", From: from},
		{ID: 3, Source: "binance", From: from, LiftedAt: from},
	}
	pairReq := func(base, quote string, f, u time.Time) sourceExcludeRequest {
		r := createReq()
		r.base, r.quote, r.from, r.until = base, quote, f, u
		return r
	}
	cases := []struct {
		name    string
		req     sourceExcludeRequest
		overlap bool
	}{
		{"same pair, same window", pairReq("crypto:BTC", "native", from, time.Time{}), true},
		{"same pair under an XLM alias", pairReq("crypto:BTC", "crypto:XLM", from, time.Time{}), true},
		{"source-wide over a pair row", pairReq("", "", from.Add(30*time.Minute), time.Time{}), true},
		{"other pair", pairReq("crypto:ETH", "native", from, time.Time{}), false},
		{"window after the row's until", pairReq("crypto:BTC", "native", from.Add(time.Hour), time.Time{}), false},
		{"window ending at the row's from", pairReq("crypto:BTC", "native", from.Add(-time.Hour), from), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var log []string
			store := &fakeExclusionStore{log: &log, rows: append([]timescale.SourceExclusion(nil), existing...)}
			err := createSourceExclusion(context.Background(), &recordingAudit{log: &log}, store, tc.req, false)
			if tc.overlap {
				if err == nil || !strings.Contains(err.Error(), "-allow-overlap") || len(log) != 0 {
					t.Fatalf("err = %v, calls = %v; want refused before any write", err, log)
				}
				tc.req.allowOverlap = true
				if err := createSourceExclusion(context.Background(), &recordingAudit{log: &log}, store, tc.req, false); err != nil {
					t.Fatalf("-allow-overlap still refused: %v", err)
				}
			} else if err != nil {
				t.Fatalf("non-overlapping exclusion refused: %v", err)
			}
			if len(store.rows) != len(existing)+1 {
				t.Fatalf("rows = %d, want one inserted", len(store.rows))
			}
		})
	}
}
