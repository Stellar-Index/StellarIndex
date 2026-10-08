package clickhouse

import (
	"encoding/hex"
	"time"

	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/entrywalk"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// extractLedgerEntryChanges populates ext.Changes with one row per LedgerEntryChange in the
// whole ledger: the substrate the account-state explorer (ADR-0038) and the ADR-0034 promise
// to re-derive LedgerEntry supply observers from the lake rely on.
//
// Mirrors dispatcher.walkLedgerEntryChanges (meta-version handling, unsupported-version
// counter) so lake rows match the live decoder hook:
//
//   - Every tx is walked, failed txs included: a failed tx's fee debit is committed, only its
//     operation changes roll back.
//   - The walk is ledger-wide and three-phase, as the SDK's ingest.LedgerChangeReader: every
//     tx's fee changes, then every tx's apply-phase changes, then every tx's
//     PostTxApplyFeeChanges (P23 Soroban fee refunds; LCM V2 only). intra_ledger_seq thus ranks
//     an apply change above a later tx's fee change and a refund above both, so "the final
//     intra-ledger change wins FINAL dedup" means the ledger-final balance. Ledger UPGRADE
//     changes are not walked (no tx_hash), same as the dispatcher.
//   - A fourth phase records state-archival EVICTIONS as `removed` rows, only for temporary
//     entries and TTL keys: persistent entries and contract code are archived and restorable,
//     so they stay live. Skipped keys still take their walk position, aligned with
//     dispatcher.walkEvictedKeys.
//
// Within each LedgerEntryChanges block changes are walked in entrywalk.Canonical order, since
// core's block order is hash-map order and differs between exports; only a canonical order makes
// re-extract reproducible.
//
// Fee-meta + TxChangesBefore/After sit at op_index -1, per-operation changes at their op_index.
// change_index is a per-TRANSACTION counter (stable across re-ingest, so idempotent under the
// ReplacingMergeTree) continuing across phases, so a tx's fee change keeps 0. A change that
// won't marshal is skipped and counted, never fatal, and still takes its intra_ledger_seq.
//
// intra_ledger_seq is the per-LEDGER position, monotonic over the canonical walk. It is
// folded into ledger_entries_current's RMT version so the last intra-ledger change to a key
// wins FINAL dedup deterministically.
func extractLedgerEntryChanges(ext *LedgerExtract, txs []ingest.LedgerTransaction, evicted []xdr.LedgerKey, seq uint32, closeTime time.Time) {
	var entryChangeSeq uint32
	// change_index is per-transaction, so it must survive the gap between the two
	// ledger-wide phases: one slot per tx.
	changeIdx := make([]uint32, len(txs))
	emitterFor := func(i int) func(int, xdr.LedgerEntryChange) {
		txHash := hex.EncodeToString(txs[i].Result.TransactionHash[:])
		return func(opIndex int, c xdr.LedgerEntryChange) {
			pos := entryChangeSeq
			entryChangeSeq++
			row, ok := entryChangeRow(seq, closeTime, txHash, int32(opIndex), changeIdx[i], c)
			if !ok {
				ext.EntryChangesUnencodable++
				return
			}
			row.IntraLedgerSeq = pos
			ext.Changes = append(ext.Changes, row)
			changeIdx[i]++
		}
	}

	// ── Phase 1: the fee phase for every tx, in tx-set apply order.
	// op_index -1 marks tx-level changes.
	for i := range txs {
		emit := emitterFor(i)
		emitChangeSet(txs[i].FeeChanges, -1, emit)
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
			// Not silent: an unwalked apply phase looks like a ledger where nothing happened.
			// Mirrors the dispatcher's entryMetaUnsupported counter.
			ext.EntryMetaUnsupported++
		}
	}
	// ── Phase 3: the post-apply fee phase (P23 Soroban fee refunds) for every tx, in the same
	// order, at op_index -1 like the fee phase.
	for i := range txs {
		emit := emitterFor(i)
		emitChangeSet(txs[i].PostTxApplyFeeChanges, -1, emit)
	}
	// ── Phase 4: evictions, last because core evicts at ledger close after
	// every tx has applied. Appended, so phase 1-3 positions are unchanged.
	emitEvictions(ext, evicted, seq, closeTime, entryChangeSeq)
}

// emitEvictions appends one `removed` row per evicted key: empty tx_hash, op_index -1,
// change_index counting within the group. A persistent entry or contract code moves to the hot
// archive and stays restorable, so its last live row stays current. Every key takes its walk
// position, written or not, matching the dispatcher.
func emitEvictions(ext *LedgerExtract, evicted []xdr.LedgerKey, seq uint32, closeTime time.Time, intraSeq uint32) {
	var changeIdx uint32
	for i := range evicted {
		pos := intraSeq
		intraSeq++
		if isArchivedOnEviction(evicted[i]) {
			continue
		}
		row, ok := entryChangeRow(seq, closeTime, "", -1, changeIdx, xdr.LedgerEntryChange{
			Type:    xdr.LedgerEntryChangeTypeLedgerEntryRemoved,
			Removed: &evicted[i],
		})
		if !ok {
			ext.EntryChangesUnencodable++
			continue
		}
		row.IntraLedgerSeq = pos
		ext.Changes = append(ext.Changes, row)
		changeIdx++
	}
}

func isArchivedOnEviction(k xdr.LedgerKey) bool {
	switch k.Type {
	case xdr.LedgerEntryTypeContractCode:
		return true
	case xdr.LedgerEntryTypeContractData:
		return k.ContractData != nil && k.ContractData.Durability == xdr.ContractDataDurabilityPersistent
	}
	return false
}

func emitChangeSet(changes []xdr.LedgerEntryChange, opIdx int, emit func(int, xdr.LedgerEntryChange)) {
	changes = entrywalk.Canonical(changes)
	for i := range changes {
		emit(opIdx, changes[i])
	}
}

// entryChangeRow builds one LedgerEntryChangeRow from an xdr.LedgerEntryChange; ok=false when
// it can't be marshalled. A P23 `restored` change is a post-image: dropping it leaves a
// restored entry's TTL row at its lapsed value and the liveness filter serves it as archived.
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
	case xdr.LedgerEntryChangeTypeLedgerEntryCreated, xdr.LedgerEntryChangeTypeLedgerEntryUpdated,
		xdr.LedgerEntryChangeTypeLedgerEntryState, xdr.LedgerEntryChangeTypeLedgerEntryRestored:
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

// ownerAndAsset extracts the owner account (G-strkey) and asset ("CODE-ISSUER" / "native" /
// "pool:<hex>") from a ledger key. Both empty for entry types with no single owner (claimable
// balances, pools, contract data/code, ttl, config); asset is empty for all but trustlines.
//
// The empty asset on other holding types is structural, not a gap: the key does not name the
// asset (a claimable balance's key is a hash, a pool holds two assets, a SAC Balance entry
// names only its contract, derivable only forward via canonical.Asset.SacContractID). Any
// per-asset supply summed off this column is a trustline-only LOWER BOUND; the lake-flows
// total over the SAC contract (stellar.supply_flows) sees all four domains. See
// asset_supply_reader.go's ClassicCirculatingSupply.
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

// entryBalance returns the stroop balance of an account or trustline entry, else 0; a column
// so top-holder reads sort and aggregate in SQL.
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

// ledgerEntryOf returns the LedgerEntry for a created/updated/state/restored change.
func ledgerEntryOf(c xdr.LedgerEntryChange) (xdr.LedgerEntry, bool) {
	switch c.Type {
	case xdr.LedgerEntryChangeTypeLedgerEntryCreated:
		return c.GetCreated()
	case xdr.LedgerEntryChangeTypeLedgerEntryUpdated:
		return c.GetUpdated()
	case xdr.LedgerEntryChangeTypeLedgerEntryState:
		return c.GetState()
	case xdr.LedgerEntryChangeTypeLedgerEntryRestored:
		return c.GetRestored()
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
	case xdr.LedgerEntryChangeTypeLedgerEntryRestored:
		return "restored"
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
