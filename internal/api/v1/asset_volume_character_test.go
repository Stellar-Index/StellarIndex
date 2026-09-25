package v1_test

// §2 volume_character — the trailing-window account-structure overlay on
// /v1/assets/{id}. Asserts the derived character + signals surface, and
// that this analytics field is orthogonal to pricing (it must not re-rank
// or suppress anything — §4 is out of scope).

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type stubVolumeCharacterReader struct {
	vc       timescale.AssetVolumeCharacter
	notFound bool
	err      error
}

func (s *stubVolumeCharacterReader) AssetVolumeCharacterRollup(_ context.Context, _ string) (timescale.AssetVolumeCharacter, bool, error) {
	return s.vc, !s.notFound, s.err
}

// scamAUDVolumeCharacter is the concentrated verdict the census produces
// for the reported scam AUD.
func scamAUDVolumeCharacter() timescale.AssetVolumeCharacter {
	return timescale.AssetVolumeCharacter{
		WindowDays:             14,
		VolumeUSD:              "2870000",
		DistinctMakers:         2,
		DistinctTakers:         1,
		TopAccountPairVolShare: 0.99,
		SelfCrossShare:         0,
		IssuerSideShare:        0.99,
		MarketStyledShare:      1.0,
		IsMarketStyled:         true,
		Character:              timescale.VolumeCharacterConcentrated,
	}
}

func TestAssetGet_VolumeCharacter_Surfaced(t *testing.T) {
	stub := &stubVolumeCharacterReader{vc: scamAUDVolumeCharacter()}
	srv, aud := audDetailServer(t, v1.Options{VolumeCharacter: stub})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/assets/"+aud.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var env struct {
		Data v1.AssetDetail `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	d := env.Data
	if d.VolumeCharacter != "concentrated" {
		t.Errorf("volume_character = %q, want concentrated", d.VolumeCharacter)
	}
	if d.VolumeCharacterSignals == nil {
		t.Fatalf("volume_character_signals missing")
	}
	sig := d.VolumeCharacterSignals
	if sig.TopAccountPairVolShare != 0.99 {
		t.Errorf("top_account_pair_vol_share = %v, want 0.99", sig.TopAccountPairVolShare)
	}
	if !sig.IsMarketStyled {
		t.Errorf("is_market_styled = false, want true")
	}
	if sig.DistinctMakers != 2 || sig.DistinctTakers != 1 {
		t.Errorf("distinct makers/takers = %d/%d, want 2/1", sig.DistinctMakers, sig.DistinctTakers)
	}
	if sig.WindowDays != 14 {
		t.Errorf("window_days = %d, want 14", sig.WindowDays)
	}
	if sig.VolumeUSD != "2870000.00" {
		t.Errorf("volume_usd = %q, want 2870000.00", sig.VolumeUSD)
	}
}

// TestAssetGet_VolumeCharacter_VolumeUSDExact: volume_usd is rendered to
// 2dp from the rollup's NUMERIC text in big.Rat (ADR-0003). A float hop
// loses the cents above 2^53 cents and rounds the exact half-cent 1234.125
// to even ("1234.12"); an unparseable value omits the signals.
func TestAssetGet_VolumeCharacter_VolumeUSDExact(t *testing.T) {
	cases := []struct {
		name, stored, want string
	}{
		{"above_2^53_cents", "90071992547409.93", "90071992547409.93"},
		{"half_cent_rounds_away_from_zero", "1234.125", "1234.13"},
		{"extra_scale_truncated_to_cents", "5000.00449999", "5000.00"},
		{"unparseable_omits_signals", "NaN", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vc := scamAUDVolumeCharacter()
			vc.VolumeUSD = c.stored
			srv, aud := audDetailServer(t, v1.Options{VolumeCharacter: &stubVolumeCharacterReader{vc: vc}})
			ts := httpTestServer(t, srv)
			resp := mustGet(t, ts.URL+"/v1/assets/"+aud.String())
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			var env struct {
				Data v1.AssetDetail `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			sig := env.Data.VolumeCharacterSignals
			if c.want == "" {
				if sig != nil || env.Data.VolumeCharacter != "" {
					t.Errorf("unparseable volume_usd served signals %+v / %q, want omitted", sig, env.Data.VolumeCharacter)
				}
				return
			}
			if sig == nil {
				t.Fatalf("volume_character_signals missing")
			}
			if sig.VolumeUSD != c.want {
				t.Errorf("volume_usd = %q, want %q", sig.VolumeUSD, c.want)
			}
		})
	}
}

// TestAssetGet_VolumeCharacter_DoesNotAffectPrice — volume_character is
// analytics-only: a `concentrated` verdict must not suppress or move the
// asset's price. price_usd is byte-identical with and without the reader,
// and carries the real market price either way.
func TestAssetGet_VolumeCharacter_DoesNotAffectPrice(t *testing.T) {
	get := func(t *testing.T, opts v1.Options) v1.AssetDetail {
		t.Helper()
		srv, aud := audDetailServer(t, opts)
		ts := httpTestServer(t, srv)
		resp := mustGet(t, ts.URL+"/v1/assets/"+aud.String())
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		var env struct {
			Data v1.AssetDetail `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return env.Data
	}

	withVC := get(t, v1.Options{VolumeCharacter: &stubVolumeCharacterReader{vc: scamAUDVolumeCharacter()}})
	withoutVC := get(t, v1.Options{})

	if withVC.VolumeCharacter != "concentrated" {
		t.Fatalf("precondition: expected concentrated with the reader wired, got %q", withVC.VolumeCharacter)
	}
	if withoutVC.VolumeCharacter != "" {
		t.Fatalf("precondition: no reader wired must omit volume_character, got %q", withoutVC.VolumeCharacter)
	}
	if withVC.PriceUSD == nil || *withVC.PriceUSD != "0.65" {
		t.Errorf("price_usd (concentrated) = %v, want 0.65 — analytics must not suppress pricing", withVC.PriceUSD)
	}
	if withoutVC.PriceUSD == nil || *withoutVC.PriceUSD != "0.65" {
		t.Errorf("price_usd (no reader) = %v, want 0.65", withoutVC.PriceUSD)
	}
}
