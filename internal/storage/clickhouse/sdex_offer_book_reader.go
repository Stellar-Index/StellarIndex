package clickhouse

import (
	"context"
	"fmt"

	"github.com/stellar/go-stellar-sdk/xdr"

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

// LoadLiveOffers streams every LIVE offer entry from the lake's
// current-state projection, returning the offers plus the
// ledger_entry_changes high-water ledger read BEFORE the scan started —
// the caller's incremental cursor (changes landing during the scan are
// re-read by the first [OfferChangesSince] and re-applied idempotently
// by version). Undecodable entries are skipped (counted by the caller
// via len). See the package comment above for the design trade-offs.
func (r *ExplorerReader) LoadLiveOffers(ctx context.Context) ([]LiveOffer, uint32, error) {
	_, cursor, err := entryChangeLedgerBounds(ctx, r.conn)
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
// ledger_seq <= the stream's current high-water ledger, in
// (ledger_seq, intra_ledger_seq) order, returning the changes and the
// new cursor. The read is partition-pruned by ledger_seq, so a 60s
// cadence costs a few small partitions at most. Duplicate/overlapping
// rows are safe: the caller applies changes by version, idempotently.
func (r *ExplorerReader) OfferChangesSince(ctx context.Context, fromLedger uint32) ([]OfferChange, uint32, error) {
	_, tip, err := entryChangeLedgerBounds(ctx, r.conn)
	if err != nil {
		return nil, 0, err
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
