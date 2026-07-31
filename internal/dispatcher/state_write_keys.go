package dispatcher

import (
	"bytes"

	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// This file populates events.Event.StateWriteKeys on the production LCM
// path: the base64 XDR LedgerKeys of the contract-data entries whose
// VALUE the event's operation CHANGED, filtered per event to the event's
// own contract. It is the state-write sibling of the OpArgs enrichment
// (extractInvokeContractCalls): same per-operation granularity, same
// "decoders stay pure — the dispatcher supplies inputs" contract.
//
// VALUE-CHANGED, not merely written — ground-truthed on r1 ledger
// 62056824 (2026-07-31): the RedStone adapter's write_prices REWRITES
// every REQUESTED feed's entry, byte-identical for feeds its freshness
// verifier rejected (an empty-batch push still rewrote its one requested
// key unchanged), while an ACCEPTED feed's stored PriceData always
// changes (write_timestamp at minimum). So "keys written" names the
// REQUESTED set and only "keys whose value changed" names the ACCEPTED
// set — the signal Redstone's exact subset attribution needs
// (internal/sources/redstone/decode.go resolveFeedAttribution).
//
// The rule, applied identically here and by the ClickHouse re-derive
// twin (internal/storage/clickhouse/state_write_keys.go, from the lake's
// ledger_entry_changes rows):
//
//   - pre-image  = the op's FIRST `state` change for the key
//     (ContractDataEntry.Val bytes);
//   - post-image = the op's LAST `created`/`updated` change for the key;
//   - changed    = post exists AND (no pre-image OR pre.Val != post.Val).
//
// `removed` is a deletion, not a value write. P23 `restored` changes are
// ignored on BOTH sides (the lake's extract_entry_changes.go does not
// capture them): a restored-then-rewritten-unchanged entry therefore has
// no visible pre-image and would count as changed on both paths equally —
// consumers treat StateWriteKeys as best-effort input with their own
// arity check + fallback (Redstone falls back to payload-median
// alignment), so the worst case is a fallback, never a misattribution.
// Any per-key parse/marshal failure excludes that key (fail toward the
// consumer's fallback, mirroring the lake extractor's skip-and-tolerate).

// contractDataWrite is one value-changing contract-data write.
type contractDataWrite struct {
	ContractID string // C-strkey of the entry's owning contract
	KeyB64     string // base64-encoded XDR LedgerKey
}

// opContractDataWriteKeys returns the contract-data entries whose value
// operation opIdx of tx CHANGED (see the file comment for the exact
// rule), in first-write meta order, one entry per key. Nil when the meta
// version is unknown or the op changed nothing.
func opContractDataWriteKeys(tx ingest.LedgerTransaction, opIdx int) []contractDataWrite {
	changes := opChangeSet(tx, opIdx)
	if len(changes) == 0 {
		return nil
	}
	order, states := accumulateKeyChanges(changes)
	var out []contractDataWrite
	for _, k := range order {
		if ks := states[k]; ks.valueChanged() {
			out = append(out, ks.write)
		}
	}
	return out
}

// opChangeSet returns the LedgerEntryChanges of operation opIdx, nil for
// unknown meta versions or out-of-range indices.
func opChangeSet(tx ingest.LedgerTransaction, opIdx int) []xdr.LedgerEntryChange {
	switch tx.UnsafeMeta.V {
	case 3:
		v3 := tx.UnsafeMeta.MustV3()
		if opIdx < 0 || opIdx >= len(v3.Operations) {
			return nil
		}
		return v3.Operations[opIdx].Changes
	case 4:
		v4 := tx.UnsafeMeta.MustV4()
		if opIdx < 0 || opIdx >= len(v4.Operations) {
			return nil
		}
		return v4.Operations[opIdx].Changes
	default:
		return nil
	}
}

// accumulateKeyChanges folds an op's change list into per-key pre/post
// images (the changed-write rule's inputs), returning keys in
// first-write order.
func accumulateKeyChanges(changes []xdr.LedgerEntryChange) (order []string, states map[string]*keyChangeState) {
	states = make(map[string]*keyChangeState)
	for i := range changes {
		entry, isPre, relevant := changeEntry(changes[i])
		if !relevant {
			continue // removed / restored / unknown — not part of the rule
		}
		w, val, ok := contractDataWriteOf(entry)
		if !ok {
			continue // not contract data
		}
		ks := states[w.KeyB64]
		if ks == nil {
			ks = &keyChangeState{}
			states[w.KeyB64] = ks
		}
		if wroteFirstPost := ks.record(w, val, isPre); wroteFirstPost {
			order = append(order, w.KeyB64)
		}
	}
	return order, states
}

// changeEntry unwraps the entry carried by a state/created/updated
// change. relevant=false for the change types outside the rule.
func changeEntry(c xdr.LedgerEntryChange) (entry xdr.LedgerEntry, isPre, relevant bool) {
	switch c.Type {
	case xdr.LedgerEntryChangeTypeLedgerEntryState:
		return c.MustState(), true, true
	case xdr.LedgerEntryChangeTypeLedgerEntryCreated:
		return c.MustCreated(), false, true
	case xdr.LedgerEntryChangeTypeLedgerEntryUpdated:
		return c.MustUpdated(), false, true
	default:
		return xdr.LedgerEntry{}, false, false
	}
}

// keyChangeState accumulates one key's pre/post images across an
// operation's change list.
type keyChangeState struct {
	write   contractDataWrite
	preVal  []byte
	postVal []byte
	hasPre  bool
	hasPost bool
	bad     bool // any parse failure → exclude the key (fail toward fallback)
}

// record folds one change into the state: a nil val marks the key bad
// (unmarshalable genuine contract data), the FIRST state change becomes
// the pre-image, the LAST write becomes the post-image. Returns true
// exactly when this was the key's first post-image (the caller's
// ordering signal).
func (ks *keyChangeState) record(w contractDataWrite, val []byte, isPre bool) (firstPost bool) {
	if val == nil {
		ks.bad = true
		return false
	}
	if isPre {
		if !ks.hasPre {
			ks.preVal, ks.hasPre = val, true
		}
		return false
	}
	firstPost = !ks.hasPost
	ks.write, ks.postVal, ks.hasPost = w, val, true
	return firstPost
}

// valueChanged applies the changed-write rule from the file comment.
func (ks *keyChangeState) valueChanged() bool {
	if ks.bad || !ks.hasPost {
		return false
	}
	if ks.hasPre && bytes.Equal(ks.preVal, ks.postVal) {
		return false // identical-value rewrite — not accepted state
	}
	return true
}

// contractDataWriteOf projects one contract-data LedgerEntry to its
// (owner, key) identity plus its marshalled Val bytes. ok=false for
// non-contract-data entries and account-owned contract data. A nil val
// with ok=true signals a marshal failure on a genuine contract-data
// entry — the caller excludes that key rather than guessing (mirroring
// the lake extractor's skip-and-tolerate).
func contractDataWriteOf(entry xdr.LedgerEntry) (w contractDataWrite, val []byte, ok bool) {
	if entry.Data.Type != xdr.LedgerEntryTypeContractData {
		return contractDataWrite{}, nil, false
	}
	cd, cok := entry.Data.GetContractData()
	if !cok || cd.Contract.Type != xdr.ScAddressTypeScAddressTypeContract {
		return contractDataWrite{}, nil, false
	}
	cid, err := contractIDToStrkey(cd.Contract.MustContractId())
	if err != nil {
		return contractDataWrite{}, nil, false
	}
	key, err := entry.LedgerKey()
	if err != nil {
		return contractDataWrite{}, nil, false
	}
	keyB64, err := xdr.MarshalBase64(key)
	if err != nil {
		return contractDataWrite{}, nil, false
	}
	w = contractDataWrite{ContractID: cid, KeyB64: keyB64}
	valBytes, err := cd.Val.MarshalBinary()
	if err != nil {
		return w, nil, true // identity known, value not — caller excludes
	}
	return w, valBytes, true
}

// stateWriteKeysFor filters an operation's value-changing contract-data
// writes down to the keys owned by contractID — the per-event slice that
// becomes events.Event.StateWriteKeys. Nil when nothing matches, so
// events of contracts that changed nothing carry no allocation.
func stateWriteKeysFor(writes []contractDataWrite, contractID string) []string {
	var out []string
	for _, w := range writes {
		if w.ContractID == contractID {
			out = append(out, w.KeyB64)
		}
	}
	return out
}
