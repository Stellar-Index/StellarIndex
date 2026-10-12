package clickhouse

import (
	"encoding/hex"
	"time"

	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/entrywalk"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// extractLedgerEntryChanges populates ext.Changes with one row per LedgerEntryChange, mirroring dispatcher.walkLedgerEntryChanges. Every tx
// is walked, failed too (its fee debit is committed), in three ledger-wide phases like ingest.LedgerChangeReader (fees, apply,
// PostTxApplyFeeChanges), so intra_ledger_seq makes the FINAL-dedup winner the ledger-final balance; upgrade changes are not walked. A
// fourth phase records EVICTIONS as `removed` rows for temporary entries and TTL keys only; skipped keys keep their walk position. Blocks
// follow entrywalk.Canonical order. change_index is per-TRANSACTION and stable across re-ingest (idempotent RMT write); an unmarshalable
// change is skipped, counted, never fatal.
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

// emitEvictions appends one `removed` row per evicted key: empty tx_hash, op_index -1, change_index counting within the group. A
// persistent entry or contract code moves to the hot archive and stays restorable, so its last live row stays current. Every key takes its
// walk position, written or not.
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

// entryChangeRow builds one LedgerEntryChangeRow; ok=false when it can't be marshalled. A P23 `restored` change is a post-image: dropping
// it leaves a restored entry's TTL row at its lapsed value and the liveness filter serves it as archived.
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

// ownerAndAsset extracts the owner account (G-strkey) and asset ("CODE-ISSUER" / "native" / "pool:<hex>") from a ledger key; both empty for
// entry types with no single owner. Asset is empty for all but trustlines because other keys do not name it (claimable balance key is a
// hash, a pool holds two assets, a SAC Balance names only its contract), so per-asset supply summed off this column is a trustline-only
// LOWER BOUND; stellar.supply_flows sees all four domains. See ClassicCirculatingSupply.
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
