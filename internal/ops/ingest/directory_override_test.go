package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fakeDirectoryOverrideStore applies the takeover with the real tag
// filter, so the tags asserted below are the ones the store would write.
type fakeDirectoryOverrideStore struct {
	rows     map[string]timescale.DirectoryEntry
	cleared  []string
	reasons  []string
	actors   []string
	deleted  []string
	audits   []platform.AuditEntry
	auditErr error
	// unaudited counts mutations that ran with no audit row appended before them.
	unaudited int
}

func (f *fakeDirectoryOverrideStore) Append(_ context.Context, e platform.AuditEntry) error {
	if f.auditErr != nil {
		return f.auditErr
	}
	f.audits = append(f.audits, e)
	return nil
}

func (f *fakeDirectoryOverrideStore) mutated() {
	if len(f.audits) <= len(f.cleared)+len(f.deleted)-1 {
		f.unaudited++
	}
}

func (f *fakeDirectoryOverrideStore) DirectoryEntryByAddress(_ context.Context, a string) (timescale.DirectoryEntry, bool, error) {
	e, ok := f.rows[a]
	return e, ok, nil
}

func (f *fakeDirectoryOverrideStore) ClearDirectoryScamFlag(_ context.Context, a, operator, reason string) (before, after timescale.DirectoryEntry, found bool, err error) {
	f.cleared = append(f.cleared, a)
	f.mutated()
	f.reasons = append(f.reasons, reason)
	f.actors = append(f.actors, operator)
	before = f.rows[a]
	after = before
	after.Tags = timescale.DirectoryTagsWithoutScamFlags(before.Tags)
	after.Source = timescale.DirectoryOperatorOverrideSource
	f.rows[a] = after
	return before, after, true, nil
}

func (f *fakeDirectoryOverrideStore) DeleteDirectoryOverride(_ context.Context, a string) (bool, error) {
	f.deleted = append(f.deleted, a)
	f.mutated()
	delete(f.rows, a)
	return true, nil
}

var overrideIssuer = "G" + strings.Repeat("B", 55)

const (
	overrideReason = "issuer verified via its stellar.toml; upstream tag is a false positive"
	overrideActor  = "ops-oncall"
)

func newOverrideFake(source string, tags ...string) *fakeDirectoryOverrideStore {
	return &fakeDirectoryOverrideStore{rows: map[string]timescale.DirectoryEntry{
		overrideIssuer: {Address: overrideIssuer, Name: "Rio Issuer", Domain: "example.org", Tags: tags, Source: source},
	}}
}

func TestDirectoryOverride_ClearKeepsRecognitionTags(t *testing.T) {
	f := newOverrideFake("stellar-expert", "issuer", "unsafe", "memo-required")
	var out bytes.Buffer
	req := directoryOverrideRequest{address: overrideIssuer, clearScamFlag: true, reason: overrideReason, actor: overrideActor, write: true}
	if err := runDirectoryOverride(context.Background(), f, f, &out, req); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if !slices.Equal(f.cleared, []string{overrideIssuer}) {
		t.Fatalf("ClearDirectoryScamFlag calls = %v, want exactly one for %s", f.cleared, overrideIssuer)
	}
	if !slices.Equal(f.reasons, []string{overrideReason}) {
		t.Errorf("reason handed to the store = %q, want %q", f.reasons, overrideReason)
	}
	if !slices.Equal(f.actors, []string{overrideActor}) {
		t.Errorf("operator handed to the store = %q, want %q — the override must record who lifted the flag", f.actors, overrideActor)
	}
	got := f.rows[overrideIssuer]
	if want := []string{"issuer", "memo-required"}; !slices.Equal(got.Tags, want) {
		t.Errorf("tags after override = %q, want %q (issuer is what the RWA funnel reads)", got.Tags, want)
	}
	if got.Source != timescale.DirectoryOperatorOverrideSource {
		t.Errorf("source after override = %q, want %q", got.Source, timescale.DirectoryOperatorOverrideSource)
	}
}

func TestDirectoryOverride_DryRunWritesNothing(t *testing.T) {
	f := newOverrideFake("stellar-expert", "issuer", "unsafe")
	var out bytes.Buffer
	if err := runDirectoryOverride(context.Background(), f, f, &out,
		directoryOverrideRequest{address: overrideIssuer, clearScamFlag: true, reason: overrideReason, actor: overrideActor}); err != nil {
		t.Fatalf("dry-run clear: %v", err)
	}
	if len(f.cleared) != 0 || len(f.deleted) != 0 {
		t.Fatalf("dry run wrote: cleared=%v deleted=%v", f.cleared, f.deleted)
	}
	if !strings.Contains(out.String(), "[issuer unsafe] → [issuer]") {
		t.Errorf("dry-run preview does not show the tag change: %q", out.String())
	}
}

func TestDirectoryOverride_Refusals(t *testing.T) {
	cases := []struct {
		name string
		f    *fakeDirectoryOverrideStore
		req  directoryOverrideRequest
		want error
	}{
		{
			"unflagged row", newOverrideFake("stellar-expert", "issuer"),
			directoryOverrideRequest{address: overrideIssuer, clearScamFlag: true, reason: overrideReason, actor: overrideActor, write: true},
			timescale.ErrDirectoryNotScamFlagged,
		},
		{
			"clear without a reason", newOverrideFake("stellar-expert", "unsafe"),
			directoryOverrideRequest{address: overrideIssuer, clearScamFlag: true, actor: overrideActor, write: true},
			nil,
		},
		{
			"clear with a blank reason", newOverrideFake("stellar-expert", "unsafe"),
			directoryOverrideRequest{address: overrideIssuer, clearScamFlag: true, reason: " \t", actor: overrideActor, write: true},
			nil,
		},
		{
			"clear without an operator", newOverrideFake("stellar-expert", "unsafe"),
			directoryOverrideRequest{address: overrideIssuer, clearScamFlag: true, reason: overrideReason, write: true},
			nil,
		},
		{
			"clear with a blank operator", newOverrideFake("stellar-expert", "unsafe"),
			directoryOverrideRequest{address: overrideIssuer, clearScamFlag: true, reason: overrideReason, actor: " ", write: true},
			nil,
		},
		{
			"delete an upstream row", newOverrideFake("stellar-expert", "unsafe"),
			directoryOverrideRequest{address: overrideIssuer, remove: true, reason: overrideReason, actor: overrideActor, write: true},
			nil,
		},
		{
			"delete without a reason", newOverrideFake(timescale.DirectoryOperatorOverrideSource, "issuer"),
			directoryOverrideRequest{address: overrideIssuer, remove: true, actor: overrideActor, write: true},
			nil,
		},
		{
			"delete without an operator", newOverrideFake(timescale.DirectoryOperatorOverrideSource, "issuer"),
			directoryOverrideRequest{address: overrideIssuer, remove: true, reason: overrideReason, write: true},
			nil,
		},
		{
			"no action", newOverrideFake("stellar-expert", "unsafe"),
			directoryOverrideRequest{address: overrideIssuer, write: true},
			nil,
		},
		{
			"both actions", newOverrideFake("stellar-expert", "unsafe"),
			directoryOverrideRequest{address: overrideIssuer, clearScamFlag: true, reason: overrideReason, actor: overrideActor, remove: true, write: true},
			nil,
		},
		{
			"malformed address", newOverrideFake("stellar-expert", "unsafe"),
			directoryOverrideRequest{address: "GABC", clearScamFlag: true, reason: overrideReason, actor: overrideActor, write: true},
			nil,
		},
		{
			"no row", newOverrideFake("stellar-expert", "unsafe"),
			directoryOverrideRequest{address: "G" + strings.Repeat("C", 55), clearScamFlag: true, reason: overrideReason, actor: overrideActor, write: true},
			nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runDirectoryOverride(context.Background(), tc.f, tc.f, &bytes.Buffer{}, tc.req)
			if err == nil {
				t.Fatal("got nil error, want a refusal")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if len(tc.f.cleared) != 0 || len(tc.f.deleted) != 0 {
				t.Errorf("a refusal wrote: cleared=%v deleted=%v", tc.f.cleared, tc.f.deleted)
			}
		})
	}
}

func TestDirectoryOverride_DeleteOverride(t *testing.T) {
	f := newOverrideFake(timescale.DirectoryOperatorOverrideSource, "issuer")
	if err := runDirectoryOverride(context.Background(), f, f, &bytes.Buffer{},
		directoryOverrideRequest{address: overrideIssuer, remove: true, reason: overrideReason, actor: overrideActor, write: true}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !slices.Equal(f.deleted, []string{overrideIssuer}) {
		t.Fatalf("DeleteDirectoryOverride calls = %v, want exactly one for %s", f.deleted, overrideIssuer)
	}
}

// TestDirectoryOverride_AuditsEveryWriteFirst: each write appends one staff audit_log row naming the action, the address, who and
// why before it mutates, and an audit failure changes nothing.
func TestDirectoryOverride_AuditsEveryWriteFirst(t *testing.T) {
	cases := []struct {
		name, action string
		f            *fakeDirectoryOverrideStore
		req          directoryOverrideRequest
	}{
		{
			"clear", "directory.clear_scam_flag", newOverrideFake("stellar-expert", "issuer", "unsafe"),
			directoryOverrideRequest{address: overrideIssuer, clearScamFlag: true, reason: overrideReason, actor: overrideActor, write: true},
		},
		{
			"delete", "directory.delete_override", newOverrideFake(timescale.DirectoryOperatorOverrideSource, "issuer"),
			directoryOverrideRequest{address: overrideIssuer, remove: true, reason: overrideReason, actor: overrideActor, write: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := runDirectoryOverride(context.Background(), tc.f, tc.f, &bytes.Buffer{}, tc.req); err != nil {
				t.Fatal(err)
			}
			if len(tc.f.audits) != 1 || tc.f.unaudited != 0 {
				t.Fatalf("audit rows = %d, mutations before an audit row = %d; want 1 and 0", len(tc.f.audits), tc.f.unaudited)
			}
			e := tc.f.audits[0]
			var meta map[string]any
			if err := json.Unmarshal(e.Metadata, &meta); err != nil {
				t.Fatal(err)
			}
			if e.ActorKind != platform.ActorStaff || e.Action != tc.action || e.TargetKind != "account_directory" || e.TargetID != overrideIssuer ||
				meta["actor"] != overrideActor || meta["reason"] != overrideReason {
				t.Errorf("audit row = %+v meta %v; want staff %s on account_directory/%s by %q for %q", e, meta, tc.action, overrideIssuer, overrideActor, overrideReason)
			}

			failing := newOverrideFake("stellar-expert", "issuer", "unsafe")
			if tc.req.remove {
				failing = newOverrideFake(timescale.DirectoryOperatorOverrideSource, "issuer")
			}
			failing.auditErr = errors.New("audit_log unavailable")
			if err := runDirectoryOverride(context.Background(), failing, failing, &bytes.Buffer{}, tc.req); err == nil {
				t.Fatal("audit failure: got nil, want a refusal")
			}
			if len(failing.cleared) != 0 || len(failing.deleted) != 0 {
				t.Errorf("audit failure still wrote: cleared=%v deleted=%v", failing.cleared, failing.deleted)
			}
		})
	}
}

// writeIngestConfig drops a minimal, otherwise-valid stellarindex.toml
// carrying a KNOWN-GOOD postgres DSN, so a bare config.Load() accepts the
// file and only an APPLIED env override can make config load fail.
func writeIngestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "stellarindex.toml")
	body := `
[storage]
postgres_dsn = "postgres://good:good@localhost/stellarindex?sslmode=disable"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
