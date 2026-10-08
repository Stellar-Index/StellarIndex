package clickhouse

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// This file is the lake-side twin of internal/dispatcher's events.Event.StateWriteKeys
// enrichment: per streamed contract event, it recovers from stellar.ledger_entry_changes
// (linkage (ledger_seq, tx_hash, op_index)) the base64 LedgerKeys of the contract-data entries
// whose VALUE the op changed, filtered to the event's contract, by the dispatcher's rule
// (internal/dispatcher/state_write_keys.go):
//
//   - pre-image  = the op's FIRST `state` row for the key (ContractDataEntry.Val bytes);
//   - post-image = the op's LAST `created`/`updated` row for the key;
//   - changed    = post exists AND (no pre-image OR pre.Val != post.Val).
//
// Byte-identical rewrites are not changes: write_prices rewrites every requested feed's entry
// but only the accepted feed's PriceData changes.
//
// Cost: one point query per stateWriteKeyBatch events, a primary-key prefix lookup
// ((ledger_seq, tx_hash, op_index) is the ORDER BY prefix) bounded by the batch's ledger span
// for partition pruning. Opt-in per source (like withOpArgs); only redstone uses it.
//
// ReplacingMergeTree: un-merged duplicates repeat (ledger_seq, tx_hash, op_index,
// change_index); the scan keeps the row with the latest ingested_at per change_index, as FINAL
// would, so no FINAL is needed and a re-ingested correction beats its stale predecessor.

// stateWriteKeyBatch is how many streamed events accumulate per ledger_entry_changes lookup.
// Buffered events hold wide OpArgs, so keep it small; 128 is well under a megabyte for
// redstone while cutting query count by two orders of magnitude.
const stateWriteKeyBatch = 128

// opRef identifies one operation in the lake.
type opRef struct {
	Ledger  uint32
	TxHash  string
	OpIndex int
}

// entryChangeLite is the slice of a ledger_entry_changes row the changed-write rule needs.
type entryChangeLite struct {
	ChangeIndex uint32
	ChangeType  string
	KeyXDR      string
	EntryXDR    string
}

// stateWriteKeyEnricher buffers events, resolves their value-changing write keys in one query
// per batch, and forwards each event, enriched and in order, to fn.
type stateWriteKeyEnricher struct {
	ctx  context.Context
	conn driver.Conn
	fn   func(events.Event) error
	buf  []events.Event
}

func newStateWriteKeyEnricher(ctx context.Context, conn driver.Conn, fn func(events.Event) error) *stateWriteKeyEnricher {
	return &stateWriteKeyEnricher{ctx: ctx, conn: conn, fn: fn}
}

// add buffers one event, flushing when the batch is full.
func (e *stateWriteKeyEnricher) add(ev events.Event) error {
	e.buf = append(e.buf, ev)
	if len(e.buf) >= stateWriteKeyBatch {
		return e.flush()
	}
	return nil
}

// flush resolves keys for the buffered batch and forwards every event. A lookup error is
// fatal (the stream fails); an op with no change rows yields no keys, so a coverage hole
// degrades consumers to their no-keys fallback instead of failing the stream.
func (e *stateWriteKeyEnricher) flush() error {
	if len(e.buf) == 0 {
		return nil
	}
	changesByOp, err := fetchOpEntryChanges(e.ctx, e.conn, e.buf)
	if err != nil {
		return err
	}
	for i := range e.buf {
		ev := e.buf[i]
		ref := opRef{Ledger: ev.Ledger, TxHash: ev.TxHash, OpIndex: ev.OperationIndex}
		ev.StateWriteKeys = changedWriteKeysForContract(changesByOp[ref], ev.ContractID)
		if ferr := e.fn(ev); ferr != nil {
			return ferr
		}
	}
	e.buf = e.buf[:0]
	return nil
}

// fetchOpEntryChanges runs one batched lookup for the distinct ops behind the buffered events
// and returns each op's contract-data change rows in change_index order, deduped across parts.
func fetchOpEntryChanges(ctx context.Context, conn driver.Conn, batch []events.Event) (map[opRef][]entryChangeLite, error) {
	refs := make([]opRef, 0, len(batch))
	seen := make(map[opRef]bool, len(batch))
	for i := range batch {
		ref := opRef{Ledger: batch[i].Ledger, TxHash: batch[i].TxHash, OpIndex: batch[i].OperationIndex}
		if seen[ref] {
			continue
		}
		seen[ref] = true
		refs = append(refs, ref)
	}
	rows, err := conn.Query(ctx, stateWriteKeysQuery(refs))
	if err != nil {
		return nil, fmt.Errorf("clickhouse: query state write keys (%d ops): %w", len(refs), err)
	}
	defer func() { _ = rows.Close() }()

	acc := newOpChangeAccumulator(len(refs))
	for rows.Next() {
		var (
			ledger     uint32
			txHash     string
			opIndex    int32
			ingestedAt time.Time
			lite       entryChangeLite
		)
		if serr := rows.Scan(&ledger, &txHash, &opIndex, &lite.ChangeIndex, &lite.ChangeType, &lite.KeyXDR, &lite.EntryXDR, &ingestedAt); serr != nil {
			return nil, fmt.Errorf("clickhouse: scan state write keys: %w", serr)
		}
		acc.add(opRef{Ledger: ledger, TxHash: txHash, OpIndex: int(opIndex)}, lite, ingestedAt)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("clickhouse: state write keys rows: %w", rerr)
	}
	return acc.out, nil
}

// opChangeAccumulator folds ORDER BY change_index rows into per-op lists, deduping repeated
// change_index rows from un-merged parts by keeping the LATEST ingested_at (FINAL-merge
// semantics). Keeping the first-seen row could resurrect a stale pre-correction row, since
// read order among duplicate parts is not version order. Pure, for unit tests.
type opChangeAccumulator struct {
	out     map[opRef][]entryChangeLite
	lastIdx map[opRef]uint32
	lastIng map[opRef]time.Time
}

func newOpChangeAccumulator(sizeHint int) *opChangeAccumulator {
	return &opChangeAccumulator{
		out:     make(map[opRef][]entryChangeLite, sizeHint),
		lastIdx: make(map[opRef]uint32, sizeHint),
		lastIng: make(map[opRef]time.Time, sizeHint),
	}
}

// add folds one row. Duplicates of one logical row are adjacent (ORDER BY change_index); a
// repeated change_index replaces the previous row only when ingested_at is strictly newer.
// Ties keep the first: equal versions carry identical content.
func (a *opChangeAccumulator) add(ref opRef, lite entryChangeLite, ingestedAt time.Time) {
	if prev, ok := a.lastIdx[ref]; ok && prev == lite.ChangeIndex {
		if ingestedAt.After(a.lastIng[ref]) {
			a.out[ref][len(a.out[ref])-1] = lite
			a.lastIng[ref] = ingestedAt
		}
		return
	}
	a.lastIdx[ref] = lite.ChangeIndex
	a.lastIng[ref] = ingestedAt
	a.out[ref] = append(a.out[ref], lite)
}

// stateWriteKeysQuery renders the batched lookup. The ledger BETWEEN prunes partitions; the
// tuple IN pins exact ops (the ORDER BY prefix, so index lookups). tx_hash values from the lake
// are quote-escaped like every lake literal (see sqlQuoteEscaped). Pure, for unit tests.
func stateWriteKeysQuery(refs []opRef) string {
	minL, maxL := refs[0].Ledger, refs[0].Ledger
	tuples := make([]string, 0, len(refs))
	for _, r := range refs {
		if r.Ledger < minL {
			minL = r.Ledger
		}
		if r.Ledger > maxL {
			maxL = r.Ledger
		}
		tuples = append(tuples, fmt.Sprintf("(%d,%s,%d)", r.Ledger, sqlQuoteEscaped(r.TxHash), r.OpIndex))
	}
	return fmt.Sprintf(`
		SELECT ledger_seq, tx_hash, op_index, change_index, change_type, key_xdr, entry_xdr, ingested_at
		FROM stellar.ledger_entry_changes
		WHERE ledger_seq BETWEEN %d AND %d
		  AND entry_type = 'contract_data'
		  AND change_type IN ('state','created','updated')
		  AND (ledger_seq, tx_hash, op_index) IN (%s)
		ORDER BY ledger_seq, tx_hash, op_index, change_index`,
		minL, maxL, strings.Join(tuples, ","))
}

// changedWriteKeysForContract applies the changed-write rule to one op's change rows and
// returns the keys owned by contractID (C-strkey) in first-write order. A per-key parse
// failure excludes that key (toward the consumer's fallback, never a guess).
func changedWriteKeysForContract(rows []entryChangeLite, contractID string) []string {
	if len(rows) == 0 {
		return nil
	}
	var order []string
	states := make(map[string]*lakeKeyState)
	for _, r := range rows {
		owner, val, ok, decodable := contractDataEntryVal(r.EntryXDR)
		if !decodable {
			// Unparseable entry_xdr: the owner is unknowable but the key is not (key_xdr is separate).
			// Poison the KEY, not the ROW: dropping the row discards a pre-image and would promote an
			// identical rewrite to "changed". Other contracts' keys embed their own address, so a
			// foreign row's poison cannot collide with this contract's keys.
			ks := states[r.KeyXDR]
			if ks == nil {
				ks = &lakeKeyState{}
				states[r.KeyXDR] = ks
			}
			ks.bad = true
			continue
		}
		if !ok || owner != contractID {
			continue
		}
		ks := states[r.KeyXDR]
		if ks == nil {
			ks = &lakeKeyState{}
			states[r.KeyXDR] = ks
		}
		if ks.record(r.ChangeType, val) {
			order = append(order, r.KeyXDR)
		}
	}
	var out []string
	for _, k := range order {
		if states[k].valueChanged() {
			out = append(out, k)
		}
	}
	return out
}

// lakeKeyState accumulates one key's pre/post images; the CH mirror of the dispatcher's
// keyChangeState.
type lakeKeyState struct {
	preVal  []byte
	postVal []byte
	hasPre  bool
	hasPost bool
	bad     bool
}

// record folds one row: nil val marks the key bad, the FIRST `state` row is the pre-image,
// the LAST write row the post-image. Returns true on the key's first post-image (ordering signal).
func (ks *lakeKeyState) record(changeType string, val []byte) (firstPost bool) {
	if val == nil {
		ks.bad = true
		return false
	}
	switch changeType {
	case "state":
		if !ks.hasPre {
			ks.preVal, ks.hasPre = val, true
		}
	case "created", "updated":
		firstPost = !ks.hasPost
		ks.postVal, ks.hasPost = val, true
	}
	return firstPost
}

// valueChanged applies the changed-write rule (see the file comment).
func (ks *lakeKeyState) valueChanged() bool {
	if ks.bad || !ks.hasPost {
		return false
	}
	if ks.hasPre && bytes.Equal(ks.preVal, ks.postVal) {
		return false // identical-value rewrite — not accepted state
	}
	return true
}

// contractDataEntryVal parses entry_xdr and returns the owning C-strkey plus the
// ContractDataEntry.Val bytes. decodable=false: unmarshal failed, so the caller must poison
// the row's KEY (see changedWriteKeysForContract), not drop the row. decodable=true with
// ok=false: not contract-data or account-owned, skip; nil val with ok=true: Val marshal
// failure, callers exclude the key.
func contractDataEntryVal(entryB64 string) (contractID string, val []byte, ok, decodable bool) {
	var entry xdr.LedgerEntry
	if err := xdr.SafeUnmarshalBase64(entryB64, &entry); err != nil {
		return "", nil, false, false
	}
	if entry.Data.Type != xdr.LedgerEntryTypeContractData {
		return "", nil, false, true
	}
	cd, cok := entry.Data.GetContractData()
	if !cok || cd.Contract.Type != xdr.ScAddressTypeScAddressTypeContract {
		return "", nil, false, true
	}
	cid := cd.Contract.MustContractId()
	strk, err := strkey.Encode(strkey.VersionByteContract, cid[:])
	if err != nil {
		return "", nil, false, true
	}
	valBytes, err := cd.Val.MarshalBinary()
	if err != nil {
		return strk, nil, true, true
	}
	return strk, valBytes, true, true
}
