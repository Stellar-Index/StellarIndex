package clickhouse

import (
	"context"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// feeBumpTx builds a fee bump the way core records it: the payer (muxed, to
// prove it is demuxed like source_account) bids outerFee around an inner tx
// that bid 100, and the result carries the inner hash and inner failure.
func feeBumpTx(t *testing.T, payerAccount string, outerFee int64, innerHash xdr.Hash) ingest.LedgerTransaction {
	t.Helper()
	payerID := xdr.MustAddress(payerAccount)
	muxed := xdr.MuxedAccount{
		Type:     xdr.CryptoKeyTypeKeyTypeMuxedEd25519,
		Med25519: &xdr.MuxedAccountMed25519{Id: 7, Ed25519: *payerID.Ed25519},
	}
	return ingest.LedgerTransaction{
		Index: 1,
		Envelope: xdr.TransactionEnvelope{
			Type: xdr.EnvelopeTypeEnvelopeTypeTxFeeBump,
			FeeBump: &xdr.FeeBumpTransactionEnvelope{
				Tx: xdr.FeeBumpTransaction{
					FeeSource: muxed,
					Fee:       xdr.Int64(outerFee),
					InnerTx: xdr.FeeBumpTransactionInnerTx{
						Type: xdr.EnvelopeTypeEnvelopeTypeTx,
						V1:   &xdr.TransactionV1Envelope{Tx: xdr.Transaction{Fee: 100}},
					},
				},
			},
		},
		Result: xdr.TransactionResultPair{
			TransactionHash: xdr.Hash{0xaa},
			Result: xdr.TransactionResult{
				FeeCharged: 2_000,
				Result: xdr.TransactionResultResult{
					Code: xdr.TransactionResultCodeTxFeeBumpInnerFailed,
					InnerResultPair: &xdr.InnerTransactionResultPair{
						TransactionHash: innerHash,
						Result: xdr.InnerTransactionResult{
							Result: xdr.InnerTransactionResultResult{Code: xdr.TransactionResultCodeTxInsufficientBalance},
						},
					},
				},
			},
		},
	}
}

// TestExtractFeeBump pins that a fee bump's outer layer reaches the row: the
// SDK's Account()/MaxFee() answer for the INNER tx, so without it the payer,
// its bid, the inner hash and the inner failure reason are all lost.
func TestExtractFeeBump(t *testing.T) {
	payerAccount := keypair.MustRandom().Address()
	innerHash := xdr.Hash{0x1e, 0x1e}
	tx := feeBumpTx(t, payerAccount, 20_000, innerHash)

	fb := extractFeeBump(tx)
	want := feeBump{
		InnerTxHash:     hex.EncodeToString(innerHash[:]),
		FeeAccount:      payerAccount,
		Fee:             20_000,
		InnerResultCode: int32(xdr.TransactionResultCodeTxInsufficientBalance),
	}
	if fb != want {
		t.Fatalf("extractFeeBump = %+v, want %+v", fb, want)
	}
	if got := tx.MaxFee(); got != 100 {
		t.Fatalf("fixture: SDK MaxFee = %d, want the inner bid 100", got)
	}
}

// TestExtractFeeBump_PlainTxIsZero: an ordinary V1 tx has no outer layer.
func TestExtractFeeBump_PlainTxIsZero(t *testing.T) {
	tx := ingest.LedgerTransaction{Envelope: xdr.TransactionEnvelope{
		Type: xdr.EnvelopeTypeEnvelopeTypeTx,
		V1:   &xdr.TransactionV1Envelope{Tx: xdr.Transaction{Fee: 100}},
	}}
	if fb := extractFeeBump(tx); fb != (feeBump{}) {
		t.Fatalf("plain tx fee bump = %+v, want zero", fb)
	}
}

// TestTransactionByHashScanResolvesInnerHash: with the index unavailable, a
// fee bump's inner hash still resolves through the scan fallback's second
// probe, and the ledger-scoped read matches it on inner_tx_hash.
func TestTransactionByHashScanResolvesInnerHash(t *testing.T) {
	conn := &stubConn{}
	conn.respond = func(q string) (driver.Rows, error) {
		switch {
		case isIndexProbe(q):
			return &stubRows{}, nil // empty index: scan path
		case isBloomScan(q):
			return &stubRows{}, nil // not an outer hash
		case isInnerBloomScan(q):
			return &stubRows{data: [][]any{{uint32(66_000_000)}}}, nil
		case isLedgerScopedRead(q):
			return &stubRows{data: [][]any{txRowFor(66_000_000, "outer")}}, nil
		default:
			return nil, fmt.Errorf("unexpected query: %s", q)
		}
	}
	r := &ExplorerReader{conn: conn, txIndexProbe: schemaProbe{retryAfter: -1}}

	tx, found, err := r.TransactionByHash(context.Background(), testTxHash)
	if err != nil || !found {
		t.Fatalf("TransactionByHash(inner) = (found=%v, err=%v), want hit", found, err)
	}
	if tx.Seq != 66_000_000 || tx.TxHash != "outer" {
		t.Fatalf("resolved %d/%s, want 66000000/outer", tx.Seq, tx.TxHash)
	}
	last := conn.args[len(conn.args)-1]
	if len(last) != 3 || last[1] != testTxHash || last[2] != testTxHash {
		t.Fatalf("ledger-scoped read args = %v, want the hash bound to tx_hash and inner_tx_hash", last)
	}
}
