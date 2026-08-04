package clickhouse

import (
	"encoding/hex"
	"time"

	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// extractLedgerEntryChanges populates ext.Changes with one row per
// LedgerEntryChange in the WHOLE ledger — the substrate the account-state
// explorer (ADR-0038 Phase C) re-derives current balances/trustlines/offers/
// contract-data from, and the ADR-0034 "re-derive the LedgerEntry supply
// observers from the lake" promise. Closes the G12-03 known gap in
// ExtractLedger.
//
// Mirrors dispatcher.walkLedgerEntryChanges — including its
// meta-version handling and its unsupported-version counter — so the lake's rows match
// what the live LedgerEntryChangeDecoder hook sees — including its two
// correctness properties (see that function's doc for the full derivation):
//
//   - Every tx is walked, FAILED TXS INCLUDED: a failed tx's fee debit is
//     committed on chain, and only its operation changes are rolled back
//     (C2-023/C2-040, audit-2026-07-23).
//   - The walk is LEDGER-WIDE AND THREE-PHASE — every tx's fee changes,
//     then every tx's apply-phase changes, then every tx's
//     PostTxApplyFeeChanges — mirroring the SDK's canonical
//     ingest.LedgerChangeReader state machine (feeChangesState →
//     metaChangesState → postTxApplyState). stellar-core charges all fees
//     before applying any transaction (C2-032), and protocol 23 moved the
//     Soroban fee REFUND into a third ledger-wide phase applied after all
//     transactions execute (R-A01-1; LCM V2 only, so empty pre-P23).
//     intra_ledger_seq therefore ranks an apply-phase change above a later
//     tx's fee change and a refund above both, which is what makes "the
//     FINAL intra-ledger change wins FINAL dedup" mean the ledger-final
//     balance. Ledger UPGRADE changes (the SDK's 4th state) are deliberately
//     not walked — they are not transaction-scoped and carry no tx_hash;
//     dispatcher.walkLedgerEntryChanges makes the identical choice.
//
// Change positions within a tx keep their existing shape: fee-meta +
// TxChangesBefore/After at op_index -1, per-operation changes at their
// op_index. change_index is a monotonic per-TRANSACTION counter (stable
// across re-ingest → idempotent under the ReplacingMergeTree) and continues
// across the two phases for a given tx, so a tx's fee change keeps
// change_index 0. Resilient: a change that won't marshal is skipped, never
// fatal.
//
// intra_ledger_seq is the per-LEDGER position. Unlike change_index it is
// monotonic over the whole ledger's canonical walk, so it uniquely orders
// every change to a given key within one ledger. It is folded into
// ledger_entries_current's ReplacingMergeTree version so the LAST
// intra-ledger change to a key wins FINAL dedup deterministically
// (audit-2026-07-16 C2-4c).
func extractLedgerEntryChanges(ext *LedgerExtract, txs []ingest.LedgerTransaction, seq uint32, closeTime time.Time) {
	var entryChangeSeq uint32
	// change_index is per-transaction, so it must survive the gap between
	// the two ledger-wide phases — one slot per tx.
	changeIdx := make([]uint32, len(txs))
	emitterFor := func(i int) func(int, xdr.LedgerEntryChange) {
		txHash := hex.EncodeToString(txs[i].Result.TransactionHash[:])
		return func(opIndex int, c xdr.LedgerEntryChange) {
			row, ok := entryChangeRow(seq, closeTime, txHash, int32(opIndex), changeIdx[i], c)
			if !ok {
				return
			}
			row.IntraLedgerSeq = entryChangeSeq
			ext.Changes = append(ext.Changes, row)
			changeIdx[i]++
			entryChangeSeq++
		}
	}

	// ── Phase 1: the fee phase for every tx, in tx-set apply order.
	// op_index -1 marks tx-level changes.
	for i := range txs {
		emit := emitterFor(i)
		for j := range txs[i].FeeChanges {
			emit(-1, txs[i].FeeChanges[j])
		}
	}
	// ── Phase 2: the apply phase for every tx, in the same order.
	for i := range txs {
		emit := emitterFor(i)
		switch txs[i].UnsafeMeta.V {
		case 3:
			v3 := txs[i].UnsafeMeta.MustV3()
			emitChangeSet(v3.TxChangesBefore, -1, emit)
			for opIdx := range v3.Operations {
				emitChangeSet(v3.Operations[opIdx].Changes, opIdx, emit)
			}
			emitChangeSet(v3.TxChangesAfter, -1, emit)
		case 4:
			v4 := txs[i].UnsafeMeta.MustV4()
			emitChangeSet(v4.TxChangesBefore, -1, emit)
			for opIdx := range v4.Operations {
				emitChangeSet(v4.Operations[opIdx].Changes, opIdx, emit)
			}
			emitChangeSet(v4.TxChangesAfter, -1, emit)
		default:
			// Not silent: an unwalked apply phase is indistinguishable
			// from a ledger in which nothing happened. Mirrors the
			// dispatcher's entryMetaUnsupported counter.
			ext.EntryMetaUnsupported++
		}
	}
	// ── Phase 3: the post-apply fee phase (P23 Soroban fee refunds) for
	// every tx, in the same order. op_index -1 — a tx-level change, like the
	// fee phase it mirrors.
	for i := range txs {
		emit := emitterFor(i)
		for j := range txs[i].PostTxApplyFeeChanges {
			emit(-1, txs[i].PostTxApplyFeeChanges[j])
		}
	}
}

func emitChangeSet(changes []xdr.LedgerEntryChange, opIdx int, emit func(int, xdr.LedgerEntryChange)) {
	for i := range changes {
		emit(opIdx, changes[i])
	}
}

// entryChangeRow builds one LedgerEntryChangeRow from an xdr.LedgerEntryChange.
// ok=false when the change can't be marshalled (skip + tolerate). For
// created/updated/state the key is derived from the entry and the entry XDR is
// retained; for removed only the key is present.
func entryChangeRow(seq uint32, closeTime time.Time, txHash string, opIndex int32, changeIdx uint32, c xdr.LedgerEntryChange) (LedgerEntryChangeRow, bool) {
	row := LedgerEntryChangeRow{
		LedgerSeq:   seq,
		CloseTime:   closeTime,
		TxHash:      txHash,
		OpIndex:     opIndex,
		ChangeIndex: changeIdx,
		ChangeType:  changeTypeName(c.Type),
	}

	var key xdr.LedgerKey
	switch c.Type {
	case xdr.LedgerEntryChangeTypeLedgerEntryCreated, xdr.LedgerEntryChangeTypeLedgerEntryUpdated, xdr.LedgerEntryChangeTypeLedgerEntryState:
		entry, ok := ledgerEntryOf(c)
		if !ok {
			return LedgerEntryChangeRow{}, false
		}
		k, err := entry.LedgerKey()
		if err != nil {
			return LedgerEntryChangeRow{}, false
		}
		key = k
		row.EntryType = entryTypeName(entry.Data.Type)
		row.Balance = entryBalance(entry)
		entryB64, err := xdr.MarshalBase64(entry)
		if err != nil {
			return LedgerEntryChangeRow{}, false
		}
		row.EntryXDR = entryB64
	case xdr.LedgerEntryChangeTypeLedgerEntryRemoved:
		k, ok := c.GetRemoved()
		if !ok {
			return LedgerEntryChangeRow{}, false
		}
		key = k
		row.EntryType = entryTypeName(k.Type)
	default:
		return LedgerEntryChangeRow{}, false
	}

	keyB64, err := xdr.MarshalBase64(key)
	if err != nil {
		return LedgerEntryChangeRow{}, false
	}
	row.KeyXDR = keyB64
	row.AccountID, row.Asset = ownerAndAsset(key)
	return row, true
}

// ownerAndAsset extracts the queryable owner account (G-strkey) and asset
// (canonical "CODE-ISSUER" / "native" / "pool:<hex>") from a ledger key, for
// the account-state + asset-holder explorer indexes. Both empty for entry
// types with no single owning account (claimable balances, liquidity pools,
// contract data/code, ttl, config); asset is empty for everything but
// trustlines.
func ownerAndAsset(key xdr.LedgerKey) (accountID, asset string) {
	switch key.Type {
	case xdr.LedgerEntryTypeAccount:
		if a, ok := key.GetAccount(); ok {
			accountID = a.AccountId.Address()
		}
	case xdr.LedgerEntryTypeTrustline:
		if t, ok := key.GetTrustLine(); ok {
			accountID = t.AccountId.Address()
			asset = xdrjson.TrustLineAssetID(t.Asset)
		}
	case xdr.LedgerEntryTypeOffer:
		if o, ok := key.GetOffer(); ok {
			accountID = o.SellerId.Address()
		}
	case xdr.LedgerEntryTypeData:
		if d, ok := key.GetData(); ok {
			accountID = d.AccountId.Address()
		}
	}
	return accountID, asset
}

// entryBalance returns the stroop balance carried by an account (native) or
// trustline entry, 0 for every other entry type. Stored as a queryable column
// so top-holder / account-balance reads sort + aggregate in SQL.
func entryBalance(e xdr.LedgerEntry) int64 {
	switch e.Data.Type {
	case xdr.LedgerEntryTypeAccount:
		if a, ok := e.Data.GetAccount(); ok {
			return int64(a.Balance)
		}
	case xdr.LedgerEntryTypeTrustline:
		if t, ok := e.Data.GetTrustLine(); ok {
			return int64(t.Balance)
		}
	}
	return 0
}

// ledgerEntryOf returns the LedgerEntry for a created/updated/state change.
func ledgerEntryOf(c xdr.LedgerEntryChange) (xdr.LedgerEntry, bool) {
	switch c.Type {
	case xdr.LedgerEntryChangeTypeLedgerEntryCreated:
		return c.GetCreated()
	case xdr.LedgerEntryChangeTypeLedgerEntryUpdated:
		return c.GetUpdated()
	case xdr.LedgerEntryChangeTypeLedgerEntryState:
		return c.GetState()
	default:
		return xdr.LedgerEntry{}, false
	}
}

// changeTypeName maps the XDR change-type enum to a stable lake value.
func changeTypeName(t xdr.LedgerEntryChangeType) string {
	switch t {
	case xdr.LedgerEntryChangeTypeLedgerEntryCreated:
		return "created"
	case xdr.LedgerEntryChangeTypeLedgerEntryUpdated:
		return "updated"
	case xdr.LedgerEntryChangeTypeLedgerEntryRemoved:
		return "removed"
	case xdr.LedgerEntryChangeTypeLedgerEntryState:
		return "state"
	default:
		return "unknown"
	}
}

// entryTypeName maps the XDR ledger-entry-type enum to a stable lake value.
func entryTypeName(t xdr.LedgerEntryType) string {
	switch t {
	case xdr.LedgerEntryTypeAccount:
		return "account"
	case xdr.LedgerEntryTypeTrustline:
		return "trustline"
	case xdr.LedgerEntryTypeOffer:
		return "offer"
	case xdr.LedgerEntryTypeData:
		return "data"
	case xdr.LedgerEntryTypeClaimableBalance:
		return "claimable_balance"
	case xdr.LedgerEntryTypeLiquidityPool:
		return "liquidity_pool"
	case xdr.LedgerEntryTypeContractData:
		return "contract_data"
	case xdr.LedgerEntryTypeContractCode:
		return "contract_code"
	case xdr.LedgerEntryTypeTtl:
		return "ttl"
	case xdr.LedgerEntryTypeConfigSetting:
		return "config_setting"
	default:
		return "unknown"
	}
}
