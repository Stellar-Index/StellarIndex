package incident

import (
	"context"
	"errors"
	"flag"
	"strings"
	"testing"
	"time"

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

type fakeExclusionStore struct {
	rows []timescale.SourceExclusion
	log  *[]string
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
		if f.rows[i].ID == id {
			f.rows[i].LiftedAt, f.rows[i].LiftedBy, f.rows[i].LiftReason = time.Now(), by, reason
			return nil
		}
	}
	return timescale.ErrNotFound
}

func (f *fakeExclusionStore) ActiveSourceExclusions(context.Context) ([]timescale.SourceExclusion, error) {
	var out []timescale.SourceExclusion
	for _, r := range f.rows {
		if r.Active() {
			out = append(out, r)
		}
	}
	return out, nil
}

func resolveArgs(t *testing.T, args ...string) (sourceExcludeRequest, error) {
	t.Helper()
	fs := flag.NewFlagSet("source-exclude", flag.ContinueOnError)
	f := registerSourceExcludeFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return f.resolve()
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
	if req, err := resolveArgs(t, "-list"); err != nil || req.mode != modeList {
		t.Fatalf("-list = %+v, %v; want list mode with no reason required", req, err)
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
	if strings.Join(log, ",") != "audit,insert" {
		t.Fatalf("call order = %v, want audit then insert", log)
	}
	if got := audit.entries[0].Action; got != "source.exclude" {
		t.Fatalf("audit action = %q", got)
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
	if !store.rows[0].Active() || strings.Join(log, ",") != "audit" {
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
	if len(store.rows) != 1 || store.rows[0].Active() || store.rows[0].LiftReason != "venue back" {
		t.Fatalf("row after lift = %+v, want kept and stamped", store.rows)
	}

	log = nil
	if err := liftSourceExclusion(context.Background(), &recordingAudit{log: &log}, store, req, false); err == nil || len(log) != 0 {
		t.Fatalf("second lift: err=%v calls=%v, want refused before any audit", err, log)
	}
}
