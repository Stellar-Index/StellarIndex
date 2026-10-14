package explorer

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/entryhistory"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

const (
	// entryLedgerBatch is how many indexed ledgers one pass streams through the projector; entryLedgerPasses caps
	// the passes so a sparse filter cannot walk the whole index inside one request.
	entryLedgerBatch  = 32
	entryLedgerPasses = 8
	// Lake partitions 39-53 were derived before intra_ledger_seq took its current meaning; every page names the range.
	entryChangesOldNumberingFrom = 39_000_000
	entryChangesOldNumberingThru = 53_999_999
	// entryCursorBelowLedger is the tx hash of a cursor that resumes strictly below its ledger.
	entryCursorBelowLedger = "-"
)

// entryChangeKey is the part of a projected row the page walker orders, cuts and resumes on.
type entryChangeKey struct {
	Ledger         uint32
	IntraLedgerSeq uint32
	TxHash         string
	OpIndex        int32
	ChangeIndex    uint32
	Role           string
}

func accountEntryKey(a clickhouse.AccountEntryChange) entryChangeKey {
	return entryChangeKey{Ledger: a.Ledger, IntraLedgerSeq: a.IntraLedgerSeq, TxHash: a.TxHash, OpIndex: a.OpIndex, ChangeIndex: a.ChangeIndex, Role: a.Role}
}

func assetEntryKey(a clickhouse.AssetEntryChange) entryChangeKey {
	return entryChangeKey{Ledger: a.Ledger, IntraLedgerSeq: a.IntraLedgerSeq, TxHash: a.TxHash, OpIndex: a.OpIndex, ChangeIndex: a.ChangeIndex, Role: a.Role}
}

// entryPage is one cut page of projected rows: a full page continues from its last row; a short page that stopped
// at the pass cap continues strictly below the last ledger walked and is a lower bound.
type entryPage[T any] struct {
	rows       []T
	next       string
	lowerBound bool
}

// entryPageSource is how one feed reaches its index and the lake: ledgers lists the key's indexed ledgers at or
// below a ceiling, newest first; keep appends the feed's rows out of one projected source row; key positions a row.
type entryPageSource[T any] struct {
	ledgers func(ctx context.Context, ceiling uint32) ([]uint32, error)
	stream  func(ctx context.Context, ledgers []uint32, fn func(clickhouse.EntryHistorySourceRow) error) error
	keep    func(out []T, acct []clickhouse.AccountEntryChange, asset []clickhouse.AssetEntryChange) []T
	key     func(T) entryChangeKey
}

// walkEntryPage walks the indexed ledgers newest first, a batch at a time, until the page is full, the index is
// exhausted or the pass cap is hit.
func walkEntryPage[T any](ctx context.Context, src entryPageSource[T], limit int, cur clickhouse.AssetEntryChangeCursor, wm uint32) (entryPage[T], error) {
	ceiling := wm
	if cur.IsSet() {
		c := cur.Ledger
		if cur.TxHash == entryCursorBelowLedger {
			c--
		}
		ceiling = min(ceiling, c)
	}
	var (
		rows []T
		more bool
		last uint32
	)
	for pass := 0; pass < entryLedgerPasses; pass++ {
		ledgers, err := src.ledgers(ctx, ceiling)
		if err != nil {
			return entryPage[T]{}, err
		}
		if len(ledgers) == 0 {
			more = false
			break
		}
		got, err := projectEntryRows(ctx, src, ledgers, cur)
		if err != nil {
			return entryPage[T]{}, err
		}
		rows = append(rows, got...)
		last = ledgers[len(ledgers)-1]
		more = len(ledgers) == entryLedgerBatch
		if len(rows) >= limit || !more {
			break
		}
		ceiling = last - 1
	}
	return finishEntryPage(rows, src.key, limit, more, last), nil
}

// finishEntryPage orders the rows newest first and cuts the page.
func finishEntryPage[T any](rows []T, key func(T) entryChangeKey, limit int, more bool, last uint32) entryPage[T] {
	slices.SortFunc(rows, func(a, b T) int { return compareEntryKey(key(b), key(a)) })
	page := entryPage[T]{rows: rows}
	if len(rows) >= limit {
		page.rows = rows[:limit]
		k := key(rows[limit-1])
		page.next = fmt.Sprintf("%d.%s.%d.%d.%s", k.Ledger, k.TxHash, k.OpIndex, k.ChangeIndex, k.Role)
	} else if more {
		page.next = fmt.Sprintf("%d.%s.-1.0.%s", last, entryCursorBelowLedger, entryCursorBelowLedger)
		page.lowerBound = true
	}
	return page
}

// projectEntryRows streams the listed ledgers through the projector and keeps the feed's rows that lie after the
// cursor. Unmerged ReplacingMergeTree duplicates are adjacent in sort-key order.
func projectEntryRows[T any](ctx context.Context, src entryPageSource[T], ledgers []uint32, cur clickhouse.AssetEntryChangeCursor) ([]T, error) {
	p := entryhistory.NewProjector()
	var (
		out      []T
		prev     clickhouse.EntryHistorySourceRow
		curSeq   uint32
		curFound bool
	)
	err := src.stream(ctx, ledgers, func(s clickhouse.EntryHistorySourceRow) error {
		if sameEntryChange(s, prev) {
			return nil
		}
		prev = s
		if isEntryCursorRow(s, cur) {
			curSeq, curFound = s.IntraLedgerSeq, true
		}
		acct, asset, err := p.Project(s)
		if err != nil {
			return fmt.Errorf("ledger %d change %s/%d/%d: %w", s.Ledger, s.TxHash, s.OpIndex, s.ChangeIndex, err)
		}
		out = src.keep(out, acct, asset)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if cur.IsSet() {
		out = slices.DeleteFunc(out, func(a T) bool { return !afterEntryCursor(src.key(a), cur, curSeq, curFound) })
	}
	return out, nil
}

func sameEntryChange(a, b clickhouse.EntryHistorySourceRow) bool {
	return a.Ledger == b.Ledger && a.TxHash == b.TxHash && a.OpIndex == b.OpIndex && a.ChangeIndex == b.ChangeIndex
}

func isEntryCursorRow(s clickhouse.EntryHistorySourceRow, cur clickhouse.AssetEntryChangeCursor) bool {
	return cur.IsSet() && s.Ledger == cur.Ledger && s.TxHash == cur.TxHash && s.OpIndex == cur.OpIndex && s.ChangeIndex == cur.ChangeIndex
}

// afterEntryCursor reports whether k lies after the cursor in newest-first order; the cursor row's intra-ledger
// position is known only when the row itself was seen in the stream.
func afterEntryCursor(k entryChangeKey, cur clickhouse.AssetEntryChangeCursor, curSeq uint32, curFound bool) bool {
	if k.Ledger != cur.Ledger {
		return k.Ledger < cur.Ledger
	}
	if curFound && k.IntraLedgerSeq != curSeq {
		return k.IntraLedgerSeq < curSeq
	}
	return cmp.Or(strings.Compare(k.TxHash, cur.TxHash), cmp.Compare(k.OpIndex, cur.OpIndex),
		cmp.Compare(k.ChangeIndex, cur.ChangeIndex), strings.Compare(k.Role, cur.Role)) < 0
}

// compareEntryKey orders rows oldest first: ledger, apply order within the ledger, then the lake sort key.
func compareEntryKey(a, b entryChangeKey) int {
	return cmp.Or(cmp.Compare(a.Ledger, b.Ledger), cmp.Compare(a.IntraLedgerSeq, b.IntraLedgerSeq),
		strings.Compare(a.TxHash, b.TxHash), cmp.Compare(a.OpIndex, b.OpIndex),
		cmp.Compare(a.ChangeIndex, b.ChangeIndex), strings.Compare(a.Role, b.Role))
}

// parseEntryChangeCursor reads "ledger.tx_hash.op_index.change_index.role".
// op_index is -1 for tx-level changes and tx_hash is empty for upgrade-level ones.
func (h *Handler) parseEntryChangeCursor(w http.ResponseWriter, r *http.Request) (clickhouse.AssetEntryChangeCursor, bool) {
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return clickhouse.AssetEntryChangeCursor{}, true
	}
	bad := func() (clickhouse.AssetEntryChangeCursor, bool) {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-cursor",
			"Invalid cursor", http.StatusBadRequest,
			"cursor must be an opaque value returned in a prior next_cursor")
		return clickhouse.AssetEntryChangeCursor{}, false
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 5 || parts[4] == "" {
		return bad()
	}
	ledger, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil || ledger == 0 {
		return bad()
	}
	opIdx, err := strconv.ParseInt(parts[2], 10, 32)
	if err != nil || opIdx < -1 {
		return bad()
	}
	changeIdx, err := strconv.ParseUint(parts[3], 10, 32)
	if err != nil {
		return bad()
	}
	return clickhouse.AssetEntryChangeCursor{
		Ledger: uint32(ledger), TxHash: parts[1], OpIndex: int32(opIdx), ChangeIndex: uint32(changeIdx), Role: parts[4],
	}, true
}
