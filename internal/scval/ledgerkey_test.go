package scval

import (
	"errors"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// Real r1 fixture: the RedStone adapter's per-feed contract-data key at
// ledger 62056824 — ContractData{CA526Y2N…, ScString("USTRY"), persistent}.
const testContractDataKeyB64 = "AAAABgAAAAE7r2NNhY1q1h+JSvMDLKe8+WE7oLTJ1BXmafczoXPOOwAAAA4AAAAFVVNUUlkAAAAAAAAB"

func TestParseContractDataKey_RealFixture(t *testing.T) {
	contractID, key, err := ParseContractDataKey(testContractDataKeyB64)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if contractID != "CA526Y2NQWGWVVQ7RFFPGAZMU66PSYJ3UC2MTVAV4ZU7OM5BOPHDXUSG" {
		t.Errorf("contract = %s, want the RedStone adapter", contractID)
	}
	s, err := AsString(key)
	if err != nil {
		t.Fatalf("key AsString: %v", err)
	}
	if s != "USTRY" {
		t.Errorf("key = %q, want USTRY", s)
	}
}

func TestParseContractDataKey_Rejections(t *testing.T) {
	if _, _, err := ParseContractDataKey("!!not-base64"); !errors.Is(err, ErrScValDecode) {
		t.Errorf("garbage: err = %v, want ErrScValDecode", err)
	}

	// A non-contract-data LedgerKey (account) is a type error, not a
	// decode error — callers filtering mixed key lists skip this class.
	acct := xdr.MustAddress("GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF")
	accountKey, merr := xdr.MarshalBase64(xdr.LedgerKey{
		Type:    xdr.LedgerEntryTypeAccount,
		Account: &xdr.LedgerKeyAccount{AccountId: acct},
	})
	if merr != nil {
		t.Fatal(merr)
	}
	if _, _, err := ParseContractDataKey(accountKey); !errors.Is(err, ErrScValType) {
		t.Errorf("account key: err = %v, want ErrScValType", err)
	}
}
