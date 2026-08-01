package explorer

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/classicmovements"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// These tests pin W2-explorer-1 (MED security, audit-2026-08-01): the
// public, unauthenticated /v1/accounts/{g}/movements feed must NOT
// render a SAC/asset label taken from the attacker-influenceable CAP-67
// sep0011 event topic as a trusted identity. sep41_transfers are
// ingested from ANY token contract (not identity-gated), so a hostile
// non-SAC token can emit a 4-topic transfer whose trailing sep0011
// topic claims a trusted asset (e.g. Circle USDC) and — absent a
// cross-check — impersonate that identity on a victim's movements.
//
// The fix routes the event-topic fallback in resolveSEP41MovementAsset
// through the SAME derivation cross-check the sibling wasm_view.go uses
// (sacAssetViaEvents): re-derive the SAC address from the claimed asset
// and only trust the label when it equals the on-chain contract_id.
//
// Circle USDC's real issuer is used only as a recognizable label; the
// spoof case attaches that claim to an UNRELATED contract id, the
// legit case attaches it to the genuine derived SAC address.

// sacSpoofReader is an ExplorerReader whose SAC-resolution seams return
// configured values, so the resolver's trusted-instance path
// (SACClassicAssetName) and attacker-influenceable event path
// (SACAssetFromEvents) can be driven independently. SACAssetFromEvents
// returns the claim verbatim regardless of contract id — mirroring a
// real attacker's contract genuinely emitting the spoofed topic.
type sacSpoofReader struct {
	*capReader
	classicName    string // SACClassicAssetName result; "" => not found (forces event path)
	eventClaimName string // SACAssetFromEvents result, returned verbatim; "" => not found
}

func (r *sacSpoofReader) SACClassicAssetName(context.Context, string) (string, bool, error) {
	if r.classicName == "" {
		return "", false, nil
	}
	return r.classicName, true, nil
}

func (r *sacSpoofReader) SACAssetFromEvents(context.Context, string) (string, bool, error) {
	if r.eventClaimName == "" {
		return "", false, nil
	}
	return r.eventClaimName, true, nil
}

// TestResolveSEP41MovementAsset_SpoofedEventLabelRejected: a claim that
// does NOT match the derived SAC address for the claimed asset must not
// render as that asset — it falls back to the raw contract id.
func TestResolveSEP41MovementAsset_SpoofedEventLabelRejected(t *testing.T) {
	claim := "USDC:" + validTestAccount // colon form, exactly as the CAP-67 topic carries it
	reader := &sacSpoofReader{
		capReader:      &capReader{probe: &deadlineProbe{}},
		eventClaimName: claim,
	}
	h := newProbeHandler(reader, nil)

	// validTestContract is NOT the deterministic SAC address for
	// USDC-<validTestAccount>; a hostile token emitting the spoofed
	// topic must never render as Circle USDC.
	got := h.resolveSEP41MovementAsset(context.Background(), validTestContract)

	if got == "USDC:"+validTestAccount || got == "USDC-"+validTestAccount {
		t.Fatalf("spoofed SAC label rendered as trusted identity %q — SacContractID cross-check missing (W2-explorer-1)", got)
	}
	if got != validTestContract {
		t.Fatalf("unverifiable claim should fall back to the raw contract_id %q, got %q", validTestContract, got)
	}
}

// TestResolveSEP41MovementAsset_LegitimateSACRendersTrusted: a genuine
// SAC — whose on-chain contract id DOES equal the derived address for
// its claimed asset — still renders the trusted, canonical label.
func TestResolveSEP41MovementAsset_LegitimateSACRendersTrusted(t *testing.T) {
	asset, err := canonical.NewClassicAsset("USDC", validTestAccount)
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	sac, err := asset.SacContractID()
	if err != nil {
		t.Fatalf("SacContractID: %v", err)
	}

	reader := &sacSpoofReader{
		capReader:      &capReader{probe: &deadlineProbe{}},
		eventClaimName: "USDC:" + validTestAccount,
	}
	h := newProbeHandler(reader, nil)

	got := h.resolveSEP41MovementAsset(context.Background(), sac)
	want := asset.String() // canonical dash form: "USDC-<validTestAccount>"
	if got != want {
		t.Fatalf("legitimate SAC (derived address matches claim) should render trusted label %q, got %q", want, got)
	}
}

// TestMapSEP41RowsToMovements_SpoofedAssetNotRenderedOnWire proves the
// property end-to-end at the render seam: the mapped movement row's
// Asset field (what the wire response carries) does NOT present the
// spoofed trusted identity.
func TestMapSEP41RowsToMovements_SpoofedAssetNotRenderedOnWire(t *testing.T) {
	reader := &sacSpoofReader{
		capReader:      &capReader{probe: &deadlineProbe{}},
		eventClaimName: "USDC:" + validTestAccount,
	}
	h := newProbeHandler(reader, nil)

	rows := []timescale.SEP41TransferRow{{
		ContractID: validTestContract, // hostile non-SAC token
		Ledger:     classicmovements.P23StartLedger + 10,
		TxHash:     validTestTxHash,
		ObservedAt: time.Unix(1_700_000_000, 0).UTC(),
		FromAddr:   "GCOUNTERPARTYXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX2",
		ToAddr:     validTestAccount,
		Amount:     big.NewInt(1),
	}}

	out := h.mapSEP41RowsToMovements(context.Background(), validTestAccount, rows, "")
	if len(out) != 1 {
		t.Fatalf("want exactly 1 mapped movement, got %d", len(out))
	}
	if out[0].Asset == "USDC:"+validTestAccount || out[0].Asset == "USDC-"+validTestAccount {
		t.Fatalf("wire movement asset %q impersonates Circle USDC — spoof leaked to the public feed (W2-explorer-1)", out[0].Asset)
	}
	if out[0].Asset != validTestContract {
		t.Fatalf("spoofed movement should render the raw contract_id %q, got %q", validTestContract, out[0].Asset)
	}
}
