package accounts

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/accounterasure"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
	"github.com/Stellar-Index/StellarIndex/internal/platform/postgresstore"
)

type fakeAccountEraser struct {
	erased     []uuid.UUID
	actor      platform.ActorKind
	finish     []string
	planned    []uuid.UUID
	erasedSlug bool
}

func (f *fakeAccountEraser) Erase(_ context.Context, id uuid.UUID, actor platform.ActorKind) (accounterasure.Report, error) {
	f.erased, f.actor = append(f.erased, id), actor
	return accounterasure.Report{Counts: postgresstore.ErasureCounts{Users: 2}}, nil
}

func (f *fakeAccountEraser) FinishBySlug(_ context.Context, slug string) (accounterasure.Report, error) {
	f.finish = append(f.finish, slug)
	return accounterasure.Report{}, nil
}

func (f *fakeAccountEraser) PlanErasure(_ context.Context, id uuid.UUID) (postgresstore.ErasurePlan, error) {
	f.planned = append(f.planned, id)
	return postgresstore.ErasurePlan{AccountID: id, Emails: []string{"owner@acme.example"}, UserIDs: []uuid.UUID{uuid.New()}}, nil
}

func (f *fakeAccountEraser) SlugErased(context.Context, string) (bool, error) {
	return f.erasedSlug, nil
}

// TestAccountErase_DryRunWritesNothingAndPrintsNoAddress — the default
// pass only plans, and its output never carries an email.
func TestAccountErase_DryRunWritesNothingAndPrintsNoAddress(t *testing.T) {
	f := &fakeAccountEraser{}
	var out bytes.Buffer
	id := uuid.New()
	if err := runAccountEraseWith(context.Background(), &out, f, f, id, "", true); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(f.erased) != 0 || len(f.planned) != 1 {
		t.Errorf("dry run erased %d / planned %d, want 0 / 1", len(f.erased), len(f.planned))
	}
	if strings.Contains(out.String(), "@") {
		t.Errorf("output carries an address: %s", out.String())
	}
}

// TestAccountErase_WriteErasesAsStaff — an operator erasure is recorded
// as a staff action.
func TestAccountErase_WriteErasesAsStaff(t *testing.T) {
	f := &fakeAccountEraser{}
	var out bytes.Buffer
	id := uuid.New()
	if err := runAccountEraseWith(context.Background(), &out, f, f, id, "", false); err != nil {
		t.Fatalf("erase: %v", err)
	}
	if len(f.erased) != 1 || f.erased[0] != id || f.actor != platform.ActorStaff {
		t.Errorf("erased %v as %q, want %s as staff", f.erased, f.actor, id)
	}
}

// TestAccountErase_FinishDryRunRefusesALiveSlug — the dry run says what
// -write would refuse.
func TestAccountErase_FinishDryRunRefusesALiveSlug(t *testing.T) {
	f := &fakeAccountEraser{}
	if err := runAccountEraseWith(context.Background(), &bytes.Buffer{}, f, f, uuid.Nil, "acme", true); err == nil {
		t.Error("dry-run -finish-slug accepted a slug that was never erased")
	}
	f.erasedSlug = true
	if err := runAccountEraseWith(context.Background(), &bytes.Buffer{}, f, f, uuid.Nil, "acme", false); err != nil || len(f.finish) != 1 {
		t.Errorf("finish = %v, calls %v; want one FinishBySlug", err, f.finish)
	}
}

// TestAccountErase_FlagValidation — exactly one target.
func TestAccountErase_FlagValidation(t *testing.T) {
	for _, args := range [][]string{
		{"-config", "x.toml"},
		{"-config", "x.toml", "-account-id", uuid.NewString(), "-finish-slug", "a"},
		{"-config", "x.toml", "-account-id", "not-a-uuid"},
	} {
		if err := Erase(args); err == nil {
			t.Errorf("Erase(%v) accepted", args)
		}
	}
}
