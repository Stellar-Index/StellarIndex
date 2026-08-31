package timescale

import (
	"strings"
	"testing"
)

// ValidateAssetsCursor pins the per-shape rejection. Garbage that
// matched the old loose parser (returns 0, "") would fall through
// silently and produce an empty page that looked like end-of-
// pagination — see the package comment on parseAssetCursor.

func TestValidateAssetsCursor_emptyAlwaysOK(t *testing.T) {
	for _, order := range []AssetsOrder{
		AssetsOrderObservationCountDesc,
		AssetsOrderVolume24hUSDDesc,
	} {
		if err := ValidateAssetsCursor("", order); err != nil {
			t.Errorf("empty cursor must be valid for order %v, got %v", order, err)
		}
	}
}

func TestValidateAssetsCursor_obsCountOrder(t *testing.T) {
	cases := []struct {
		in        string
		wantErr   bool
		wantTagIn string // substring expected in error message
	}{
		{"100:USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", false, ""},
		{"0:native", false, ""},
		{"99999999999999:XLM-GABC", false, ""},
		{"missing-colon", true, "separator"},
		{"100:", true, "asset_id"},
		{":native", true, "observation_count"},
		{"abc:native", true, "observation_count"},
		{"-5:native", true, "observation_count"}, // negative not produced; reject
		{"1.5:native", true, "observation_count"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			err := ValidateAssetsCursor(tc.in, AssetsOrderObservationCountDesc)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tc.wantTagIn) {
					t.Errorf("expected error containing %q, got %v", tc.wantTagIn, err)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateAssetsCursor_volumeOrder(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		// Empty volume prefix is what nextAssetCursor emits when the
		// last row had a null vol_usd — must round-trip cleanly.
		{":USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", false},
		{"123.45:native", false},
		{"0:native", false},
		{"1234567890:native", false},
		{"missing-colon", true},
		{"123.45:", true},      // empty asset_id
		{"abc:native", true},   // non-numeric prefix
		{"1.2.3:native", true}, // two dots
		{"-5.0:native", true},  // leading minus
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			err := ValidateAssetsCursor(tc.in, AssetsOrderVolume24hUSDDesc)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// TestValidateAssetsCursor_rejectsShapesThatReachTheDatabase pins the
// wave-D KP-2 cases: cursors that passed validation and then failed —
// or worse, silently degenerated — further down.
//
// The validator's job is to make a malformed cursor a 400 at the
// boundary. Each case below got past it and produced something else: a
// database-level error, or an empty 200 page indistinguishable from
// end-of-pagination.
func TestValidateAssetsCursor_rejectsShapesThatReachTheDatabase(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		cursor string
		order  AssetsOrder
		why    string
	}{
		{
			name:   "lone dot volume prefix",
			cursor: "0:.:USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
			order:  AssetsOrderVolume24hUSDDesc,
			why: "isNumericPrefix required no digit, so '.' validated and was bound " +
				"as $n::numeric, which Postgres rejects",
		},
		{
			name:   "rank tier past int32",
			cursor: "2147483648:100:USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
			order:  AssetsOrderObservationCountDesc,
			why:    "the tier is bound to a $n::int placeholder; a digit-string check alone lets it overflow",
		},
		{
			name:   "observation_count past int64",
			cursor: "0:99999999999999999999:USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
			order:  AssetsOrderObservationCountDesc,
			why: "parseAssetCursor's ParseInt fails and degenerates the cursor to (0,0,\"\"), " +
				"which matches no rows — a silent empty page that looks like the end of the walk",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateAssetsCursor(tc.cursor, tc.order); err == nil {
				t.Errorf("cursor %q accepted; it must be rejected at the boundary — %s",
					tc.cursor, tc.why)
			}
		})
	}
}

// The shapes the listing actually emits must keep validating — a
// tightened validator that rejects a server-emitted cursor would break
// pagination outright, which is worse than the bug it closes.
func TestValidateAssetsCursor_serverEmittedShapesStillValid(t *testing.T) {
	t.Parallel()
	id := "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	for _, tc := range []struct {
		name   string
		cursor string
		order  AssetsOrder
	}{
		{"obs count", "0:4200:" + id, AssetsOrderObservationCountDesc},
		{"flagged tier", "2:17:" + id, AssetsOrderObservationCountDesc},
		{"volume with decimal", "0:1234.56:" + id, AssetsOrderVolume24hUSDDesc},
		{"volume integral", "0:1234:" + id, AssetsOrderVolume24hUSDDesc},
		// A NULL vol_usd row emits an EMPTY volume prefix. This must stay
		// valid: it is a shape the server itself produces.
		{"volume empty (null vol_usd)", "0::" + id, AssetsOrderVolume24hUSDDesc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateAssetsCursor(tc.cursor, tc.order); err != nil {
				t.Errorf("server-emitted cursor %q rejected: %v", tc.cursor, err)
			}
		})
	}
}
