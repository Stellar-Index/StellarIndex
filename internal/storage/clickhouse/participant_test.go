package clickhouse

import (
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// The three fixed accounts used across the derivation tests. Real ed25519
// strkeys so xdrjson.ParticipantAccounts' strkey validation passes.
const (
	testSource  = "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF" // op source
	testDest    = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA" // counterparty
	testIssuer  = "GCEZWKCA5VLDNRLN3RPRJMRZOX3Z6G5CHCGSNFHEYVXM3XOJMDS674JZ" // an asset issuer
	testTrustor = "GDRXE2BQUC3AZNPVFSCEZ76NJ3WWL25FYFK6RGZGIEKWE4SOOHSUJUJ6" // trustor / from
)

// opBody marshals an operation body of the given type + value to the base64 the
// lake stores in stellar.operations.body_xdr.
func opBody(t *testing.T, typ xdr.OperationType, value interface{}) string {
	t.Helper()
	body, err := xdr.NewOperationBody(typ, value)
	if err != nil {
		t.Fatalf("NewOperationBody(%v): %v", typ, err)
	}
	b64, err := xdr.MarshalBase64(body)
	if err != nil {
		t.Fatalf("MarshalBase64(%v): %v", typ, err)
	}
	return b64
}

func accounts(rows []OperationParticipantRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Account
	}
	return out
}

func mustAssetCode4(t *testing.T, code string) xdr.AssetCode {
	t.Helper()
	var raw xdr.AssetCode4
	copy(raw[:], code)
	ac, err := xdr.NewAssetCode(xdr.AssetTypeAssetTypeCreditAlphanum4, raw)
	if err != nil {
		t.Fatalf("NewAssetCode: %v", err)
	}
	return ac
}

// TestOperationParticipantRows_ByOpType asserts the NON-source participant set
// the shared derivation (used by BOTH the live extractor and the backfill)
// extracts per op type. The op's own source is always excluded.
func TestOperationParticipantRows_ByOpType(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string // expected non-source participant accounts (order = sorted)
	}{
		{
			name: "payment → destination",
			body: opBody(t, xdr.OperationTypePayment, xdr.PaymentOp{
				Destination: xdr.MustMuxedAddress(testDest),
				Asset:       xdr.MustNewNativeAsset(),
				Amount:      10,
			}),
			want: []string{testDest},
		},
		{
			name: "create_account → destination",
			body: opBody(t, xdr.OperationTypeCreateAccount, xdr.CreateAccountOp{
				Destination:     xdr.MustAddress(testDest),
				StartingBalance: 20_000_000,
			}),
			want: []string{testDest},
		},
		{
			name: "account_merge → destination",
			body: opBody(t, xdr.OperationTypeAccountMerge, xdr.MustMuxedAddress(testDest)),
			want: []string{testDest},
		},
		{
			name: "path_payment_strict_send → destination",
			body: opBody(t, xdr.OperationTypePathPaymentStrictSend, xdr.PathPaymentStrictSendOp{
				SendAsset:   xdr.MustNewNativeAsset(),
				SendAmount:  5,
				Destination: xdr.MustMuxedAddress(testDest),
				DestAsset:   xdr.MustNewCreditAsset("USDC", testIssuer),
				DestMin:     1,
			}),
			// The dest_asset issuer renders as "USDC-ISSUER" (not a bare
			// strkey) so it is NOT a participant — only the destination is.
			want: []string{testDest},
		},
		{
			name: "allow_trust → trustor",
			body: opBody(t, xdr.OperationTypeAllowTrust, xdr.AllowTrustOp{
				Trustor:   xdr.MustAddress(testTrustor),
				Asset:     mustAssetCode4(t, "USDC"),
				Authorize: 1,
			}),
			want: []string{testTrustor},
		},
		{
			name: "set_trust_line_flags → trustor",
			body: opBody(t, xdr.OperationTypeSetTrustLineFlags, xdr.SetTrustLineFlagsOp{
				Trustor:  xdr.MustAddress(testTrustor),
				Asset:    xdr.MustNewCreditAsset("USDC", testIssuer),
				SetFlags: 1,
			}),
			want: []string{testTrustor},
		},
		{
			name: "clawback → from",
			body: opBody(t, xdr.OperationTypeClawback, xdr.ClawbackOp{
				Asset:  xdr.MustNewCreditAsset("USDC", testIssuer),
				From:   xdr.MustMuxedAddress(testTrustor),
				Amount: 3,
			}),
			want: []string{testTrustor},
		},
		{
			name: "change_trust → none (issuer is asset-embedded, not a bare account)",
			body: opBody(t, xdr.OperationTypeChangeTrust, xdr.ChangeTrustOp{
				Line:  xdr.MustNewCreditAsset("USDC", testIssuer).ToChangeTrustAsset(),
				Limit: 100,
			}),
			want: nil,
		},
		{
			name: "manage_sell_offer → none (only assets, no counterparty account)",
			body: opBody(t, xdr.OperationTypeManageSellOffer, xdr.ManageSellOfferOp{
				Selling: xdr.MustNewCreditAsset("USDC", testIssuer),
				Buying:  xdr.MustNewNativeAsset(),
				Amount:  1,
				Price:   xdr.Price{N: 1, D: 1},
			}),
			want: nil,
		},
		{
			name: "bump_sequence → none",
			body: opBody(t, xdr.OperationTypeBumpSequence, xdr.BumpSequenceOp{BumpTo: 42}),
			want: nil,
		},
	}

	closeTime := time.Unix(1_700_000_000, 0).UTC()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := operationParticipantRows(tc.body, testSource, 100, closeTime, "abc123", 7, 2)
			if err != nil {
				t.Fatalf("operationParticipantRows: %v", err)
			}
			got := accounts(rows)
			if !equalStrings(got, tc.want) {
				t.Fatalf("participants = %v, want %v", got, tc.want)
			}
			// Field threading is uniform across rows — spot-check the first.
			if len(rows) > 0 {
				r := rows[0]
				if r.LedgerSeq != 100 || r.TxHash != "abc123" || r.TxIndex != 7 || r.OpIndex != 2 || !r.CloseTime.Equal(closeTime) {
					t.Errorf("row context mis-threaded: %+v", r)
				}
			}
		})
	}
}

// TestOperationParticipantRows_SkipsSource proves the op's own source account is
// never emitted — the reader covers it via operations.source_account, and a
// duplicate would double-count the op for its source (the AccountOperations
// UNION relies on the sourced-XOR-participant invariant).
func TestOperationParticipantRows_SkipsSource(t *testing.T) {
	// A self-payment: source pays itself. The destination equals the source.
	body := opBody(t, xdr.OperationTypePayment, xdr.PaymentOp{
		Destination: xdr.MustMuxedAddress(testSource),
		Asset:       xdr.MustNewNativeAsset(),
		Amount:      1,
	})
	rows, err := operationParticipantRows(body, testSource, 100, time.Unix(0, 0).UTC(), "h", 0, 0)
	if err != nil {
		t.Fatalf("operationParticipantRows: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("self-op participants = %v, want none (source excluded)", accounts(rows))
	}
}

// TestOperationParticipantRows_MuxedDestResolvesToG proves a muxed (M-)
// destination resolves to its underlying G — that's the account whose received
// activity we index.
func TestOperationParticipantRows_MuxedDestResolvesToG(t *testing.T) {
	// M-address whose underlying ed25519 is testDest (id 0).
	muxed := xdr.MuxedAccount{
		Type: xdr.CryptoKeyTypeKeyTypeMuxedEd25519,
		Med25519: &xdr.MuxedAccountMed25519{
			Id:      1234,
			Ed25519: *xdr.MustAddress(testDest).Ed25519,
		},
	}
	body := opBody(t, xdr.OperationTypePayment, xdr.PaymentOp{
		Destination: muxed,
		Asset:       xdr.MustNewNativeAsset(),
		Amount:      1,
	})
	rows, err := operationParticipantRows(body, testSource, 1, time.Unix(0, 0).UTC(), "h", 0, 0)
	if err != nil {
		t.Fatalf("operationParticipantRows: %v", err)
	}
	got := accounts(rows)
	if !equalStrings(got, []string{testDest}) {
		t.Fatalf("muxed dest resolved to %v, want [%s]", got, testDest)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
