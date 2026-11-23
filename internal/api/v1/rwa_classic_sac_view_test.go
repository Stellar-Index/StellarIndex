// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// lateCatalogueStub answers its first issuer page empty, as a catalogue that has not yet indexed the member's row.
type lateCatalogueStub struct {
	*rwaListStub
	hide atomic.Int32
}

func (l *lateCatalogueStub) ListAssetsExt(ctx context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	if l.hide.Add(-1) >= 0 {
		return nil, nil
	}
	return l.rwaListStub.ListAssetsExt(ctx, opts)
}

// TestRWA_ClassicRowCataloguedAfterMembershipIsNotServedTwice: the membership's SAC dedupe read the issuer page before the
// catalogue held the member's row, so it kept the SAC as a contract member; the view then finds the classic row. The one
// supply must be one row and one value, and the classic row (the asset's only other row) must stay.
func TestRWA_ClassicRowCataloguedAfterMembershipIsNotServedTwice(t *testing.T) {
	bound, dir, rows := oneBoundMember()
	sac, err := canonical.Asset{Type: canonical.AssetClassic, Code: "USTRY", Issuer: rwaGoodIssuer}.SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	srv := rwaClassicSACServer(bound, dir, rows, sac)
	late := &lateCatalogueStub{rwaListStub: &rwaListStub{
		stubAssetsReaderExt: &stubAssetsReaderExt{}, byIssuer: rows, supply: rwaSupplyFor(rows),
	}}
	late.hide.Store(1)
	srv.AssetsReader = late
	view := getRWA(t, srv)

	classic, contracts := 0, 0
	for _, a := range view.Assets {
		if a.ContractID == sac {
			t.Errorf("the classic member's SAC is served beside its classic row: %+v", a)
		}
		if a.ContractID != "" {
			contracts++
		} else if a.Code == "USTRY" && a.Issuer == rwaGoodIssuer {
			classic++
		}
	}
	if classic != 1 || contracts != 1 {
		t.Fatalf("classic USTRY rows = %d, contract rows = %d; want 1 and the unrelated contract", classic, contracts)
	}
	if !view.Funnel.Balanced {
		t.Fatalf("funnel does not balance:\n%s", funnelDump(view))
	}
	assertContractDrop(t, view, "contract_candidates_evaluated", "contract_is_classic_member_sac", 1)
}
