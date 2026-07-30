package clickhouse

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// TestAccountEntryKeyPrefix pins the PK-prefix construction: the prefix
// must be a true string prefix of the account's real trustline/offer
// LedgerKey base64 (prefix-stability at the 39-byte / 52-char boundary),
// and must differ between entry types and between accounts.
func TestAccountEntryKeyPrefix(t *testing.T) {
	const g = "GA3GJGKCUKPOPL6NYPMSBK7LMFYNW7SJMAJ7ZGWR3KGSHJWJHQRQZA3L"

	tp, err := accountEntryKeyPrefix(g, xdr.LedgerEntryTypeTrustline)
	if err != nil {
		t.Fatal(err)
	}
	if len(tp) != 52 {
		t.Fatalf("prefix length = %d chars, want 52 (39 bytes, base64 3-byte-aligned)", len(tp))
	}
	// Build a REAL trustline key for this account and assert prefix-ness.
	var aid xdr.AccountId
	if err := aid.SetAddress(g); err != nil {
		t.Fatal(err)
	}
	lk := xdr.LedgerKey{
		Type: xdr.LedgerEntryTypeTrustline,
		TrustLine: &xdr.LedgerKeyTrustLine{
			AccountId: aid,
			Asset:     xdr.TrustLineAsset{Type: xdr.AssetTypeAssetTypeNative},
		},
	}
	full, err := xdr.MarshalBase64(lk)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(full, tp) {
		t.Fatalf("real trustline key %q does not start with prefix %q", full, tp)
	}

	op, err := accountEntryKeyPrefix(g, xdr.LedgerEntryTypeOffer)
	if err != nil {
		t.Fatal(err)
	}
	if op == tp {
		t.Fatal("offer and trustline prefixes must differ (discriminant byte)")
	}
	// Round-trip sanity: the decoded prefix's first 4 bytes are the type.
	raw, err := base64.StdEncoding.DecodeString(op)
	if err != nil {
		t.Fatal(err)
	}
	if raw[3] != byte(xdr.LedgerEntryTypeOffer) {
		t.Fatalf("prefix discriminant = %d, want %d", raw[3], xdr.LedgerEntryTypeOffer)
	}

	other, err := accountEntryKeyPrefix("GAFEDMD42RJVK4TWOV7QXFTBPJOU7HIKS4DBNKZYUNFUEK24AW26ZJJ5", xdr.LedgerEntryTypeTrustline)
	if err != nil {
		t.Fatal(err)
	}
	if other == tp {
		t.Fatal("two different accounts produced the same prefix")
	}
}
