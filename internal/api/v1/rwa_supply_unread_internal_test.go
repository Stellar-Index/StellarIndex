package v1

import (
	"context"
	"errors"
	"testing"
)

// TestRWAFillMissingSupplyReportsAnUnreadSupplyAsUnread: an unpriced RWA row left without supply because the lake and the
// trustline sum both failed must report the read as unanswered, so the view is flagged degraded instead of serving the gap
// as "this asset has no supply".
func TestRWAFillMissingSupplyReportsAnUnreadSupplyAsUnread(t *testing.T) {
	s, stub := lakeSupplyServer(t, "")
	stub.err = errors.New("lake sum exceeded its budget")
	s.Explorer = failingClassicExplorer{}
	rows := cetesRow()
	rows[0].PriceUSD = nil

	unread := s.rwaFillMissingSupply(context.Background(), rows)

	if rows[0].CirculatingSupply != nil {
		t.Fatalf("circulating_supply = %s, want none: no reader answered", *rows[0].CirculatingSupply)
	}
	if !unread {
		t.Error("supply left absent by two failed reads was reported as answered; the view would cache a " +
			"supply_unavailable refusal for the full TTL with flags.degraded=false")
	}
}

// TestRWAFillMissingSupplyLakeFigureWithoutAFloorIsUnread: a lake net stamped while the trustline floor that bounds an
// under-seeded lake went unread is unchecked, as the listing reports it; the fill must say so.
func TestRWAFillMissingSupplyLakeFigureWithoutAFloorIsUnread(t *testing.T) {
	s, _ := lakeSupplyServer(t, "50000000000")
	s.Explorer = failingClassicExplorer{}
	rows := cetesRow()
	rows[0].PriceUSD = nil

	unread := s.rwaFillMissingSupply(context.Background(), rows)

	if rows[0].SupplyBasis == nil || *rows[0].SupplyBasis != "classic_lake_flows" {
		t.Fatalf("supply_basis = %v, want classic_lake_flows", rows[0].SupplyBasis)
	}
	if !unread {
		t.Error("a lake figure with the trustline floor unread was served as answered")
	}
}

// TestRWAFillMissingSupplyAnsweredEmptyIsNotUnread is the control: both readers answered and neither holds the asset.
func TestRWAFillMissingSupplyAnsweredEmptyIsNotUnread(t *testing.T) {
	s, _ := lakeSupplyServer(t, "")
	s.Explorer = &trustlineOnlyExplorer{supply: map[string]string{"OTHER-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN": "1"}}
	rows := cetesRow()
	rows[0].PriceUSD = nil

	if s.rwaFillMissingSupply(context.Background(), rows) {
		t.Error("an answered read with no reading was reported as an outage")
	}
	if rows[0].CirculatingSupply != nil {
		t.Fatalf("circulating_supply = %s, want none", *rows[0].CirculatingSupply)
	}
}
