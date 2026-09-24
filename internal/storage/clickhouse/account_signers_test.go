package clickhouse

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/xdr"
)

func accountEntryStub(t *testing.T, entryXDR string) *stubConn {
	t.Helper()
	return &stubConn{respond: func(q string) (driver.Rows, error) {
		if strings.Contains(q, "entry_type = 'account'") {
			return &stubRows{data: [][]any{{entryXDR, "updated", int64(10_000_000), uint32(77)}}}, nil
		}
		return &stubRows{}, nil
	}}
}

// AccountSigners reads the signing configuration from the entry alone and
// never touches trustlines or offers.
func TestAccountSignersReadsEntryOnly(t *testing.T) {
	account := keypair.MustRandom().Address()
	cosigner := keypair.MustRandom().Address()
	acc := xdr.AccountEntry{
		AccountId:  xdr.MustAddress(account),
		Thresholds: xdr.Thresholds{0, 1, 2, 3},
		Signers:    []xdr.Signer{{Key: xdr.MustSigner(cosigner), Weight: 2}},
	}
	conn := accountEntryStub(t, mustEntryB64(t, xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeAccount, Account: &acc}))
	r := &ExplorerReader{conn: conn}

	st, err := r.AccountSigners(context.Background(), account)
	if err != nil {
		t.Fatalf("AccountSigners: %v", err)
	}
	if !st.Exists || st.MasterWeight != 0 || st.ThreshMed != 2 {
		t.Fatalf("state = exists %v master %d med %d, want true 0 2", st.Exists, st.MasterWeight, st.ThreshMed)
	}
	if len(st.Signers) != 1 || st.Signers[0].Key != cosigner || st.Signers[0].Weight != 2 {
		t.Fatalf("signers = %+v, want [%s weight 2]", st.Signers, cosigner)
	}
	if len(conn.queries) != 1 {
		t.Fatalf("issued %d queries, want only the account-entry lookup", len(conn.queries))
	}
}

// A corrupt stored entry is an error for the signer lookup (auth must not
// read it as "no account") and still the empty state for the explorer page.
func TestCorruptAccountEntryIsAnErrorForSignersOnly(t *testing.T) {
	account := keypair.MustRandom().Address()

	_, err := (&ExplorerReader{conn: accountEntryStub(t, "not-xdr")}).AccountSigners(context.Background(), account)
	if !errors.Is(err, errCorruptAccountEntry) {
		t.Fatalf("AccountSigners err = %v, want errCorruptAccountEntry", err)
	}

	st, err := (&ExplorerReader{conn: accountEntryStub(t, "not-xdr")}).AccountState(context.Background(), account)
	if err != nil || st.Exists {
		t.Fatalf("AccountState = exists %v err %v, want the empty state and no error", st.Exists, err)
	}
}
