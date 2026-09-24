package ingest

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fakeDirectoryOverrideStore applies the takeover with the real tag
// filter, so the tags asserted below are the ones the store would write.
type fakeDirectoryOverrideStore struct {
	rows    map[string]timescale.DirectoryEntry
	cleared []string
	reasons []string
	deleted []string
}

func (f *fakeDirectoryOverrideStore) DirectoryEntryByAddress(_ context.Context, a string) (timescale.DirectoryEntry, bool, error) {
	e, ok := f.rows[a]
	return e, ok, nil
}

func (f *fakeDirectoryOverrideStore) ClearDirectoryScamFlag(_ context.Context, a, reason string) (before, after timescale.DirectoryEntry, found bool, err error) {
	f.cleared = append(f.cleared, a)
	f.reasons = append(f.reasons, reason)
	before = f.rows[a]
	after = before
	after.Tags = timescale.DirectoryTagsWithoutScamFlags(before.Tags)
	after.Source = timescale.DirectoryOperatorOverrideSource
	f.rows[a] = after
	return before, after, true, nil
}

func (f *fakeDirectoryOverrideStore) DeleteDirectoryOverride(_ context.Context, a string) (bool, error) {
	f.deleted = append(f.deleted, a)
	delete(f.rows, a)
	return true, nil
}

var overrideIssuer = "G" + strings.Repeat("B", 55)

const overrideReason = "issuer verified via its stellar.toml; upstream tag is a false positive"

func newOverrideFake(source string, tags ...string) *fakeDirectoryOverrideStore {
	return &fakeDirectoryOverrideStore{rows: map[string]timescale.DirectoryEntry{
		overrideIssuer: {Address: overrideIssuer, Name: "Rio Issuer", Domain: "example.org", Tags: tags, Source: source},
	}}
}

func TestDirectoryOverride_ClearKeepsRecognitionTags(t *testing.T) {
	f := newOverrideFake("stellar-expert", "issuer", "unsafe", "memo-required")
	var out bytes.Buffer
	req := directoryOverrideRequest{address: overrideIssuer, clearScamFlag: true, reason: overrideReason, write: true}
	if err := runDirectoryOverride(context.Background(), f, &out, req); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if !slices.Equal(f.cleared, []string{overrideIssuer}) {
		t.Fatalf("ClearDirectoryScamFlag calls = %v, want exactly one for %s", f.cleared, overrideIssuer)
	}
	if !slices.Equal(f.reasons, []string{overrideReason}) {
		t.Errorf("reason handed to the store = %q, want %q", f.reasons, overrideReason)
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
	if err := runDirectoryOverride(context.Background(), f, &out,
		directoryOverrideRequest{address: overrideIssuer, clearScamFlag: true, reason: overrideReason}); err != nil {
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
			directoryOverrideRequest{address: overrideIssuer, clearScamFlag: true, reason: overrideReason, write: true},
			timescale.ErrDirectoryNotScamFlagged,
		},
		{
			"clear without a reason", newOverrideFake("stellar-expert", "unsafe"),
			directoryOverrideRequest{address: overrideIssuer, clearScamFlag: true, write: true},
			nil,
		},
		{
			"clear with a blank reason", newOverrideFake("stellar-expert", "unsafe"),
			directoryOverrideRequest{address: overrideIssuer, clearScamFlag: true, reason: " \t", write: true},
			nil,
		},
		{
			"delete an upstream row", newOverrideFake("stellar-expert", "unsafe"),
			directoryOverrideRequest{address: overrideIssuer, remove: true, write: true},
			nil,
		},
		{
			"no action", newOverrideFake("stellar-expert", "unsafe"),
			directoryOverrideRequest{address: overrideIssuer, write: true},
			nil,
		},
		{
			"both actions", newOverrideFake("stellar-expert", "unsafe"),
			directoryOverrideRequest{address: overrideIssuer, clearScamFlag: true, reason: overrideReason, remove: true, write: true},
			nil,
		},
		{
			"malformed address", newOverrideFake("stellar-expert", "unsafe"),
			directoryOverrideRequest{address: "GABC", clearScamFlag: true, reason: overrideReason, write: true},
			nil,
		},
		{
			"no row", newOverrideFake("stellar-expert", "unsafe"),
			directoryOverrideRequest{address: "G" + strings.Repeat("C", 55), clearScamFlag: true, reason: overrideReason, write: true},
			nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runDirectoryOverride(context.Background(), tc.f, &bytes.Buffer{}, tc.req)
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
	if err := runDirectoryOverride(context.Background(), f, &bytes.Buffer{},
		directoryOverrideRequest{address: overrideIssuer, remove: true, write: true}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !slices.Equal(f.deleted, []string{overrideIssuer}) {
		t.Fatalf("DeleteDirectoryOverride calls = %v, want exactly one for %s", f.deleted, overrideIssuer)
	}
}
