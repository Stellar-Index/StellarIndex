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

// SDEX live-offer reads (order-book substrate).
//
// The classic order book is the set of LIVE `offer` ledger entries. The lake
// holds them two ways:
//
//   - stellar.ledger_entries_current — ReplacingMergeTree(version) keyed
//     (entry_type, key_xdr): the latest version per offer key, but ~1.02B
//     offer ROWS on r1 including every dead offer's trailing state, with the
//     selling/buying assets only inside the entry_xdr blob (no asset columns
//     for offers, so no per-pair predicate is possible without decoding).
//   - stellar.ledger_entry_changes — the append-only change stream,
//     partitioned by ledger_seq (cheap partition-pruned incremental reads).
//
// Serving a per-pair book straight off either table is therefore not honest
// within an explorer read budget: a pair filter requires decoding entry_xdr
// row by row across the whole offer slice. The cheapest honest design —
// chosen over a materialized offers projection (needs Go-side XDR decode at
// insert, i.e. a new ingest writer, ADR-0031 territory) — is an IN-PROCESS
// live book: one full-slice load at process start ([LoadLiveOffers]), then
// small partition-pruned increments ([OfferChangesSince]) applied to an
// in-memory map (internal/api/v1's SDEXOrderBookCache). Trade-offs, stated
// plainly: the initial load streams the whole offer slice once per process
// start (minutes of IO on r1, bounded work-shape below); the served book
// then lags the lake tip by at most the advance cadence; and process memory
// carries the live book (tens of thousands of offers — small).
//
// Work-shape bound for the full-slice read (same reasoning as
// [boundedScanSettings]): FINAL streams a merge over the table's own
// (entry_type, key_xdr) sort order — memory is merge-buffer-bounded, NOT
// key-cardinality-bounded, which is why FINAL is preferred here over a
// GROUP BY/argMax whose hash state would scale with the ~hundreds of
// millions of distinct offer keys ever created. max_threads=4 pins the
// fan-out (the 2026-07 campaign measured default fan-out costing 40× the
// memory of a pinned scan on this table's part layout).
const liveOfferScanSettings = "SETTINGS max_threads = 4, max_memory_usage = 8589934592"

// LiveOffer is one live classic offer decoded from its ledger entry.
// Amounts/prices are classic protocol types (int64 stroops, int32 price
// rationals) — NOT Soroban i128s; aggregation over many offers still uses
// big math on the caller side (sums can exceed int64).
type LiveOffer struct {
	// KeyXDR is the entry's LedgerKey (base64) — the book's map key.
	KeyXDR string
	// OfferID is the protocol offer id.
	OfferID int64
	// Seller is the offer owner's G-strkey.
	Seller string
	// Selling / Buying are canonical asset ids (`native`, `CODE-G...`).
	Selling string
	Buying  string
	// Amount is the remaining SELLING amount, in stroops (7-decimal).
	Amount int64
	// PriceN/PriceD is the exact price rational: buying units per
	// selling unit = N/D.
	PriceN int32
	PriceD int32
	// Ledger is the ledger this state was set at; Version orders states
	// of the same key ((ledger<<32)|intra — the table's own version).
	Ledger  uint32
	Version uint64
}

// OfferChange is one offer-entry change from the incremental stream.
// Removed=true means the offer left the book (taken or cancelled);
// Offer is only populated when Removed is false.
type OfferChange struct {
	KeyXDR  string
	Removed bool
	Version uint64
	Ledger  uint32
	Offer   LiveOffer
}

// offerBookLoadHoleLookback is how far below the lake tip a full load
// looks for an unhealed hole when it picks the book's starting cursor.
// The cursor has no predecessor at load time, and a contiguity scan from
// the lake floor is a whole-lake window sort (over the CH memory cap), so
// the load anchors at the first ledger present in the last 100k — about
// six days at the 5 s close cadence, against a healer that runs every ten
// minutes. A hole older than that has outlived the healer by days and is
// an operator incident, not a cursor concern; the periodic re-load in the
// cache re-anchors once it is filled.
const offerBookLoadHoleLookback = 100_000

// offerBookTip is the order book's hole-safe upper read bound: the
// highest ledger reachable from `from` without crossing a ledger the lake
// does not hold. anchored=true is the incremental case — `from` is
// cursor+1, so if `from` itself is missing the answer is from-1 and the
// cursor HOLDS until catch-up fills it. anchored=false is the case with no
// cursor to continue from (a full load, or a book loaded off an empty
// lake): `from` is only a floor, so the run starts at the first ledger
// actually present at or above it — every lake begins at ledger 2, and an
// anchored read from 1 would report a boundary hole forever.
func offerBookTip(from uint32, anchored bool, lc ledgerContiguity) uint32 {
	if !anchored && lc.minPresent > from {
		from = lc.minPresent
	}
	return watermark(from, lc.lakeMax, lc.firstGap, lc.minPresent)
}

// offerBookLoadCursor picks the cursor a full load hands the cache: the
// contiguous tip of the lake's recent window, read BEFORE the offer scan.
// A plain max(ledger_seq) here would start the book ABOVE any hole that is
// open at load time, and the ledger catch-up later writes into that hole
// would sit below the cursor for the life of the process.
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

// LoadLiveOffers streams every LIVE offer entry from the lake's
// current-state projection, returning the offers plus the lake's
// CONTIGUOUS tip read BEFORE the scan started ([offerBookLoadCursor]) —
// the caller's incremental cursor. Everything above that cursor —
// changes landing during the scan, and ledgers the scan saw above a
// still-open hole — is re-read by [OfferChangesSince] and re-applied
// idempotently by version. Undecodable entries are skipped (counted by
// the caller via len). See the package comment above for the design
// trade-offs.
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

// OfferChangesSince streams offer-entry changes with fromLedger <
// ledger_seq <= the lake's CONTIGUOUS tip above fromLedger, in
// (ledger_seq, intra_ledger_seq) order, returning the changes and the
// new cursor. The read is partition-pruned by ledger_seq, so a 60s
// cadence costs a few small partitions at most. Duplicate/overlapping
// rows are safe: the caller applies changes by version, idempotently.
//
// The upper bound is [offerBookTip], never max(ledger_seq): the returned
// cursor is committed by the caller, and the LiveSink drops whole ledgers
// under pressure. Reading to the raw max crosses such a hole and commits
// a cursor above it, so the rows ch-live-catchup later writes INTO the
// hole are below the cursor forever — an offer removed in the dropped
// ledger is served as resting liquidity until the process restarts
// (audit 2026-09-02 F162). Bounded by the contiguous tip the cursor holds
// just below the hole and resumes through it once it is filled — the
// same guard as projector.resolveTip and chops.Cap67Range.
func (r *ExplorerReader) OfferChangesSince(ctx context.Context, fromLedger uint32) ([]OfferChange, uint32, error) {
	lc, err := ledgerContiguityFrom(ctx, r.conn, fromLedger+1)
	if err != nil {
		return nil, 0, err
	}
	// fromLedger == 0 is a book loaded off an empty lake: no cursor to
	// continue from, so start at the lake's first ledger (see offerBookTip).
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
				// A skipped non-removed change FREEZES this key's
				// previously-applied state in the served book (the update
				// it carried is lost until the key's next decodable
				// change) — surface it instead of dropping it silently
				// (audit 2026-07-31). Offer entries are core-emitted XDR,
				// so any increment here points at a lake problem upstream.
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

// offerVersion mirrors ledger_entries_current's materialized version:
// (ledger_seq << 32) | intra_ledger_seq.
func offerVersion(ledger, intra uint32) uint64 {
	return uint64(ledger)<<32 | uint64(intra)
}

// OfferRemovalRef identifies one version-tie-suspect book entry: an
// offer's LedgerKey plus the ledger its winning current-state row was
// recorded at.
type OfferRemovalRef struct {
	KeyXDR string
	Ledger uint32
}

// offerRemovalProbeBatch bounds one OfferRemovedAt query. Each ref
// prunes to its own ledger's granules via the (ledger_seq, …) primary
// key, so per-batch cost is ~linear in refs; 500 scattered old-era
// refs measured 0.77s / trivial memory on r1 (2026-07-31).
const offerRemovalProbeBatch = 500

// OfferRemovedAt reports which of the given offers have a `removed`
// change row AT THE SAME LEDGER as their winning current-state row.
//
// Why this exists — the zombie-offer class (2026-07-31): historical
// backfill wrote ledger_entry_changes rows with intra_ledger_seq = 0,
// so every same-ledger change to one key ties on
// ledger_entries_current's ReplacingMergeTree version and an ARBITRARY
// row survives the merge. An offer that was updated then fully
// consumed within one ledger can survive as `updated` — a phantom
// "live" offer years after it left the chain (founding case: XLM/USDC
// offers 845025288/845025425/845025699/845028065, consumed 2021-11-10
// at ledger 38224736+, still "live" in the book on 2026-07-31 and
// serving a crossed best bid 0.4327 vs best ask 0.1722). The losing
// `removed` row is physically gone from ledger_entries_current after
// the merge, but ledger_entry_changes still holds it — this probe
// recovers the truth with a partition-pruned point read.
//
// The same-ledger check is sufficient for winners of the current-state
// FINAL scan: an offer LedgerKey embeds the protocol offer ID and is
// never reused, so a removal at a LATER ledger would itself have been
// the higher-version winner (no tie), and a removal at an EARLIER
// ledger is impossible. Refs whose change rows are absent entirely
// (never-ingested windows) simply come back "not removed" — that
// residual class is what the crossed-pairs gauge watches.
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

// offerFromEntryXDR decodes one offer LedgerEntry. ok=false for
// undecodable bytes or a non-offer entry — refuse to guess.
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
