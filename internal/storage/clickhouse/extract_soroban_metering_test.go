package clickhouse

import (
	"errors"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// sorobanData builds a SorobanTransactionData with the given declared resources
// and footprint entry counts (the entries themselves are zero-valued keys — the
// decoder only counts them).
func sorobanData(instr, diskRead, write uint32, readEntries, writeEntries int, feeBid int64) xdr.SorobanTransactionData {
	return xdr.SorobanTransactionData{
		Resources: xdr.SorobanResources{
			Footprint: xdr.LedgerFootprint{
				ReadOnly:  make([]xdr.LedgerKey, readEntries),
				ReadWrite: make([]xdr.LedgerKey, writeEntries),
			},
			Instructions:  xdr.Uint32(instr),
			DiskReadBytes: xdr.Uint32(diskRead),
			WriteBytes:    xdr.Uint32(write),
		},
		ResourceFee: xdr.Int64(feeBid),
	}
}

// metaV4WithFees builds a p27-shape TransactionMeta carrying the charged-fee
// ext (non-refundable, refundable, rent).
func metaV4WithFees(nonRefund, refund, rent int64) xdr.TransactionMeta {
	return xdr.TransactionMeta{
		V: 4,
		V4: &xdr.TransactionMetaV4{
			SorobanMeta: &xdr.SorobanTransactionMetaV2{
				Ext: xdr.SorobanTransactionMetaExt{
					V: 1,
					V1: &xdr.SorobanTransactionMetaExtV1{
						TotalNonRefundableResourceFeeCharged: xdr.Int64(nonRefund),
						TotalRefundableResourceFeeCharged:    xdr.Int64(refund),
						RentFeeCharged:                       xdr.Int64(rent),
					},
				},
			},
		},
	}
}

// TestExtractSorobanMetering_V1WithV4Meta pins the happy path: a direct Soroban
// tx (V1 envelope, p27 V4 meta) yields the declared bid from the envelope and
// the charged fees from the meta, with footprint counts read as list lengths.
func TestExtractSorobanMetering_V1WithV4Meta(t *testing.T) {
	sd := sorobanData(30_000_000, 4096, 1024, 3, 2, 987_654)
	tx := ingest.LedgerTransaction{
		Envelope: xdr.TransactionEnvelope{
			Type: xdr.EnvelopeTypeEnvelopeTypeTx,
			V1: &xdr.TransactionV1Envelope{
				Tx: xdr.Transaction{Ext: xdr.TransactionExt{V: 1, SorobanData: &sd}},
			},
		},
		UnsafeMeta: metaV4WithFees(100_000, 20_000, 5_000),
	}
	m := extractSorobanMetering(tx)
	if m.Instructions != 30_000_000 || m.DiskReadBytes != 4096 || m.WriteBytes != 1024 {
		t.Fatalf("declared resources = %+v, want instr=30M read=4096 write=1024", m)
	}
	if m.ReadEntries != 3 || m.WriteEntries != 2 {
		t.Fatalf("footprint entries = read %d / write %d, want 3 / 2", m.ReadEntries, m.WriteEntries)
	}
	if m.ResourceFeeBid != 987_654 {
		t.Fatalf("resource fee bid = %d, want 987654", m.ResourceFeeBid)
	}
	if m.NonRefundableFee != 100_000 || m.RefundableFee != 20_000 || m.RentFee != 5_000 {
		t.Fatalf("charged fees = %+v, want 100000/20000/5000", m)
	}
}

// TestExtractSorobanMetering_FeeBumpReachesInner is the regression guard for the
// audit's nil-panic finding: a fee-bump wrapping a Soroban tx must be unwrapped
// to its inner V1 tx to reach the SorobanTransactionData. A naive .V1.Tx access
// would nil-panic here (env.V1 is nil on a fee-bump).
func TestExtractSorobanMetering_FeeBumpReachesInner(t *testing.T) {
	sd := sorobanData(12_345, 64, 32, 1, 1, 4_242)
	tx := ingest.LedgerTransaction{
		Envelope: xdr.TransactionEnvelope{
			Type: xdr.EnvelopeTypeEnvelopeTypeTxFeeBump,
			FeeBump: &xdr.FeeBumpTransactionEnvelope{
				Tx: xdr.FeeBumpTransaction{
					InnerTx: xdr.FeeBumpTransactionInnerTx{
						Type: xdr.EnvelopeTypeEnvelopeTypeTx,
						V1: &xdr.TransactionV1Envelope{
							Tx: xdr.Transaction{Ext: xdr.TransactionExt{V: 1, SorobanData: &sd}},
						},
					},
				},
			},
		},
		UnsafeMeta: metaV4WithFees(9, 8, 7),
	}
	m := extractSorobanMetering(tx) // must NOT panic
	if m.Instructions != 12_345 || m.ResourceFeeBid != 4_242 {
		t.Fatalf("fee-bump inner not reached: %+v", m)
	}
	if m.NonRefundableFee != 9 || m.RefundableFee != 8 || m.RentFee != 7 {
		t.Fatalf("fee-bump charged fees = %+v, want 9/8/7", m)
	}
}

// TestExtractSorobanMetering_ClassicIsZero: a classic (non-Soroban) V1 tx has no
// SorobanTransactionData, so every metering field is zero — never a panic, never
// a spurious value.
func TestExtractSorobanMetering_ClassicIsZero(t *testing.T) {
	tx := ingest.LedgerTransaction{
		Envelope: xdr.TransactionEnvelope{
			Type: xdr.EnvelopeTypeEnvelopeTypeTx,
			V1: &xdr.TransactionV1Envelope{
				Tx: xdr.Transaction{Ext: xdr.TransactionExt{V: 0}}, // no SorobanData
			},
		},
		UnsafeMeta: xdr.TransactionMeta{V: 3, V3: &xdr.TransactionMetaV3{}}, // no SorobanMeta
	}
	if m := extractSorobanMetering(tx); m != (sorobanMetering{}) {
		t.Fatalf("classic tx metering = %+v, want zero", m)
	}
}

// TestExtractSorobanMetering_V3Meta: the pre-p27 meta shape (V3) carries the same
// charged-fee ext; the decoder must read it via the V3 arm too.
func TestExtractSorobanMetering_V3Meta(t *testing.T) {
	sd := sorobanData(1, 2, 3, 0, 0, 10)
	tx := ingest.LedgerTransaction{
		Envelope: xdr.TransactionEnvelope{
			Type: xdr.EnvelopeTypeEnvelopeTypeTx,
			V1: &xdr.TransactionV1Envelope{
				Tx: xdr.Transaction{Ext: xdr.TransactionExt{V: 1, SorobanData: &sd}},
			},
		},
		UnsafeMeta: xdr.TransactionMeta{
			V: 3,
			V3: &xdr.TransactionMetaV3{
				SorobanMeta: &xdr.SorobanTransactionMeta{
					Ext: xdr.SorobanTransactionMetaExt{
						V:  1,
						V1: &xdr.SorobanTransactionMetaExtV1{TotalNonRefundableResourceFeeCharged: 55},
					},
				},
			},
		},
	}
	m := extractSorobanMetering(tx)
	if m.ResourceFeeBid != 10 || m.NonRefundableFee != 55 {
		t.Fatalf("V3 meta path = %+v, want feeBid=10 nonRefund=55", m)
	}
}

// TestExtractSorobanMetering_SorobanTxNoFeeExt: a Soroban tx whose meta has no
// fee ext (V0 ext) yields the declared resources but zero charged fees — no
// panic on the absent ext.
func TestExtractSorobanMetering_SorobanTxNoFeeExt(t *testing.T) {
	sd := sorobanData(7, 0, 0, 0, 0, 3)
	tx := ingest.LedgerTransaction{
		Envelope: xdr.TransactionEnvelope{
			Type: xdr.EnvelopeTypeEnvelopeTypeTx,
			V1: &xdr.TransactionV1Envelope{
				Tx: xdr.Transaction{Ext: xdr.TransactionExt{V: 1, SorobanData: &sd}},
			},
		},
		UnsafeMeta: xdr.TransactionMeta{
			V:  4,
			V4: &xdr.TransactionMetaV4{SorobanMeta: &xdr.SorobanTransactionMetaV2{Ext: xdr.SorobanTransactionMetaExt{V: 0}}},
		},
	}
	m := extractSorobanMetering(tx)
	if m.Instructions != 7 || m.NonRefundableFee != 0 {
		t.Fatalf("no-fee-ext path = %+v, want instr=7 nonRefund=0", m)
	}
}

// sorobanTxWithMeta is a direct Soroban tx (V1 envelope) carrying meta.
func sorobanTxWithMeta(meta xdr.TransactionMeta) ingest.LedgerTransaction {
	sd := sorobanData(7, 0, 0, 0, 0, 3)
	return ingest.LedgerTransaction{
		Index: 1,
		Envelope: xdr.TransactionEnvelope{
			Type: xdr.EnvelopeTypeEnvelopeTypeTx,
			V1: &xdr.TransactionV1Envelope{
				Tx: xdr.Transaction{
					SourceAccount: xdr.MuxedAccount{Type: xdr.CryptoKeyTypeKeyTypeEd25519, Ed25519: &xdr.Uint256{}},
					Ext:           xdr.TransactionExt{V: 1, SorobanData: &sd},
				},
			},
		},
		Result: xdr.TransactionResultPair{Result: xdr.TransactionResult{
			Result: xdr.TransactionResultResult{Code: xdr.TransactionResultCodeTxSuccess, Results: &[]xdr.OperationResult{}},
		}},
		UnsafeMeta: meta,
		Ledger:     xdr.LedgerCloseMeta{V: 0, V0: &xdr.LedgerCloseMetaV0{}},
	}
}

// TestExtractSorobanMetering_UnknownMetaVersionFlagged: a meta version past V4
// leaves the charged fees unreadable; the zeros must be flagged, not passed off
// as a zero charge.
func TestExtractSorobanMetering_UnknownMetaVersionFlagged(t *testing.T) {
	m := extractSorobanMetering(sorobanTxWithMeta(xdr.TransactionMeta{V: 5}))
	if !m.FeeMetaUnsupported {
		t.Fatalf("V5 meta on a Soroban tx: FeeMetaUnsupported = false, want true (%+v)", m)
	}
	if m.Instructions != 7 || m.ResourceFeeBid != 3 {
		t.Fatalf("declared resources must still be read from the envelope: %+v", m)
	}
	if m.NonRefundableFee != 0 || m.RefundableFee != 0 || m.RentFee != 0 {
		t.Fatalf("charged fees = %+v, want 0 (unreadable)", m)
	}
}

// TestExtractSorobanMetering_KnownShapesNotFlagged: V3/V4 (with or without the
// fee ext) and a classic tx on any meta version are not unsupported.
func TestExtractSorobanMetering_KnownShapesNotFlagged(t *testing.T) {
	cases := map[string]ingest.LedgerTransaction{
		"soroban v4 with ext": sorobanTxWithMeta(metaV4WithFees(1, 2, 3)),
		"soroban v4 no soroban meta": sorobanTxWithMeta(
			xdr.TransactionMeta{V: 4, V4: &xdr.TransactionMetaV4{}}),
		"soroban v3 no soroban meta": sorobanTxWithMeta(
			xdr.TransactionMeta{V: 3, V3: &xdr.TransactionMetaV3{}}),
		"classic v5": {
			Envelope: xdr.TransactionEnvelope{
				Type: xdr.EnvelopeTypeEnvelopeTypeTx,
				V1:   &xdr.TransactionV1Envelope{Tx: xdr.Transaction{Ext: xdr.TransactionExt{V: 0}}},
			},
			UnsafeMeta: xdr.TransactionMeta{V: 5},
		},
	}
	for name, tx := range cases {
		if m := extractSorobanMetering(tx); m.FeeMetaUnsupported {
			t.Errorf("%s: FeeMetaUnsupported = true, want false", name)
		}
	}
}

// TestSorobanMetaFeeExt_VersionArms pins which meta versions are known.
func TestSorobanMetaFeeExt_VersionArms(t *testing.T) {
	for _, v := range []int32{0, 1, 2} {
		if _, ok, err := sorobanMetaFeeExt(xdr.TransactionMeta{V: v}); ok || err != nil {
			t.Errorf("V%d: ok=%v err=%v, want false/nil (pre-Soroban shape)", v, ok, err)
		}
	}
	for _, v := range []int32{5, 6} {
		if _, _, err := sorobanMetaFeeExt(xdr.TransactionMeta{V: v}); !errors.Is(err, errSorobanMetaUnsupported) {
			t.Errorf("V%d: err=%v, want errSorobanMetaUnsupported", v, err)
		}
	}
}

// TestExtractTx_CountsSorobanFeeMetaUnsupported: the per-tx flag must reach the
// LedgerExtract count the indexer meters, and the tx row is still written.
func TestExtractTx_CountsSorobanFeeMetaUnsupported(t *testing.T) {
	var ext LedgerExtract
	extractTx(&ext, sorobanTxWithMeta(xdr.TransactionMeta{V: 5}), 100, time.Unix(0, 0))
	extractTx(&ext, sorobanTxWithMeta(metaV4WithFees(1, 2, 3)), 100, time.Unix(0, 0))
	if ext.SorobanFeeMetaUnsupported != 1 {
		t.Fatalf("SorobanFeeMetaUnsupported = %d, want 1", ext.SorobanFeeMetaUnsupported)
	}
	if len(ext.Txs) != 2 {
		t.Fatalf("Txs = %d, want 2", len(ext.Txs))
	}
}
