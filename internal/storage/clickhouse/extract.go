package clickhouse

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// ExtractLedger structurally decodes one LedgerCloseMeta into Tier-1 rows.
// Decoder-INDEPENDENT: it records the shape + raw XDR, not protocol meaning.
//
// Scope (Phase-2 PoC): ledgers, transactions, operations, operation_results,
// contract_events. ledger_entry_changes is deferred (not in the PoC gates;
// needs per-op change attribution) — Extract.Changes stays nil for now.
//
// Resilient like dispatcher.CensusLedger: a per-tx read error is skipped +
// tolerated, not fatal, so one bad tx can't lose a whole ledger.
func ExtractLedger(lcm xdr.LedgerCloseMeta, passphrase string) (LedgerExtract, error) {
	seq := lcm.LedgerSequence()
	closeTime := lcm.ClosedAt().UTC()
	hdr := lcm.LedgerHeaderHistoryEntry().Header

	ext := LedgerExtract{
		Ledger: LedgerRow{
			LedgerSeq:       seq,
			CloseTime:       closeTime,
			LedgerHash:      hashHex(lcm.LedgerHash()),
			PrevHash:        hashHex(lcm.PreviousLedgerHash()),
			ProtocolVersion: lcm.ProtocolVersion(),
			BucketListHash:  hashHex(lcm.BucketListHash()),
			TotalCoins:      int64(hdr.TotalCoins),
			FeePool:         int64(hdr.FeePool),
			BaseFee:         uint32(hdr.BaseFee),
			BaseReserve:     uint32(hdr.BaseReserve),
		},
	}

	reader, err := ingest.NewLedgerTransactionReaderFromLedgerCloseMeta(passphrase, lcm)
	if err != nil {
		return LedgerExtract{}, fmt.Errorf("clickhouse: extract reader ledger %d: %w", seq, err)
	}
	defer func() { _ = reader.Close() }()

	for {
		tx, rerr := reader.Read()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			continue // skip + tolerate, mirroring CensusLedger
		}
		extractTx(&ext, tx, seq, closeTime)
	}

	return ext, nil
}

// extractTx appends one transaction's rows (the tx, its ops + results, its
// contract events) to ext and updates the per-ledger counts.
func extractTx(ext *LedgerExtract, tx ingest.LedgerTransaction, seq uint32, closeTime time.Time) {
	txIndex := tx.Index - 1 // Index is 1-based; store 0-based
	txHash := hex.EncodeToString(tx.Result.TransactionHash[:])
	txSource, _ := tx.Account()
	feeCharged, _ := tx.FeeCharged()

	ext.Txs = append(ext.Txs, TransactionRow{
		LedgerSeq:      seq,
		CloseTime:      closeTime,
		TxHash:         txHash,
		TxIndex:        txIndex,
		SourceAccount:  txSource,
		FeeCharged:     feeCharged,
		MaxFee:         int64(tx.MaxFee()),
		OperationCount: uint16(tx.OperationCount()),
		Successful:     b2u8(tx.Result.Successful()),
		ResultCode:     int32(tx.Result.Result.Result.Code),
		MemoType:       tx.MemoType(),
		Memo:           tx.Memo(),
	})
	ext.Ledger.TxCount++

	extractOps(ext, tx, seq, closeTime, txHash, txSource, txIndex, tx.Result.Successful())
	extractEvents(ext, tx, seq, closeTime, txHash)
}

// extractOps appends one tx's operation + operation_result rows and updates
// the op + classic-trade-effect counts.
func extractOps(ext *LedgerExtract, tx ingest.LedgerTransaction, seq uint32, closeTime time.Time, txHash, txSource string, txIndex uint32, successful bool) {
	ops := tx.Envelope.Operations()
	opResults, hasResults := tx.Result.OperationResults()
	for i := range ops {
		op := ops[i]
		opSource := txSource
		if op.SourceAccount != nil {
			opSource = op.SourceAccount.ToAccountId().Address()
		}
		body, berr := xdr.MarshalBase64(op.Body)
		if berr != nil {
			continue
		}
		ext.Ops = append(ext.Ops, OperationRow{
			LedgerSeq:     seq,
			CloseTime:     closeTime,
			TxHash:        txHash,
			TxIndex:       txIndex,
			OpIndex:       uint32(i),
			OpType:        op.Body.Type.String(),
			SourceAccount: opSource,
			BodyXDR:       body,
		})
		ext.Ledger.OpCount++
		if hasResults && i < len(opResults) {
			appendOpResult(ext, seq, txHash, uint32(i), opResults[i]) // capture all op results (incl. failed) for the lake
			if successful {
				// classic_trade_effect_count mirrors the census/SDEX count,
				// which only counts trades in SUCCESSFUL txs (rolled-back ops
				// in a failed tx show success codes but never happened).
				ext.Ledger.ClassicTradeEffectCount += uint32(claimAtomCount(op, opResults[i]))
			}
		}
	}
}

func appendOpResult(ext *LedgerExtract, seq uint32, txHash string, opIndex uint32, res xdr.OperationResult) {
	rxdr, xerr := xdr.MarshalBase64(res)
	if xerr != nil {
		return
	}
	ext.Results = append(ext.Results, OperationResultRow{
		LedgerSeq:  seq,
		TxHash:     txHash,
		OpIndex:    opIndex,
		ResultCode: int32(res.Code),
		ResultXDR:  rxdr,
	})
}

// extractEvents appends one tx's eligible contract-event rows.
func extractEvents(ext *LedgerExtract, tx ingest.LedgerTransaction, seq uint32, closeTime time.Time, txHash string) {
	txEvents, terr := tx.GetTransactionEvents()
	if terr != nil {
		return
	}
	for opIdx, opEvents := range txEvents.OperationEvents {
		for evIdx := range opEvents {
			row, ok := eventRow(opEvents[evIdx], seq, closeTime, txHash, opIdx, evIdx)
			if !ok {
				continue
			}
			ext.Events = append(ext.Events, row)
			ext.Ledger.SorobanEventCount++
		}
	}
}

// eventRow maps one contract event to a ContractEventRow, applying the same
// capture-eligibility gate as dispatcher.captureEligible (Type=Contract,
// ContractId set, body V0, ≥1 topic). Returns ok=false to skip ineligible
// events so the row count matches the census oracle.
func eventRow(ce xdr.ContractEvent, seq uint32, closeTime time.Time, txHash string, opIdx, evIdx int) (ContractEventRow, bool) {
	if ce.Type != xdr.ContractEventTypeContract || ce.ContractId == nil || ce.Body.V != 0 {
		return ContractEventRow{}, false
	}
	v0, ok := ce.Body.GetV0()
	if !ok || len(v0.Topics) == 0 {
		return ContractEventRow{}, false
	}
	cid, err := strkey.Encode(strkey.VersionByteContract, ce.ContractId[:])
	if err != nil {
		return ContractEventRow{}, false
	}
	topics := make([]string, 0, len(v0.Topics))
	for i := range v0.Topics {
		raw, merr := v0.Topics[i].MarshalBinary()
		if merr != nil {
			return ContractEventRow{}, false
		}
		topics = append(topics, base64.StdEncoding.EncodeToString(raw))
	}
	dataRaw, derr := v0.Data.MarshalBinary()
	if derr != nil {
		return ContractEventRow{}, false
	}
	var topic0Sym string
	if sym, sok := v0.Topics[0].GetSym(); sok {
		topic0Sym = string(sym)
	}
	return ContractEventRow{
		LedgerSeq:        seq,
		CloseTime:        closeTime,
		TxHash:           txHash,
		OpIndex:          uint32(opIdx),
		EventIndex:       uint32(evIdx),
		ContractID:       cid,
		EventType:        "contract",
		TopicCount:       uint8(len(topics)),
		Topic0Sym:        topic0Sym,
		TopicsXDR:        topics,
		DataXDR:          base64.StdEncoding.EncodeToString(dataRaw),
		OpArgsXDR:        nil, // op args plumbed in a later pass (RedStone/Band)
		InSuccessfulCall: 1,
	}, true
}

// claimAtomCount mirrors dispatcher.claimAtomCount exactly (same op types +
// success gating) so classic_trade_effect_count equals the SDEX trade count.
func claimAtomCount(op xdr.Operation, result xdr.OperationResult) int {
	if result.Code != xdr.OperationResultCodeOpInner {
		return 0
	}
	tr, ok := result.GetTr()
	if !ok {
		return 0
	}
	switch op.Body.Type {
	case xdr.OperationTypeManageSellOffer:
		r, ok := tr.GetManageSellOfferResult()
		if !ok || r.Code != xdr.ManageSellOfferResultCodeManageSellOfferSuccess {
			return 0
		}
		return len(r.MustSuccess().OffersClaimed)
	case xdr.OperationTypeManageBuyOffer:
		r, ok := tr.GetManageBuyOfferResult()
		if !ok || r.Code != xdr.ManageBuyOfferResultCodeManageBuyOfferSuccess {
			return 0
		}
		return len(r.MustSuccess().OffersClaimed)
	case xdr.OperationTypeCreatePassiveSellOffer:
		r, ok := tr.GetManageSellOfferResult()
		if !ok || r.Code != xdr.ManageSellOfferResultCodeManageSellOfferSuccess {
			return 0
		}
		return len(r.MustSuccess().OffersClaimed)
	case xdr.OperationTypePathPaymentStrictReceive:
		r, ok := tr.GetPathPaymentStrictReceiveResult()
		if !ok || r.Code != xdr.PathPaymentStrictReceiveResultCodePathPaymentStrictReceiveSuccess {
			return 0
		}
		return len(r.MustSuccess().Offers)
	case xdr.OperationTypePathPaymentStrictSend:
		r, ok := tr.GetPathPaymentStrictSendResult()
		if !ok || r.Code != xdr.PathPaymentStrictSendResultCodePathPaymentStrictSendSuccess {
			return 0
		}
		return len(r.MustSuccess().Offers)
	}
	return 0
}

func hashHex(h xdr.Hash) string { return hex.EncodeToString(h[:]) }

func b2u8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}
