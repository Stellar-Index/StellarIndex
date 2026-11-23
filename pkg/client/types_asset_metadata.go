// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package client

// AssetMetadata is the data shape of [Client.AssetMetadata] (the SEP-1 overlay); overlay fields populate only when Sep1Status == "verified" and are nil otherwise.
type AssetMetadata struct {
	AssetID    string  `json:"asset_id"`
	HomeDomain *string `json:"home_domain,omitempty"`
	Sep1Status string  `json:"sep1_status"`

	// SEP-1 [[CURRENCIES]] overlay — populated only on Sep1Status=="verified".
	Name            *string `json:"name,omitempty"`
	Description     *string `json:"description,omitempty"`
	Image           *string `json:"image,omitempty"`
	OrgName         *string `json:"org_name,omitempty"`
	AnchorAsset     *string `json:"anchor_asset,omitempty"`
	AnchorAssetType *string `json:"anchor_asset_type,omitempty"`

	// SEP-1 standing and backing declarations; issuer-declared, nil unless Sep1Status == "verified".
	CurrencyStatus         *string `json:"currency_status,omitempty"`
	IsAssetAnchored        *bool   `json:"is_asset_anchored,omitempty"`
	AttestationOfReserve   *string `json:"attestation_of_reserve,omitempty"`
	RedemptionInstructions *string `json:"redemption_instructions,omitempty"`
	Regulated              *bool   `json:"regulated,omitempty"`
	ApprovalServer         *string `json:"approval_server,omitempty"`
	ApprovalCriteria       *string `json:"approval_criteria,omitempty"`

	// SEP-1 issuance declarations — issuer-declared, distinct from
	// the F2 fields on AssetDetail which observe live ledger state.
	Conditions  *string `json:"conditions,omitempty"`
	FixedNumber *string `json:"fixed_number,omitempty"`
	MaxNumber   *string `json:"max_number,omitempty"`
	IsUnlimited *bool   `json:"is_unlimited,omitempty"`
}
