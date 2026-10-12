package clickhouse

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// SDEX live-offer reads (order-book substrate). ledger_entries_current holds ~1.02B offer rows including dead offers' trailing state and
// the assets live only inside entry_xdr, so no per-pair predicate is possible without decoding: the book is IN-PROCESS: one full load at
// start ([LoadLiveOffers]), then partition-pruned increments ([OfferChangesSince]) into SDEXOrderBookCache, lagging the tip by one
// advance cadence. FINAL streams a merge over (entry_type, key_xdr), so memory is merge-buffer-bounded, unlike GROUP BY/argMax;
// max_threads=4 pins fan-out (default used ~40x the memory). optimize_move_to_prewhere_if_final is off, as in blendReserveStateQuery:
// PREWHERE on change_type ahead of the FINAL collapse can surface a superseded offer version.
const liveOfferScanSettings = "SETTINGS max_threads = 4, max_memory_usage = 8589934592, optimize_move_to_prewhere_if_final = 0"

// LiveOffer is one live classic offer; amounts and prices are int64 stroops and int32 price rationals (not i128), so callers aggregate with big math.
type LiveOffer struct {
	KeyXDR string

	OfferID int64

	Seller string
	// Selling / Buying are canonical asset ids (`native`, `CODE-G...`).
	Selling string
	Buying  string

	Amount int64
	// PriceN/PriceD: buying units per selling unit.
	PriceN int32
	PriceD int32
	// Version orders states of one key: (ledger<<32)|intra.
	Ledger  uint32
	Version uint64
}

// OfferChange is one offer-entry change; Offer is populated only when Removed is false.
type OfferChange struct {
	KeyXDR  string
	Removed bool
	Version uint64
	Ledger  uint32
	Offer   LiveOffer
}

// offerBookLoadHoleLookback is how far below the tip a full load looks for an unhealed hole: a scan from the lake floor is a whole-lake
// window sort (over the CH memory cap), so the load anchors in the last 100k ledgers (~6 days); an older hole is an operator incident,
// and the periodic re-load re-anchors.
const offerBookLoadHoleLookback = 100_000

// offerBookTip is the hole-safe upper read bound: the highest ledger reachable from `from` without crossing a missing one. anchored=true
// (`from` is cursor+1): a missing `from` returns from-1 and the cursor HOLDS. anchored=false (no cursor): `from` is only a floor and the
// run starts at the first present ledger.
func offerBookTip(from uint32, anchored bool, lc ledgerContiguity) uint32 {
	if !anchored && lc.minPresent > from {
		from = lc.minPresent
	}
	return watermark(from, lc.lakeMax, lc.firstGap, lc.minPresent)
}

// offerBookLoadCursor is the contiguous tip of the recent window, read BEFORE the
// offer scan; a plain max(ledger_seq) would start the book above any open hole.
func (r *ExplorerReader) offerBookLoadCursor(ctx context.Context) (uint32, error) {
	var lakeMax uint64
	if err := r.conn.QueryRow(ctx, `SELECT toUInt64(max(ledger_seq)) FROM stellar.ledgers`).Scan(&lakeMax); err != nil {
		return 0, fmt.Errorf("clickhouse: offer book lake tip: %w", err)
	}
	if lakeMax == 0 {
		return 0, nil // empty lake — nothing applied, nothing to skip
	}
	var floor uint32
	if lakeMax > offerBookLoadHoleLookback {
		floor = uint32(lakeMax) - offerBookLoadHoleLookback // ledger sequences fit uint32
	}
	lc, err := ledgerContiguityFrom(ctx, r.conn, floor)
	if err != nil {
		return 0, err
	}
	return offerBookTip(floor, false, lc), nil
}

// LoadLiveOffers streams every live offer from the current-state projection and returns the CONTIGUOUS tip read BEFORE the scan as the incremental cursor; undecodable entries are skipped.
func (r *ExplorerReader) LoadLiveOffers(ctx context.Context) ([]LiveOffer, uint32, error) {
	cursor, err := r.offerBookLoadCursor(ctx)
	if err != nil {
		return nil, 0, err
	}

	q := `SELECT key_xdr, ledger_seq, intra_ledger_seq, entry_xdr
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'offer' AND change_type != 'removed' AND entry_xdr != ''
		` + liveOfferScanSettings
	rows, err := r.conn.Query(ctx, q)
	if err != nil {
		return nil, 0, fmt.Errorf("clickhouse: live offers scan: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []LiveOffer
	for rows.Next() {
		var keyXDR, entryXDR string
		var ledger, intra uint32
		if err := rows.Scan(&keyXDR, &ledger, &intra, &entryXDR); err != nil {
			return nil, 0, fmt.Errorf("clickhouse: scan live offer: %w", err)
		}
		o, ok := offerFromEntryXDR(entryXDR)
		if !ok {
			continue
		}
		o.KeyXDR = keyXDR
		o.Ledger = ledger
		o.Version = offerVersion(ledger, intra)
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("clickhouse: live offers rows: %w", err)
	}
	return out, cursor, nil
}

// OfferChangesSince streams offer changes in (fromLedger, contiguous tip], bounded by the contiguous tip (never max(ledger_seq), which could cross a LiveSink hole); overlapping rows are safe as the caller applies by version.
func (r *ExplorerReader) OfferChangesSince(ctx context.Context, fromLedger uint32) ([]OfferChange, uint32, error) {
	lc, err := ledgerContiguityFrom(ctx, r.conn, fromLedger+1)
	if err != nil {
		return nil, 0, err
	}
	// fromLedger == 0 is a book loaded off an empty lake: start at the first ledger.
	tip := offerBookTip(fromLedger+1, fromLedger > 0, lc)
	if tip < lc.lakeMax {
		slog.Warn("sdex order book: advance held below a lake hole; the book lags until ch-live-catchup fills it",
			"cursor", fromLedger, "contiguous_tip", tip, "lake_max", lc.lakeMax)
	}
	if tip <= fromLedger {
		return nil, fromLedger, nil
	}

	q := `SELECT key_xdr, ledger_seq, intra_ledger_seq, change_type, entry_xdr
		FROM stellar.ledger_entry_changes
		WHERE entry_type = 'offer' AND ledger_seq > ? AND ledger_seq <= ?
		ORDER BY ledger_seq, intra_ledger_seq
		` + liveOfferScanSettings
	rows, err := r.conn.Query(ctx, q, fromLedger, tip)
	if err != nil {
		return nil, 0, fmt.Errorf("clickhouse: offer changes scan: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []OfferChange
	for rows.Next() {
		var keyXDR, changeType, entryXDR string
		var ledger, intra uint32
		if err := rows.Scan(&keyXDR, &ledger, &intra, &changeType, &entryXDR); err != nil {
			return nil, 0, fmt.Errorf("clickhouse: scan offer change: %w", err)
		}
		ch := OfferChange{
			KeyXDR:  keyXDR,
			Version: offerVersion(ledger, intra),
			Ledger:  ledger,
			Removed: changeType == "removed",
		}
		if !ch.Removed {
			o, ok := offerFromEntryXDR(entryXDR)
			if !ok {
				// A skipped non-removed change FREEZES the key's prior state in the served book, so
				// surface it rather than drop it silently. Offer entries are core-emitted XDR, so
				// any increment points at a lake problem upstream.
				slog.Warn("sdex order book: undecodable non-removed offer change skipped; key's prior state frozen",
					"key_xdr", keyXDR, "ledger", ledger)
				obs.SDEXOrderBookUndecodableOffersTotal.Inc()
				continue
			}
			o.KeyXDR = keyXDR
			o.Ledger = ledger
			o.Version = ch.Version
			ch.Offer = o
		}
		out = append(out, ch)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("clickhouse: offer changes rows: %w", err)
	}
	return out, tip, nil
}

// offerVersion mirrors ledger_entries_current's version: (ledger_seq << 32) | intra.
func offerVersion(ledger, intra uint32) uint64 {
	return uint64(ledger)<<32 | uint64(intra)
}

// OfferRemovalRef is an offer's LedgerKey plus the ledger its winning current-state row was recorded at.
type OfferRemovalRef struct {
	KeyXDR string
	Ledger uint32
}

// offerRemovalProbeBatch bounds one OfferRemovedAt query; each ref prunes to its
// ledger's granules, so cost is ~linear in refs.
const offerRemovalProbeBatch = 500

// OfferRemovedAt reports which offers have a `removed` change row at the SAME ledger as their winning current-state row, catching phantom live offers when backfilled intra_ledger_seq ties let the wrong RMT row win; refs without change rows are "not removed".
func (r *ExplorerReader) OfferRemovedAt(ctx context.Context, refs []OfferRemovalRef) (map[string]struct{}, error) {
	removed := make(map[string]struct{})
	for start := 0; start < len(refs); start += offerRemovalProbeBatch {
		batch := refs[start:min(start+offerRemovalProbeBatch, len(refs))]
		var b strings.Builder
		args := make([]any, 0, len(batch)*2)
		for i, ref := range batch {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString("(?,?)")
			args = append(args, ref.Ledger, ref.KeyXDR)
		}
		q := `SELECT DISTINCT key_xdr
			FROM stellar.ledger_entry_changes
			WHERE entry_type = 'offer' AND change_type = 'removed'
			  AND (ledger_seq, key_xdr) IN (` + b.String() + `)
			SETTINGS max_threads = 2, max_memory_usage = 4294967296`
		rows, err := r.conn.Query(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("clickhouse: offer removal probe: %w", err)
		}
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("clickhouse: scan offer removal: %w", err)
			}
			removed[key] = struct{}{}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, fmt.Errorf("clickhouse: offer removal rows: %w", err)
		}
	}
	return removed, nil
}

// offerFromEntryXDR decodes one offer LedgerEntry; ok=false for undecodable bytes or
// a non-offer entry.
func offerFromEntryXDR(b64 string) (LiveOffer, bool) {
	var le xdr.LedgerEntry
	if xdr.SafeUnmarshalBase64(b64, &le) != nil {
		return LiveOffer{}, false
	}
	o, ok := le.Data.GetOffer()
	if !ok {
		return LiveOffer{}, false
	}
	return LiveOffer{
		OfferID: int64(o.OfferId),
		Seller:  o.SellerId.Address(),
		Selling: xdrjson.AssetID(o.Selling),
		Buying:  xdrjson.AssetID(o.Buying),
		Amount:  int64(o.Amount),
		PriceN:  int32(o.Price.N),
		PriceD:  int32(o.Price.D),
	}, true
}
