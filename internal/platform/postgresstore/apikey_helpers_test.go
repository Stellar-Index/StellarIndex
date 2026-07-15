package postgresstore

import (
	"database/sql"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// TestCidrArrayValue — the IP-allowlist enforcement path serialises
// a []netip.Prefix into the Postgres cidr[] literal `{a,b}`. This
// value IS the allowlist that gates key use, so a formatting bug
// (missing braces, wrong separator) silently disables or corrupts
// the allowlist. Empty must yield `{}` to match the schema default,
// NOT NULL (the column is NOT NULL).
func TestCidrArrayValue(t *testing.T) {
	cases := []struct {
		name string
		in   []netip.Prefix
		want string
	}{
		{"empty", nil, "{}"},
		{"single", []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, "{10.0.0.0/8}"},
		{
			"multi",
			[]netip.Prefix{
				netip.MustParsePrefix("10.0.0.0/8"),
				netip.MustParsePrefix("192.168.1.0/24"),
			},
			"{10.0.0.0/8,192.168.1.0/24}",
		},
		{"v6", []netip.Prefix{netip.MustParsePrefix("2001:db8::/32")}, "{2001:db8::/32}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ipAllowlistArray(tc.in).Value()
			if err != nil {
				t.Fatalf("Value: %v", err)
			}
			s, ok := got.(string)
			if !ok {
				t.Fatalf("Value returned %T, want string", got)
			}
			if s != tc.want {
				t.Errorf("Value = %q, want %q", s, tc.want)
			}
		})
	}
}

// TestParseCIDRArray — decodes the textual cidr[] Postgres ships
// back. Malformed entries are SKIPPED (not fatal) per the doc, so
// one bad row can't nuke the whole allowlist read. Empty input →
// nil. Also proves a Value()→parse round-trip preserves the set.
func TestParseCIDRArray(t *testing.T) {
	if got := parseCIDRArray(nil); got != nil {
		t.Errorf("parseCIDRArray(nil) = %v, want nil", got)
	}
	if got := parseCIDRArray(pq.StringArray{}); got != nil {
		t.Errorf("parseCIDRArray(empty) = %v, want nil", got)
	}

	// Malformed entries dropped; valid ones kept in order.
	got := parseCIDRArray(pq.StringArray{"10.0.0.0/8", "not-a-cidr", "192.168.1.0/24"})
	want := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("192.168.1.0/24"),
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d (%v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("prefix[%d] = %v, want %v", i, got[i], want[i])
		}
	}

	// Round-trip: parseCIDRArray must reconstruct exactly the set
	// that cidrArray.Value serialised (element-wise, order-preserving).
	orig := []netip.Prefix{
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("2001:db8::/32"),
	}
	back := parseCIDRArray(pq.StringArray{orig[0].String(), orig[1].String()})
	if len(back) != len(orig) {
		t.Fatalf("round-trip len = %d, want %d", len(back), len(orig))
	}
	for i := range orig {
		if back[i] != orig[i] {
			t.Errorf("round-trip[%d] = %v, want %v", i, back[i], orig[i])
		}
	}
}

// TestFinalizeAPIKeyCreate — the Create insert's error contract.
// ErrNoRows means the capped-INSERT's `WHERE n < cap` filtered the
// row (quota met) → ErrAPIKeyQuotaExceeded, NOT a generic 500. A
// 23505 unique violation → ErrConflict. Mapping the wrong sentinel
// would surface a quota-exceeded as a conflict (or vice versa) to
// the dashboard.
func TestFinalizeAPIKeyCreate(t *testing.T) {
	seed := platform.APIKey{ID: "kid_deadbeef", Name: "prod"}

	t.Run("success passes through", func(t *testing.T) {
		got, err := finalizeAPIKeyCreate(seed, nil)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if got.ID != seed.ID {
			t.Errorf("ID = %v, want %v", got.ID, seed.ID)
		}
	})

	t.Run("ErrNoRows maps to quota exceeded", func(t *testing.T) {
		_, err := finalizeAPIKeyCreate(platform.APIKey{}, sql.ErrNoRows)
		if !errors.Is(err, platform.ErrAPIKeyQuotaExceeded) {
			t.Errorf("err = %v, want ErrAPIKeyQuotaExceeded", err)
		}
	})

	t.Run("unique violation maps to conflict", func(t *testing.T) {
		_, err := finalizeAPIKeyCreate(platform.APIKey{}, &pq.Error{Code: "23505"})
		if !errors.Is(err, platform.ErrConflict) {
			t.Errorf("err = %v, want ErrConflict", err)
		}
	})

	t.Run("other pq error passes through wrapped", func(t *testing.T) {
		orig := &pq.Error{Code: "23502"} // not-null violation, not a conflict
		_, err := finalizeAPIKeyCreate(platform.APIKey{}, orig)
		if errors.Is(err, platform.ErrConflict) || errors.Is(err, platform.ErrAPIKeyQuotaExceeded) {
			t.Errorf("err = %v; a non-23505 pq error must not map to a sentinel", err)
		}
		if !errors.Is(err, orig) {
			t.Errorf("err = %v; want the original error preserved via %%w", err)
		}
	})
}

// TestParseUUIDNullString — a NULL / empty / unparseable column
// resolves to uuid.Nil (the "absent" sentinel the store treats as
// unset), never a partial/garbage UUID.
func TestParseUUIDNullString(t *testing.T) {
	if got := parseUUIDNullString(sql.NullString{}); got != uuid.Nil {
		t.Errorf("NULL → %v, want Nil", got)
	}
	if got := parseUUIDNullString(sql.NullString{Valid: true, String: ""}); got != uuid.Nil {
		t.Errorf("empty → %v, want Nil", got)
	}
	if got := parseUUIDNullString(sql.NullString{Valid: true, String: "not-a-uuid"}); got != uuid.Nil {
		t.Errorf("garbage → %v, want Nil", got)
	}
	id := uuid.New()
	if got := parseUUIDNullString(sql.NullString{Valid: true, String: id.String()}); got != id {
		t.Errorf("valid → %v, want %v", got, id)
	}
}

// TestUUIDOrEmpty / TestNullTime — the zero-boundary helpers that
// let the INSERT bind SQL NULL for absent optional values via
// NULLIF. A zero that leaks through as a real value would insert
// the all-zeros UUID / Go's year-1 timestamp (which timestamptz
// rejects).
func TestUUIDOrEmpty(t *testing.T) {
	if got := uuidOrEmpty(uuid.Nil); got != "" {
		t.Errorf("uuidOrEmpty(Nil) = %q, want empty", got)
	}
	id := uuid.New()
	if got := uuidOrEmpty(id); got != id.String() {
		t.Errorf("uuidOrEmpty(id) = %q, want %q", got, id.String())
	}
}

func TestNullTime(t *testing.T) {
	if got := nullTime(time.Time{}); got.Valid {
		t.Errorf("nullTime(zero) valid = true, want false")
	}
	now := time.Now().UTC()
	got := nullTime(now)
	if !got.Valid || !got.Time.Equal(now) {
		t.Errorf("nullTime(now) = %+v, want valid=%v", got, now)
	}
}
