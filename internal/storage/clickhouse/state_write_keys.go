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

// This file is the lake-side twin of internal/dispatcher's
// events.Event.StateWriteKeys enrichment: it recovers, per streamed
// contract event, the base64 XDR LedgerKeys of the contract-data entries
// whose VALUE the event's operation CHANGED, from
// stellar.ledger_entry_changes — linkage (ledger_seq, tx_hash, op_index)
// — then filters them to the event's own contract, applying EXACTLY the
// dispatcher's rule (see internal/dispatcher/state_write_keys.go for the
// ground-truth derivation on r1 ledger 62056824, where write_prices
// rewrites every REQUESTED feed's entry byte-identical and only the
// ACCEPTED feed's stored PriceData changes):
//
//   - pre-image  = the op's FIRST `state` row for the key
//     (ContractDataEntry.Val bytes from entry_xdr);
//   - post-image = the op's LAST `created`/`updated` row for the key;
//   - changed    = post exists AND (no pre-image OR pre.Val != post.Val).
//
// Cost model: the fetch is per-EVENT-batch, not per-window — one point
// query per stateWriteKeyBatch streamed events, each a primary-key
// prefix lookup ((ledger_seq, tx_hash, op_index) is the table's ORDER BY
// prefix) additionally bounded by the batch's ledger span for partition
// pruning. It is opt-in per source (like withOpArgs) because a
// firehose-scale source would pay the lookups for keys its decoder never
// reads — today only redstone opts in.
//
// ReplacingMergeTree note: duplicate un-merged parts repeat rows with
// identical (ledger_seq, tx_hash, op_index, change_index) keys; the scan
// below dedups by change_index within each op, keeping the row with the
// LATEST ingested_at — the same row a FINAL merge would keep
// (ReplacingMergeTree(ingested_at): highest version wins) — so no FINAL
// is needed and a re-ingested correction row beats its stale
// predecessor regardless of read order.

// stateWriteKeyBatch is how many streamed events accumulate before one
// ledger_entry_changes lookup resolves their changed-write keys.
// Buffered events hold their wide OpArgs, so keep this small; 128 events
// ≈ well under a megabyte for the redstone payload class while cutting
// query count by two orders of magnitude vs per-event lookups.
const stateWriteKeyBatch = 128

// opRef identifies one operation in the lake.
type opRef struct {
	Ledger  uint32
	TxHash  string
	OpIndex int
}

// entryChangeLite is the slice of a ledger_entry_changes row the
// changed-write rule needs.
type entryChangeLite struct {
	ChangeIndex uint32
	ChangeType  string
	KeyXDR      string
	EntryXDR    string
}

// stateWriteKeyEnricher buffers streamed events, resolves their
// operations' value-changing contract-data write keys in one query per
// batch, and forwards each event — enriched and in arrival order — to fn.
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

// flush resolves changed-write keys for the buffered batch and forwards
// every buffered event. A lookup ERROR is fatal (the caller's stream
// fails — same contract as any lake read failure); an op with NO
// matching change rows simply yields no keys, so a coverage hole in
// ledger_entry_changes degrades consumers to their no-keys fallback
// rather than failing the stream.
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

// fetchOpEntryChanges runs one batched lookup for the distinct operations
// behind the buffered events and returns each op's contract-data change
// rows in change_index order, deduped across un-merged
// ReplacingMergeTree parts.
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

// opChangeAccumulator folds ORDER BY change_index rows into per-op
// change lists, deduping repeated change_index rows from un-merged
// ReplacingMergeTree parts by keeping the row with the LATEST
// ingested_at — the exact FINAL-merge semantics of
// ReplacingMergeTree(ingested_at). Keeping the FIRST-seen row (the
// pre-2026-07-31 behaviour) could resurrect a stale pre-correction row
// after a re-ingest, because read order among duplicate parts is not
// version order. Split out pure for unit tests.
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

// add folds one row. Rows arrive ORDER BY change_index within an op, so
// duplicates of one logical row are adjacent; a repeated change_index
// replaces the just-appended row when its version (ingested_at) is
// strictly newer. Ties keep the first-read row — duplicate parts with
// equal versions carry identical logical content.
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

// stateWriteKeysQuery renders the batched change-row lookup. The ledger
// BETWEEN bound prunes partitions; the tuple IN pins the exact ops (the
// table's ORDER BY prefix, so each is an index lookup, not a scan).
// tx_hash values come back FROM the lake (hex), but are quote-escaped
// like every other lake-sourced literal (see sqlQuoteEscaped). Split out
// pure for unit tests.
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

// changedWriteKeysForContract applies the dispatcher's changed-write rule
// to one op's change rows and returns the keys owned by contractID
// (C-strkey), in first-write change order — the per-event
// events.Event.StateWriteKeys slice. Any per-key parse failure excludes
// that key (fail toward the consumer's fallback, never a guess).
func changedWriteKeysForContract(rows []entryChangeLite, contractID string) []string {
	if len(rows) == 0 {
		return nil
	}
	var order []string
	states := make(map[string]*lakeKeyState)
	for _, r := range rows {
		owner, val, ok, decodable := contractDataEntryVal(r.EntryXDR)
		if !decodable {
			// Unparseable entry_xdr: the row's OWNER is unknowable, but
			// its KEY is not — key_xdr is a separate column. Poison the
			// KEY rather than dropping the ROW: dropping the row silently
			// discards a pre-image, which promotes an unchanged
			// identical-rewrite key to "changed" (a misattribution
			// vector), violating the documented "parse failure excludes
			// the key" rule the dispatcher twin already enforces via its
			// bad-set. Keys of other contracts embed their own contract
			// address in the LedgerKey, so a foreign row's poison can
			// never collide with this contract's keys.
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

// lakeKeyState accumulates one key's pre/post images across an op's lake
// change rows — the CH mirror of the dispatcher's keyChangeState.
type lakeKeyState struct {
	preVal  []byte
	postVal []byte
	hasPre  bool
	hasPost bool
	bad     bool
}

// record folds one row into the state: nil val marks the key bad, the
// FIRST `state` row becomes the pre-image, the LAST write row becomes
// the post-image. Returns true exactly on the key's first post-image
// (the caller's ordering signal).
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

// contractDataEntryVal parses a ledger_entry_changes entry_xdr and
// returns the owning contract's C-strkey plus the ContractDataEntry.Val
// bytes. decodable=false means the entry_xdr itself failed to
// unmarshal — the caller cannot even learn the owner and must poison the
// row's KEY (see changedWriteKeysForContract) rather than drop the row.
// With decodable=true: ok=false when the entry is not contract-data (or
// is account-owned) — genuinely not ours, skip; a nil val with ok=true
// signals a Val marshal failure on a genuine contract-data entry —
// callers exclude that key.
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
