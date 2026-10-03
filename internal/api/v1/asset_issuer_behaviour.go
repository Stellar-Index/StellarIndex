package v1

import (
	"context"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// issuerFlowReadTimeout bounds the supply_flows sum the detail page waits
// on; a slower scan omits the totals rather than delaying the response.
const issuerFlowReadTimeout = 2 * time.Second

// AssetIssuerBehaviour is what a classic asset's issuer can do to holders
// (its live account flags) and what it has done to supply (the asset's
// Stellar Asset Contract mint/burn/clawback log). Every classic mint, burn
// and clawback is issuer-originated by protocol.
type AssetIssuerBehaviour struct {
	AuthRequired        *bool   `json:"auth_required,omitempty"`
	AuthRevocable       *bool   `json:"auth_revocable,omitempty"`
	AuthClawbackEnabled *bool   `json:"auth_clawback_enabled,omitempty"`
	AuthImmutable       *bool   `json:"auth_immutable,omitempty"`
	FlagsAsOfLedger     *uint32 `json:"flags_as_of_ledger,omitempty"`
	// Totals are integer strings in the asset's smallest unit (ADR-0003).
	MintTotal       *string `json:"mint_total,omitempty"`
	BurnTotal       *string `json:"burn_total,omitempty"`
	ClawbackTotal   *string `json:"clawback_total,omitempty"`
	SupplyFlowCount *uint64 `json:"supply_flow_count,omitempty"`
}

// Issuer-behaviour risk names carried in [AssetGlobalMarket.IssuerSignals].
const (
	issuerSignalClawbackEnabled  = "auth_clawback_enabled"
	issuerSignalRevocable        = "auth_revocable"
	issuerSignalClawbackObserved = "clawback_observed"
)

// riskSignals lists the behaviours that let the issuer move holders'
// balances, in a fixed order. Nil-safe.
func (b *AssetIssuerBehaviour) riskSignals() []string {
	if b == nil {
		return nil
	}
	var out []string
	if b.AuthClawbackEnabled != nil && *b.AuthClawbackEnabled {
		out = append(out, issuerSignalClawbackEnabled)
	}
	if b.AuthRevocable != nil && *b.AuthRevocable {
		out = append(out, issuerSignalRevocable)
	}
	if b.ClawbackTotal != nil && *b.ClawbackTotal != "0" {
		out = append(out, issuerSignalClawbackObserved)
	}
	return out
}

// applyIssuerBehaviour attaches the issuer's live flags and the asset's
// lifetime supply flows to a classic asset's detail. Best-effort: a part
// that does not resolve is omitted, never reported as false or zero.
func (s *Server) applyIssuerBehaviour(ctx context.Context, row *AssetDetail) {
	a, err := canonical.ParseAsset(row.AssetID)
	if err != nil || a.Type != canonical.AssetClassic {
		return
	}
	var b AssetIssuerBehaviour
	if f, ok := s.liveIssuerAccount(ctx, a.Issuer); ok {
		req, rev, claw, imm := f.Required, f.Revocable, f.Clawback, f.Immutable
		b.AuthRequired, b.AuthRevocable, b.AuthClawbackEnabled, b.AuthImmutable = &req, &rev, &claw, &imm
		if f.AsOfLedger > 0 {
			asOf := f.AsOfLedger
			b.FlagsAsOfLedger = &asOf
		}
	}
	s.fillIssuerFlowTotals(ctx, row.AssetID, &b)
	if b == (AssetIssuerBehaviour{}) {
		return
	}
	row.IssuerBehaviour = &b
}

// fillIssuerFlowTotals reads Σmint, Σburn and Σclawback over the asset's
// SAC. An incompletely seeded log (burns exceed mints) is omitted, as
// /v1/assets/{id}/supply refuses it.
func (s *Server) fillIssuerFlowTotals(ctx context.Context, assetID string, b *AssetIssuerBehaviour) {
	if s.tokenSupply == nil {
		return
	}
	contractID, ok := classicSACContractID(assetID)
	if !ok {
		return
	}
	fctx, cancel := context.WithTimeout(ctx, issuerFlowReadTimeout)
	defer cancel()
	sup, err := s.tokenSupply.TokenSupply(fctx, contractID)
	if err != nil || sup.Incomplete || sup.FlowCount == 0 || sup.Mint == nil || sup.Burn == nil || sup.Clawback == nil {
		return
	}
	mint, burn, claw, n := sup.Mint.String(), sup.Burn.String(), sup.Clawback.String(), sup.FlowCount
	b.MintTotal, b.BurnTotal, b.ClawbackTotal, b.SupplyFlowCount = &mint, &burn, &claw, &n
}
