//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestSDEXOrderBook_ConvergesAfterLakeHoleIsFilled is the served-data proof
// for the order book's cursor discipline (audit 2026-09-02, F162).
//
// The LiveSink drops a WHOLE ledger under buffer pressure, leaving a hole in
// the lake that ch-live-catchup back-fills minutes later. The pre-fix reader
// bounded its incremental read by max(ledger_seq) of ledger_entry_changes,
// so Advance read straight across the hole and committed the cursor PAST it;
// the healed rows then landed BELOW the cursor and were never read. An offer
// removed in the dropped ledger stayed on /v1/sdex/orderbook as resting
// liquidity — and an offer created in it never appeared — until the API
// process restarted.
//
// This drives the REAL reader and the REAL cache through the REAL endpoint
// against a real ClickHouse, in the production order of events:
//
//	B+1  offer A created                   → Load
//	B+2  offer A REMOVED, offer B created  ← this ledger is DROPPED
//	B+3  (empty), B+4 offer C created      → Advance  (hole still open)
//	B+2  healed by catch-up                → Advance  (no restart, no re-Load)
//
// and asserts both halves of the contract: while the hole is open the cursor
// HOLDS at B+1 (it never crosses the hole), and once the hole is filled the
// SAME cache advances to B+4 and serves exactly {B, C}.
//
// Proven red: on the pre-fix reader the final book is {A, C} — phantom A
// still served, B never seen — with as_of_ledger already at B+4 after the
// first Advance.
func TestSDEXOrderBook_ConvergesAfterLakeHoleIsFilled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// An isolated range ABOVE every other ledger the suite seeds (the
	// watermark tests sit at 215M/216M): the book's Load cursor is derived
	// from the lake's global tip, so this test's ledgers must be that tip.
	const base = uint32(217_000_000)
	const (
		sellerAccount = "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
		issuerAccount = "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
		assetCode     = "LAKEHOLE"
	)
	selling := assetCode + "-" + issuerAccount
	// Shared-ClickHouse isolation: TestNetworkThroughput_DedupsReingestedLedger
	// anchors its window on the GLOBAL max(close_time) and reserves 2027-06-15
	// as that tip. Nothing here keys on close_time (the book orders by
	// ledger_seq), so stay far below it rather than compete for the tip.
	closeTime := time.Date(2025, 3, 3, 4, 5, 6, 0, time.UTC)

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })

	// land writes one whole ledger the way the live sink / ch-backfill does:
	// its entry changes AND its stellar.ledgers commit marker, in one extract.
	land := func(seq uint32, changes ...chstore.LedgerEntryChangeRow) {
		t.Helper()
		for i := range changes {
			changes[i].LedgerSeq = seq
			changes[i].CloseTime = closeTime
		}
		ext := chstore.LedgerExtract{
			Ledger: chstore.LedgerRow{
				LedgerSeq: seq, CloseTime: closeTime,
				LedgerHash: "aa62", PrevHash: "bb62", ProtocolVersion: 23, BucketListHash: "cc62",
				TotalCoins: 1, FeePool: 1, BaseFee: 100, BaseReserve: 5_000_000,
			},
			Changes: changes,
		}
		if err := sink.Add(ctx, ext); err != nil {
			t.Fatalf("sink add ledger %d: %v", seq, err)
		}
		if err := sink.Flush(ctx); err != nil {
			t.Fatalf("flush ledger %d: %v", seq, err)
		}
	}
	created := func(offerID int64, priceN int32) chstore.LedgerEntryChangeRow {
		return chstore.LedgerEntryChangeRow{
			TxHash: "f162", OpIndex: 0, ChangeIndex: 0, IntraLedgerSeq: 3,
			ChangeType: "created", EntryType: "offer", AccountID: sellerAccount,
			KeyXDR:   lakeHoleOfferKeyB64(t, sellerAccount, offerID),
			EntryXDR: lakeHoleOfferEntryB64(t, sellerAccount, offerID, assetCode, issuerAccount, priceN),
		}
	}
	// A distinct tx from `created`: ledger_entry_changes is a
	// ReplacingMergeTree over (ledger_seq, tx_hash, op_index, change_index),
	// so two same-ledger rows sharing that identity collapse into one.
	removed := func(offerID int64) chstore.LedgerEntryChangeRow {
		return chstore.LedgerEntryChangeRow{
			TxHash: "f162-take", OpIndex: 0, ChangeIndex: 0, IntraLedgerSeq: 2,
			ChangeType: "removed", EntryType: "offer", AccountID: sellerAccount,
			KeyXDR: lakeHoleOfferKeyB64(t, sellerAccount, offerID),
		}
	}
	const offerA, offerB, offerC = int64(9_162_001), int64(9_162_002), int64(9_162_003)

	// ── Process start: [B, B+1] landed; offer A rests at price 2. ─────────
	land(base)
	land(base+1, created(offerA, 2))

	reader, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	cache := v1.NewSDEXOrderBookCache(reader, nil)
	if err := cache.Load(ctx); err != nil {
		t.Fatalf("cache.Load: %v", err)
	}
	ts := httptest.NewServer(v1.New(v1.Options{SDEXOrderBook: cache}).Handler())
	t.Cleanup(ts.Close)
	book := func() v1.SDEXOrderBookView {
		t.Helper()
		resp, err := http.Get(ts.URL + "/v1/sdex/orderbook?selling=" + selling + "&buying=native")
		if err != nil {
			t.Fatalf("GET orderbook: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET orderbook status = %d, want 200", resp.StatusCode)
		}
		var env struct {
			Data v1.SDEXOrderBookView `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("decode orderbook: %v", err)
		}
		return env.Data
	}
	askPrices := func(b v1.SDEXOrderBookView) []string {
		out := make([]string, 0, len(b.Asks))
		for _, lvl := range b.Asks {
			out = append(out, lvl.Price)
		}
		return out
	}

	loaded := book()
	if loaded.AsOfLedger != base+1 {
		t.Fatalf("precondition: as_of_ledger after Load = %d, want %d — this test's range must be the "+
			"lake's global tip; another test now seeds stellar.ledgers above %d", loaded.AsOfLedger, base+1, base)
	}
	if got := askPrices(loaded); len(got) != 1 || got[0] != "2.0000000" {
		t.Fatalf("precondition: asks after Load = %v, want [2.0000000] (offer A)", got)
	}

	// ── The drop: B+2 (A removed, B created) never lands; B+3, B+4 do. ────
	land(base + 3)
	land(base+4, created(offerC, 5))
	if err := cache.Advance(ctx); err != nil {
		t.Fatalf("Advance across the open hole: %v", err)
	}
	held := book()
	if held.AsOfLedger != base+1 {
		t.Errorf("as_of_ledger with the hole at %d still open = %d, want %d — the cursor must HOLD below "+
			"a lake hole; past it, the healed ledger's offer changes land below the cursor and are never read",
			base+2, held.AsOfLedger, base+1)
	}

	// ── The heal: ch-live-catchup back-fills B+2. Same cache, no restart. ─
	land(base+2, removed(offerA), created(offerB, 3))
	if err := cache.Advance(ctx); err != nil {
		t.Fatalf("Advance after the heal: %v", err)
	}
	healed := book()
	if healed.AsOfLedger != base+4 {
		t.Errorf("as_of_ledger after the hole healed = %d, want %d — a held cursor must RESUME to the "+
			"lake tip once the hole is filled (holding forever is not a fix)", healed.AsOfLedger, base+4)
	}
	got := askPrices(healed)
	if len(got) != 2 || got[0] != "3.0000000" || got[1] != "5.0000000" {
		t.Fatalf("asks after the hole healed = %v (ask_offers=%d), want [3.0000000 5.0000000]: offer A was "+
			"REMOVED in the healed ledger %d and must leave the book (a 2.0000000 level is the phantom), "+
			"offer B was CREATED in it and must appear, offer C landed above it",
			got, healed.AskOffers, base+2)
	}
}

func lakeHoleOfferKeyB64(t *testing.T, seller string, offerID int64) string {
	t.Helper()
	var key xdr.LedgerKey
	if err := key.SetOffer(xdr.MustAddress(seller), uint64(offerID)); err != nil {
		t.Fatalf("offer ledger key: %v", err)
	}
	b64, err := xdr.MarshalBase64(key)
	if err != nil {
		t.Fatalf("marshal offer key: %v", err)
	}
	return b64
}

// lakeHoleOfferEntryB64 builds an offer LedgerEntry selling 10 units of
// code-issuer for native at priceN/1. lastModifiedLedgerSeq is left zero:
// the book versions an offer by its change row's own ledger, not the entry's.
func lakeHoleOfferEntryB64(t *testing.T, seller string, offerID int64, code, issuer string, priceN int32) string {
	t.Helper()
	entry := xdr.LedgerEntry{
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeOffer,
			Offer: &xdr.OfferEntry{
				SellerId: xdr.MustAddress(seller),
				OfferId:  xdr.Int64(offerID),
				Selling:  xdr.MustNewCreditAsset(code, issuer),
				Buying:   xdr.MustNewNativeAsset(),
				Amount:   100_000_000,
				Price:    xdr.Price{N: xdr.Int32(priceN), D: 1},
			},
		},
	}
	b64, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatalf("marshal offer entry: %v", err)
	}
	return b64
}
