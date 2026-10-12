package dispatcher

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

const testPassphrase = "Test SDF Network ; September 2015"

// emptyLedgerCloseMeta builds a minimal LedgerCloseMetaV1 with zero transactions to exercise reader construction
// without full fixture assembly.
func emptyLedgerCloseMeta(t *testing.T, seq uint32) xdr.LedgerCloseMeta {
	t.Helper()
	return xdr.LedgerCloseMeta{
		V: 1,
		V1: &xdr.LedgerCloseMetaV1{
			LedgerHeader: xdr.LedgerHeaderHistoryEntry{
				Header: xdr.LedgerHeader{
					LedgerSeq: xdr.Uint32(seq),
				},
			},
			TxSet: xdr.GeneralizedTransactionSet{
				V:       1,
				V1TxSet: &xdr.TransactionSetV1{},
			},
		},
	}
}
