package v1_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubPositionsReader is a canned explorerpkg.PositionsReader
// (structural — this file doesn't import that package, only satisfies
// its method set, same convention as stubSEP41MovementsReader in
// explorer_movements_test.go).
type stubPositionsReader struct {
	blend       []timescale.BlendPositionFold
	blendErr    error
	backstop    []timescale.BlendBackstopFold
	backstopErr error
	phoenix     []timescale.PhoenixStakeFold
	phoenixErr  error
	defindex    []timescale.DefindexVaultFold
	defindexErr error
	credit      []timescale.CreditPositionFold
	creditErr   error
	aquarius    []timescale.AquariusGaugeFold
	aquariusErr error
}

func (s *stubPositionsReader) BlendPositionsByUser(context.Context, string) ([]timescale.BlendPositionFold, error) {
	return s.blend, s.blendErr
}

func (s *stubPositionsReader) BlendBackstopSharesByUser(context.Context, string) ([]timescale.BlendBackstopFold, error) {
	return s.backstop, s.backstopErr
}

func (s *stubPositionsReader) PhoenixStakeByUser(context.Context, string) ([]timescale.PhoenixStakeFold, error) {
	return s.phoenix, s.phoenixErr
}

func (s *stubPositionsReader) DefindexVaultSharesByUser(context.Context, string) ([]timescale.DefindexVaultFold, error) {
	return s.defindex, s.defindexErr
}

func (s *stubPositionsReader) CreditPositionsByOwner(context.Context, string) ([]timescale.CreditPositionFold, error) {
	return s.credit, s.creditErr
}

func (s *stubPositionsReader) AquariusGaugeByUser(context.Context, string) ([]timescale.AquariusGaugeFold, error) {
	return s.aquarius, s.aquariusErr
}

// stubPoolTokensReader is a canned explorerpkg.PoolTokensReader.
type stubPoolTokensReader struct {
	bySource map[string]map[string][]string
}

func (s *stubPoolTokensReader) PoolTokens(_ context.Context, source string) (map[string][]string, error) {
	return s.bySource[source], nil
}

// TestExplorer_AccountPositions_Fold pins the per-protocol fold ->
// wire-shape mapping: one blend supply leg, one blend borrow leg (same
// pool/asset — independent positions), one backstop position, one
// phoenix stake, one defindex vault, one open + one closed sorocredit
// position, and one aquarius gauge (negative net delta, the "unwound"
// case) — net-zero / closed positions excluded by default, included
// with ?include_closed=true.
func TestExplorer_AccountPositions_Fold(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	opened := now.Add(-72 * time.Hour)

	reader := &stubPositionsReader{
		blend: []timescale.BlendPositionFold{
			{
				Pool: "CPOOL1", Asset: "CUSDC1",
				HasSupplyLeg: true, SupplyNet: "5000000", SupplyLastActivity: now, SupplyLastLedger: 100,
				HasBorrowLeg: true, BorrowNet: "1000000", BorrowLastActivity: now, BorrowLastLedger: 101,
			},
			{
				// Fully unwound supply leg (net zero) — excluded by default.
				Pool: "CPOOL2", Asset: "CXLM1",
				HasSupplyLeg: true, SupplyNet: "0", SupplyLastActivity: now, SupplyLastLedger: 90,
			},
		},
		backstop: []timescale.BlendBackstopFold{
			{Pool: "CPOOL1", SharesNet: "250000", LastActivity: now, LastLedger: 102},
		},
		phoenix: []timescale.PhoenixStakeFold{
			{StakeContract: "CSTAKE1", LPToken: "CLPTOKEN1", NetAmount: "300000", LastActivity: now, LastLedger: 103},
		},
		defindex: []timescale.DefindexVaultFold{
			{ContractID: "CVAULT1", SharesNet: "400000", LastActivity: now, LastLedger: 104},
		},
		credit: []timescale.CreditPositionFold{
			{
				CollateralContract: "CCOLLAT1", PositionUUID: "uuid-open", OpenedAt: opened, OpenedLedger: 50,
				LatestAmount: "900000", LatestActivity: now, LatestLedger: 105, Withdrawn: false,
			},
			{
				CollateralContract: "CCOLLAT2", PositionUUID: "uuid-closed", OpenedAt: opened, OpenedLedger: 51,
				LatestAmount: "0", LatestActivity: now, LatestLedger: 106, Withdrawn: true,
			},
		},
		aquarius: []timescale.AquariusGaugeFold{
			{ContractID: "CGAUGEPOOL1", NetDelta: "-500", LastActivity: now, LastLedger: 107},
		},
	}
	poolTokens := &stubPoolTokensReader{bySource: map[string]map[string][]string{
		"blend":    {"CPOOL1": {"CUSDC1", "CXLM1"}},
		"aquarius": {"CGAUGEPOOL1": {"CXLM1", "CUSDC1"}},
	}}

	srv := v1.New(v1.Options{Positions: reader, ProtocolPoolTokens: poolTokens})
	base := httpTestServer(t, srv).URL

	resp := mustGet(t, base+"/v1/accounts/"+testG+"/positions")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data v1.AccountPositionsView `json:"data"`
	}
	mustDecode(t, resp, &body)

	if body.Data.Account != testG {
		t.Errorf("account = %q, want %q", body.Data.Account, testG)
	}
	if body.Data.Note == "" {
		t.Error("note should always be present (valuation/accrual honesty note)")
	}
	if body.Data.IncludeClosed {
		t.Error("include_closed should default false")
	}

	// 7 non-zero/non-closed positions expected: blend supply, blend
	// borrow, backstop, phoenix stake, defindex vault, sorocredit-open,
	// aquarius gauge. The zero-net blend CPOOL2 supply leg and the
	// withdrawn sorocredit position are excluded.
	if len(body.Data.Positions) != 7 {
		t.Fatalf("positions = %d, want 7: %+v", len(body.Data.Positions), body.Data.Positions)
	}

	byKindVenue := map[string]v1.PositionEntry{}
	for _, p := range body.Data.Positions {
		byKindVenue[p.Protocol+"/"+p.PositionKind+"/"+p.Venue] = p
	}

	supply, ok := byKindVenue["blend/lending_supply/CPOOL1"]
	if !ok {
		t.Fatal("missing blend lending_supply position")
	}
	if supply.Amount != "5000000" || supply.AmountSemantics != "net_underlying_at_event_time" || supply.Basis != "event_derived" {
		t.Errorf("blend supply = %+v", supply)
	}
	if supply.LastActivity.Ledger != 100 || supply.LastActivity.Time == "" {
		t.Errorf("blend supply last_activity = %+v", supply.LastActivity)
	}
	if supply.VenueLabel != "CUSDC1/CXLM1" && supply.VenueLabel != "CXLM1/CUSDC1" {
		// resolveSEP41MovementAsset falls back to the raw contract id
		// in this test (no ClickHouse/SAC reader wired), so labels are
		// the raw contract ids themselves — just confirm one was set.
		if supply.VenueLabel == "" {
			t.Errorf("blend supply venue_label = %q, want a pool_tokens-derived label", supply.VenueLabel)
		}
	}

	borrow, ok := byKindVenue["blend/lending_borrow/CPOOL1"]
	if !ok || borrow.Amount != "1000000" {
		t.Errorf("blend lending_borrow = %+v (ok=%v)", borrow, ok)
	}

	if _, ok := byKindVenue["blend/lending_supply/CPOOL2"]; ok {
		t.Error("zero-net blend CPOOL2 supply leg should be excluded by default")
	}

	backstop, ok := byKindVenue["blend/backstop_shares/CPOOL1"]
	if !ok || backstop.Amount != "250000" || backstop.AmountSemantics != "shares" {
		t.Errorf("blend backstop = %+v (ok=%v)", backstop, ok)
	}
	if len(backstop.Assets) != 1 || backstop.Assets[0] == "" {
		t.Errorf("blend backstop assets = %v, want the documented backstop-token label", backstop.Assets)
	}

	stake, ok := byKindVenue["phoenix/stake/CSTAKE1"]
	if !ok || stake.Amount != "300000" || stake.AmountSemantics != "shares" {
		t.Errorf("phoenix stake = %+v (ok=%v)", stake, ok)
	}

	vault, ok := byKindVenue["defindex/vault_shares/CVAULT1"]
	if !ok || vault.Amount != "400000" || vault.AmountSemantics != "shares" {
		t.Errorf("defindex vault = %+v (ok=%v)", vault, ok)
	}

	credit, ok := byKindVenue["sorocredit/credit/CCOLLAT1"]
	if !ok || credit.Amount != "900000" || credit.AmountSemantics != "stateful_current" || credit.Basis != "stateful" {
		t.Errorf("sorocredit open position = %+v (ok=%v)", credit, ok)
	}
	if len(credit.Assets) != 1 || credit.Assets[0] != "USDC" {
		t.Errorf("sorocredit assets = %v, want [USDC]", credit.Assets)
	}
	if _, ok := byKindVenue["sorocredit/credit/CCOLLAT2"]; ok {
		t.Error("withdrawn sorocredit position should be excluded by default")
	}

	gauge, ok := byKindVenue["aquarius/gauge/CGAUGEPOOL1"]
	if !ok || gauge.Amount != "-500" || gauge.AmountSemantics != "signed_delta_sum_unconfirmed_unit" {
		t.Errorf("aquarius gauge = %+v (ok=%v)", gauge, ok)
	}

	// ?include_closed=true surfaces the two excluded rows too.
	resp = mustGet(t, base+"/v1/accounts/"+testG+"/positions?include_closed=true")
	mustDecode(t, resp, &body)
	if !body.Data.IncludeClosed {
		t.Error("include_closed should echo true")
	}
	if len(body.Data.Positions) != 9 {
		t.Fatalf("include_closed=true positions = %d, want 9: %+v", len(body.Data.Positions), body.Data.Positions)
	}
}

// TestExplorer_AccountPositions_ValidationAndUnavailable covers the
// error paths: invalid strkey and the 503 when no positions reader is
// wired.
func TestExplorer_AccountPositions_ValidationAndUnavailable(t *testing.T) {
	srv := v1.New(v1.Options{Positions: &stubPositionsReader{}})
	base := httpTestServer(t, srv).URL

	if r := mustGet(t, base+"/v1/accounts/notanaccount/positions"); r.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid account: status = %d, want 400", r.StatusCode)
	}

	unavailable := v1.New(v1.Options{})
	ubase := httpTestServer(t, unavailable).URL
	if r := mustGet(t, ubase+"/v1/accounts/"+testG+"/positions"); r.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("no positions reader: status = %d, want 503", r.StatusCode)
	}
}

// TestExplorer_AccountPositions_EmptyIsHonest pins the empty-state
// contract: an address with no folds anywhere returns 200 with an
// empty positions array (never a 404 / never an error), and the
// valuation/accrual note is always present even when there's nothing
// to annotate.
func TestExplorer_AccountPositions_EmptyIsHonest(t *testing.T) {
	srv := v1.New(v1.Options{Positions: &stubPositionsReader{}})
	base := httpTestServer(t, srv).URL

	resp := mustGet(t, base+"/v1/accounts/"+testG+"/positions")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data v1.AccountPositionsView `json:"data"`
	}
	mustDecode(t, resp, &body)
	if len(body.Data.Positions) != 0 {
		t.Errorf("positions = %+v, want empty", body.Data.Positions)
	}
	if body.Data.Note == "" {
		t.Error("note should be present even on an empty result")
	}
}
