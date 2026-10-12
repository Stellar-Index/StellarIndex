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

// Pins that the movements feed never trusts a SAC/asset label from the attacker-influenceable CAP-67 sep0011 topic as
// trusted identity: sep41_transfers come from ANY token contract, so a hostile one can claim USDC.
// resolveSEP41MovementAsset re-derives the SAC from (code, issuer) and trusts it only if it equals contract_id.

// sacSpoofReader drives the trusted-instance path (SACClassicAssetName) and the
// attacker-influenceable event path (SACAssetFromEvents) independently. SACAssetFromEvents returns
// the claim verbatim regardless of contract id, as a real attacker's contract would emit it.
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

// End-to-end at the render seam: the mapped movement row's Asset field (what the wire carries) does not present the
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
