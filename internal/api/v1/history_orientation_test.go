// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// ─── /v1/history reads both stored directions ─────────────────────────
//
// A market has no stored direction of its own: the SDEX decoder records
// XLM/USDC and USDC/XLM as separate rows, and the page read keys on
// (base_asset, quote_asset) literally. Reading one direction answered
// `?base=AQUA&quote=USDC` with an empty page for every market recorded
// the other way round, while /v1/ohlc and /v1/chart served the same
// window from the same rows.
//
// The reader below is the trades table's own behaviour, which is what
// makes these assertions worth anything: a row lives under exactly ONE
// orientation, a read matches that orientation literally, and the
// cursor and limit are applied in the endpoint's keyset order. Nothing
// in it knows about flipping — every inversion under test is the
// handler's.

// orientedTradeStore is a HistoryReader backed by a fixed row set held
// the way `trades` holds it.
type orientedTradeStore struct {
	mu   sync.Mutex
	rows []canonical.Trade
	// pairs records every (base_asset, quote_asset) read, in order, so a
	// test can pin that both directions were asked for.
	pairs []string
	// maxRead is the largest `limit` this store was ever asked for, so a
	// test can see the completion re-read happen.
	maxRead int
	// srcLess is the database's COLLATION for the source column, and it
	// is swappable because the whole merge design turns on never
	// comparing that column in Go: a non-C collation weighs `-` and `_`
	// differently from their code points, so Go's order and the
	// database's can disagree. Nil is ascending byte order. A merge that
	// is genuinely collation-agnostic serves every row exactly once
	// under ANY setting of this, which is what
	// TestHistory_ExactlyOnceUnderEitherSourceCollation pins.
	srcLess func(a, b string) bool
}

// reset replaces the fixture this store answers from, so a model check
// can run thousands of drains against ONE http test server. Standing up
// a fresh listener per drain exhausts the machine's ephemeral ports —
// which it did, as `dial tcp: can't assign requested address`, and only
// when the package ran as a whole.
func (s *orientedTradeStore) reset(rows []canonical.Trade, srcLess func(a, b string) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows, s.srcLess = rows, srcLess
	s.pairs, s.maxRead = nil, 0
}

func (s *orientedTradeStore) readPairs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.pairs...)
}

// TradesInRangeAfter is the store's keyset page read: literal pair
// match, half-open [from, to), rows strictly after the full-PK cursor,
// ordered (ts, ledger, tx_hash, op_index, source) ASC, capped at limit.
func (s *orientedTradeStore) TradesInRangeAfter(
	_ context.Context, pair canonical.Pair, from, to, afterTs time.Time,
	afterLedger uint32, afterTxHash, afterSource string, afterOpIndex uint32, limit int,
) ([]canonical.Trade, error) {
	s.mu.Lock()
	s.pairs = append(s.pairs, pair.String())
	if limit > s.maxRead {
		s.maxRead = limit
	}
	rows, srcLess := s.rows, s.srcLess
	s.mu.Unlock()

	after := canonical.Trade{
		Timestamp: afterTs, Ledger: afterLedger,
		TxHash: afterTxHash, OpIndex: afterOpIndex, Source: afterSource,
	}
	matched := make([]canonical.Trade, 0, len(rows))
	for _, t := range rows {
		if !t.Pair.Equal(pair) {
			continue
		}
		if t.Timestamp.Before(from) || !t.Timestamp.Before(to) {
			continue
		}
		// An EMPTY afterSource is a real bind, not "no cursor": every
		// non-empty source beats it, which is exactly what the
		// past-group cursor relies on.
		if !afterTs.IsZero() && !storedKeyLess(srcLess, after, t) {
			continue
		}
		matched = append(matched, t)
	}
	sort.SliceStable(matched, func(i, j int) bool { return storedKeyLess(srcLess, matched[i], matched[j]) })
	if limit > 0 && len(matched) > limit {
		matched = matched[:limit]
	}
	return matched, nil
}

// storedKeyLess is the store's ORDER BY / cursor comparison — the full
// primary key, source last, exactly as the SQL tuple compares it, with
// the source column under this store's collation ([orientedTradeStore]).
func storedKeyLess(srcLess func(a, b string) bool, a, b canonical.Trade) bool {
	switch {
	case !a.Timestamp.Equal(b.Timestamp):
		return a.Timestamp.Before(b.Timestamp)
	case a.Ledger != b.Ledger:
		return a.Ledger < b.Ledger
	case a.TxHash != b.TxHash:
		return a.TxHash < b.TxHash
	case a.OpIndex != b.OpIndex:
		return a.OpIndex < b.OpIndex
	case srcLess != nil:
		return srcLess(a.Source, b.Source)
	default:
		return a.Source < b.Source
	}
}

func (s *orientedTradeStore) TradesInRange(
	ctx context.Context, pair canonical.Pair, from, to time.Time, limit int,
) ([]canonical.Trade, error) {
	return s.TradesInRangeAfter(ctx, pair, from, to, time.Time{}, 0, "", "", 0, limit)
}

func (s *orientedTradeStore) HistoryPoints(context.Context, canonical.Pair, string, int) ([]v1.HistoryPoint, error) {
	return nil, nil
}

func (s *orientedTradeStore) HistoryPointsInRange(context.Context, canonical.Pair, string, time.Time, time.Time, int) ([]v1.HistoryPoint, error) {
	return nil, nil
}

func (s *orientedTradeStore) TWAPPointsInRange(context.Context, canonical.Pair, string, time.Time, time.Time, int) ([]v1.HistoryPoint, error) {
	return nil, nil
}

func (s *orientedTradeStore) OHLCSeries(context.Context, canonical.Pair, string, time.Time, time.Time, int) ([]v1.OHLCSeriesBar, error) {
	return nil, nil
}

func (s *orientedTradeStore) LatestTradePerSource(context.Context, canonical.Pair, string) ([]canonical.Trade, error) {
	return nil, nil
}

// storedTrade builds one row held under `base`/`quote` with the given
// smallest-unit amounts. ts is seconds past the window start; ledger
// tracks it so the keyset order is unambiguous.
func storedTrade(t *testing.T, source string, sec int64, txSuffix string, base, quote canonical.Asset, baseAmt, quoteAmt int64) canonical.Trade {
	t.Helper()
	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		t.Fatalf("NewPair(%s, %s): %v", base, quote, err)
	}
	return canonical.Trade{
		Source:      source,
		Ledger:      uint32(1000 + sec),
		TxHash:      strings.Repeat("0", 64-len(txSuffix)) + txSuffix,
		OpIndex:     0,
		Timestamp:   orientationWindowStart.Add(time.Duration(sec) * time.Second),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(baseAmt)),
		QuoteAmount: canonical.NewAmount(big.NewInt(quoteAmt)),
	}
}

var orientationWindowStart = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

// historyPage is one decoded /v1/history response.
type historyPage struct {
	Data       []v1.TradeRow `json:"data"`
	Pagination *struct {
		Next string `json:"next"`
	} `json:"pagination"`
}

func getHistoryPage(t *testing.T, ts *testServer, query string) historyPage {
	t.Helper()
	resp := mustGet(t, ts.URL+"/v1/history?"+query)
	if resp.StatusCode != http.StatusOK {
		body, _ := json.Marshal(resp.Status)
		t.Fatalf("status = %d (%s)", resp.StatusCode, body)
	}
	var page historyPage
	mustDecode(t, resp, &page)
	return page
}

// orientationQuery is the request window every test below reads over.
func orientationQuery(base, quote canonical.Asset, limit int) string {
	return url.Values{
		"base":  {base.String()},
		"quote": {quote.String()},
		"from":  {orientationWindowStart.Format(time.RFC3339)},
		"to":    {orientationWindowStart.Add(time.Hour).Format(time.RFC3339)},
		"limit": {fmt.Sprint(limit)},
	}.Encode()
}

// TestHistory_ReverseStoredMarketServesInvertedRows is the defect
// itself: the market exists only as USDC/AQUA, and `?base=AQUA&quote=
// USDC` returned an empty page every time while /v1/ohlc served it.
//
// The inversion is pinned to the value, not to non-emptiness: the two
// legs swap, the two smallest-unit amounts swap with them, and `price`
// — rendered as quote/base — comes back as the exact reciprocal. The
// 7:1 row is the one that would expose a float round-trip: 1/(1/7)
// does not return 7 in binary floating point.
func TestHistory_ReverseStoredMarketServesInvertedRows(t *testing.T) {
	t.Parallel()
	usdc := mustParseAsset(t, usdcClassicID)
	aqua := mustParseAsset(t, aquaClassicID)

	store := &orientedTradeStore{rows: []canonical.Trade{
		// Stored USDC/AQUA: 7 USDC units bought 1 AQUA unit.
		storedTrade(t, "sdex", 10, "a1", usdc, aqua, 7, 1),
		// Stored USDC/AQUA: 1 USDC unit bought 3 AQUA units.
		storedTrade(t, "sdex", 20, "a2", usdc, aqua, 1, 3),
	}}
	ts := httpTestServer(t, v1.New(v1.Options{History: store}))

	page := getHistoryPage(t, ts, orientationQuery(aqua, usdc, 50))
	if len(page.Data) != 2 {
		t.Fatalf("rows = %d, want 2 — a market stored only as USDC/AQUA must serve under ?base=AQUA&quote=USDC", len(page.Data))
	}
	for i, want := range []struct {
		base, quote, price string
	}{
		{base: "1", quote: "7", price: "7.0000000000"},
		{base: "3", quote: "1", price: "0.3333333333"},
	} {
		got := page.Data[i]
		if got.BaseAsset != aqua.String() || got.QuoteAsset != usdc.String() {
			t.Errorf("row %d pair = %s/%s, want %s/%s — a flipped row is re-expressed in the requested orientation",
				i, got.BaseAsset, got.QuoteAsset, aqua, usdc)
		}
		if got.BaseAmount != want.base || got.QuoteAmount != want.quote {
			t.Errorf("row %d amounts = %s/%s, want %s/%s — the two legs swap with the pair",
				i, got.BaseAmount, got.QuoteAmount, want.base, want.quote)
		}
		if got.Price != want.price {
			t.Errorf("row %d price = %s, want %s — the flipped price is the exact reciprocal",
				i, got.Price, want.price)
		}
	}
	// Both directions were asked for, in the requested-first order.
	pairs := store.readPairs()
	if len(pairs) != 2 || pairs[0] != aqua.String()+"/"+usdc.String() || pairs[1] != usdc.String()+"/"+aqua.String() {
		t.Errorf("reads = %v, want the requested orientation then its flip", pairs)
	}
}

// TestHistory_StoredOrientationIsUntouched pins the other half: a row
// already held the way it was asked for is served byte-for-byte as the
// store returned it. The fold re-expresses, it does not renormalise.
func TestHistory_StoredOrientationIsUntouched(t *testing.T) {
	t.Parallel()
	usdc := mustParseAsset(t, usdcClassicID)
	aqua := mustParseAsset(t, aquaClassicID)

	store := &orientedTradeStore{rows: []canonical.Trade{
		storedTrade(t, "sdex", 10, "b1", aqua, usdc, 7, 1),
	}}
	ts := httpTestServer(t, v1.New(v1.Options{History: store}))

	page := getHistoryPage(t, ts, orientationQuery(aqua, usdc, 50))
	if len(page.Data) != 1 {
		t.Fatalf("rows = %d, want 1", len(page.Data))
	}
	got := page.Data[0]
	if got.BaseAsset != aqua.String() || got.QuoteAsset != usdc.String() {
		t.Errorf("pair = %s/%s, want %s/%s", got.BaseAsset, got.QuoteAsset, aqua, usdc)
	}
	if got.BaseAmount != "7" || got.QuoteAmount != "1" || got.Price != "0.1428571428" {
		t.Errorf("row = %s/%s @ %s, want 7/1 @ 0.1428571428", got.BaseAmount, got.QuoteAmount, got.Price)
	}
}

// TestHistory_BothDirectionsMergeInKeysetOrder pins the merge on a
// market the decoder recorded in both directions — the shape /v1/ohlc
// folds in SQL. Every row appears once, in the endpoint's order, each
// re-expressed by the orientation it was STORED in.
func TestHistory_BothDirectionsMergeInKeysetOrder(t *testing.T) {
	t.Parallel()
	usdc := mustParseAsset(t, usdcClassicID)
	aqua := mustParseAsset(t, aquaClassicID)

	store := &orientedTradeStore{rows: []canonical.Trade{
		storedTrade(t, "sdex", 10, "c1", aqua, usdc, 4, 1),  // requested-side
		storedTrade(t, "sdex", 20, "c2", usdc, aqua, 1, 5),  // flipped
		storedTrade(t, "sdex", 30, "c3", aqua, usdc, 8, 1),  // requested-side
		storedTrade(t, "sdex", 40, "c4", usdc, aqua, 1, 10), // flipped
	}}
	ts := httpTestServer(t, v1.New(v1.Options{History: store}))

	page := getHistoryPage(t, ts, orientationQuery(aqua, usdc, 50))
	if len(page.Data) != 4 {
		t.Fatalf("rows = %d, want 4 — both stored directions belong to one market", len(page.Data))
	}
	wantAmounts := [][2]string{{"4", "1"}, {"5", "1"}, {"8", "1"}, {"10", "1"}}
	for i, row := range page.Data {
		if row.BaseAsset != aqua.String() || row.QuoteAsset != usdc.String() {
			t.Errorf("row %d pair = %s/%s, want %s/%s", i, row.BaseAsset, row.QuoteAsset, aqua, usdc)
		}
		if row.BaseAmount != wantAmounts[i][0] || row.QuoteAmount != wantAmounts[i][1] {
			t.Errorf("row %d amounts = %s/%s, want %s/%s",
				i, row.BaseAmount, row.QuoteAmount, wantAmounts[i][0], wantAmounts[i][1])
		}
	}
	for i := 1; i < len(page.Data); i++ {
		if !page.Data[i-1].Timestamp.Before(page.Data[i].Timestamp) {
			t.Errorf("row %d ts %s is not after row %d ts %s — the merge must hold the keyset order",
				i, page.Data[i].Timestamp, i-1, page.Data[i-1].Timestamp)
		}
	}
	if page.Pagination != nil {
		t.Errorf("pagination = %+v, want absent — the window is drained", page.Pagination)
	}
}

// drainHistory paginates a request to exhaustion and returns every row
// in the order it was served, plus the page sizes. It fails the test if
// the drain does not terminate promptly, which is the shape a merge
// that mints a cursor it cannot advance past would take.
func drainHistory(t *testing.T, ts *testServer, query string) (rows []v1.TradeRow, pageSizes []int) {
	t.Helper()
	cursor := ""
	for page := 0; ; page++ {
		if page > 32 {
			t.Fatalf("drain did not terminate after 32 pages (%d rows)", len(rows))
		}
		q := query
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		got := getHistoryPage(t, ts, q)
		rows = append(rows, got.Data...)
		pageSizes = append(pageSizes, len(got.Data))
		if got.Pagination == nil {
			return rows, pageSizes
		}
		if got.Pagination.Next == cursor {
			t.Fatalf("cursor did not advance past %q", cursor)
		}
		cursor = got.Pagination.Next
	}
}

// TestHistory_PageBoundaryAcrossDirections is the boundary case: both
// directions hold rows immediately after every cursor, so each page cut
// falls between the two streams. The drain must serve every row exactly
// once, in order, and must not report the window drained while rows
// remain in the other direction.
func TestHistory_PageBoundaryAcrossDirections(t *testing.T) {
	t.Parallel()
	usdc := mustParseAsset(t, usdcClassicID)
	aqua := mustParseAsset(t, aquaClassicID)

	// Strictly interleaved: odd seconds requested-side, even flipped.
	rows := []canonical.Trade{
		storedTrade(t, "sdex", 10, "d1", aqua, usdc, 1, 11),
		storedTrade(t, "sdex", 11, "d2", usdc, aqua, 12, 1),
		storedTrade(t, "sdex", 12, "d3", aqua, usdc, 1, 13),
		storedTrade(t, "sdex", 13, "d4", usdc, aqua, 14, 1),
		storedTrade(t, "sdex", 14, "d5", aqua, usdc, 1, 15),
		storedTrade(t, "sdex", 15, "d6", usdc, aqua, 16, 1),
		storedTrade(t, "sdex", 16, "d7", aqua, usdc, 1, 17),
	}
	store := &orientedTradeStore{rows: rows}
	ts := httpTestServer(t, v1.New(v1.Options{History: store}))

	served, sizes := drainHistory(t, ts, orientationQuery(aqua, usdc, 2))
	if len(served) != len(rows) {
		t.Fatalf("drained %d rows in pages %v, want %d — a page boundary between the two directions must not drop a row",
			len(served), sizes, len(rows))
	}
	seen := map[string]int{}
	for i, row := range served {
		seen[row.TxHash]++
		if row.BaseAsset != aqua.String() || row.QuoteAsset != usdc.String() {
			t.Errorf("row %d pair = %s/%s, want %s/%s", i, row.BaseAsset, row.QuoteAsset, aqua, usdc)
		}
		if i > 0 && !served[i-1].Timestamp.Before(row.Timestamp) {
			t.Errorf("row %d ts %s is not after row %d ts %s — the drain must hold one order across pages",
				i, row.Timestamp, i-1, served[i-1].Timestamp)
		}
	}
	for _, r := range rows {
		if seen[r.TxHash] != 1 {
			t.Errorf("tx %s served %d times, want exactly 1", r.TxHash, seen[r.TxHash])
		}
	}
}

// TestHistory_PageBoundaryWithinOneLedger tightens the boundary onto a
// single (ts, ledger): both directions hold rows there, so the cut is
// decided by tx_hash — the component the merge and the database order
// identically — rather than by time.
func TestHistory_PageBoundaryWithinOneLedger(t *testing.T) {
	t.Parallel()
	usdc := mustParseAsset(t, usdcClassicID)
	aqua := mustParseAsset(t, aquaClassicID)

	rows := []canonical.Trade{
		storedTrade(t, "sdex", 10, "e1", aqua, usdc, 1, 21),
		storedTrade(t, "sdex", 10, "e2", usdc, aqua, 22, 1),
		storedTrade(t, "sdex", 10, "e3", aqua, usdc, 1, 23),
		storedTrade(t, "sdex", 10, "e4", usdc, aqua, 24, 1),
	}
	store := &orientedTradeStore{rows: rows}
	ts := httpTestServer(t, v1.New(v1.Options{History: store}))

	served, sizes := drainHistory(t, ts, orientationQuery(aqua, usdc, 1))
	if len(served) != len(rows) {
		t.Fatalf("drained %d rows in pages %v, want %d", len(served), sizes, len(rows))
	}
	for i, row := range served {
		wantSuffix := fmt.Sprintf("e%d", i+1)
		if !strings.HasSuffix(row.TxHash, wantSuffix) {
			t.Errorf("row %d tx = %s, want the one ending %s — one ledger's rows order by tx_hash across both directions",
				i, row.TxHash, wantSuffix)
		}
	}
}

// TestHistory_PageIsNotCutThroughATieGroup pins the one comparison the
// merge deliberately refuses to make.
//
// Two rows can share (ts, ledger, tx_hash, op_index) and differ only in
// `source` — the trades primary key allows it — and `source` is the one
// keyset component whose Go byte order can disagree with the database's
// collation. Cutting a page between two such rows would mint a cursor
// the database may order AFTER the row that was dropped, and that row
// would never be served. So the cut moves off the group, and the page
// comes back SHORT with a cursor rather than full and lossy.
func TestHistory_PageIsNotCutThroughATieGroup(t *testing.T) {
	t.Parallel()
	usdc := mustParseAsset(t, usdcClassicID)
	aqua := mustParseAsset(t, aquaClassicID)

	tied := storedTrade(t, "blend_emitter", 20, "f2", aqua, usdc, 1, 31)
	tiedFlip := storedTrade(t, "blenda", 20, "f2", usdc, aqua, 32, 1)
	rows := []canonical.Trade{
		storedTrade(t, "sdex", 10, "f1", aqua, usdc, 1, 30),
		tied,
		tiedFlip,
	}
	store := &orientedTradeStore{rows: rows}
	ts := httpTestServer(t, v1.New(v1.Options{History: store}))

	// limit 2 would cut between the two tied rows.
	page := getHistoryPage(t, ts, orientationQuery(aqua, usdc, 2))
	if len(page.Data) != 1 {
		t.Fatalf("first page = %d rows, want 1 — the page must stop before the tie group, not inside it", len(page.Data))
	}
	if page.Pagination == nil {
		t.Fatalf("pagination absent on a short page that has rows behind it — the cursor must not be inferred from the page length")
	}

	served, sizes := drainHistory(t, ts, orientationQuery(aqua, usdc, 2))
	if len(served) != len(rows) {
		t.Fatalf("drained %d rows in pages %v, want %d", len(served), sizes, len(rows))
	}
	seen := map[string]int{}
	for _, row := range served {
		seen[row.TxHash+"|"+row.Source]++
	}
	for _, r := range rows {
		if seen[r.TxHash+"|"+r.Source] != 1 {
			t.Errorf("row %s/%s served %d times, want exactly 1", r.Source, r.TxHash, seen[r.TxHash+"|"+r.Source])
		}
	}
}

// TestHistory_FlippedRowWithZeroAmountRendersZeroPrice pins what a
// flipped row does when the amount that becomes its denominator is
// zero. The column carries a positive CHECK, so this is a row the
// database should not hold — the point is that the fold performs no
// division of its own, so a degenerate row renders the endpoint's
// existing zero-denominator answer instead of panicking or poisoning
// the page.
func TestHistory_FlippedRowWithZeroAmountRendersZeroPrice(t *testing.T) {
	t.Parallel()
	usdc := mustParseAsset(t, usdcClassicID)
	aqua := mustParseAsset(t, aquaClassicID)

	store := &orientedTradeStore{rows: []canonical.Trade{
		// Stored USDC/AQUA with a zero AQUA leg: flipping makes it the
		// base, i.e. the price denominator.
		storedTrade(t, "sdex", 10, "5a", usdc, aqua, 5, 0),
		storedTrade(t, "sdex", 20, "5b", usdc, aqua, 5, 1),
	}}
	ts := httpTestServer(t, v1.New(v1.Options{History: store}))

	page := getHistoryPage(t, ts, orientationQuery(aqua, usdc, 50))
	if len(page.Data) != 2 {
		t.Fatalf("rows = %d, want 2 — a degenerate row must not drop the page", len(page.Data))
	}
	if page.Data[0].BaseAmount != "0" || page.Data[0].QuoteAmount != "5" || page.Data[0].Price != "0" {
		t.Errorf("degenerate row = %s/%s @ %s, want 0/5 @ 0",
			page.Data[0].BaseAmount, page.Data[0].QuoteAmount, page.Data[0].Price)
	}
	if page.Data[1].Price != "5.0000000000" {
		t.Errorf("neighbour price = %s, want 5.0000000000 — one degenerate row must not poison the rest",
			page.Data[1].Price)
	}
}

// ─── Tie groups that outrun the page ──────────────────────────────────
//
// Rows sharing an exact (ts, ledger, tx_hash, op_index) differ only in
// `source` — one operation attributed to several connectors — and that
// is the one keyset component the merge refuses to compare, because Go's
// byte order and the database's collation can disagree on it.
//
// A page that both OPENS and would END inside such a group cannot cut at
// the group's lower edge (that edge is index 0, and an empty page
// carrying a cursor stalls the client), so it runs to the group's upper
// edge instead. That is only sound when the group is COMPLETE in both
// directions. A direction whose read stopped ON the group holds further
// rows of it the merge never saw; the cursor, minted from the other
// direction's row, then excludes them under the database's own
// `(ts, ledger, tx_hash, op_index, source) >` predicate and they are
// never served on any page. The read completes the group first, by
// re-reading the truncated direction with a raised limit.

// TestHistory_OverLimitTieGroupIsCompletedBeforeItIsServed is that case
// at its smallest client-reachable size: `limit` validates to [1, 10000],
// so limit=1 with a three-source group is something a caller can ask for.
func TestHistory_OverLimitTieGroupIsCompletedBeforeItIsServed(t *testing.T) {
	t.Parallel()
	usdc := mustParseAsset(t, usdcClassicID)
	aqua := mustParseAsset(t, aquaClassicID)

	// One group. Two rows stored one way, one the other; the flipped
	// row's source sorts ABOVE the stored row that a limit=1 read leaves
	// unfetched, so a page that serves the group without completing it
	// mints a cursor past that row.
	rows := []canonical.Trade{
		storedTrade(t, "aaa_src", 10, "1e", aqua, usdc, 1, 41),
		storedTrade(t, "bbb_src", 10, "1e", aqua, usdc, 1, 42),
		storedTrade(t, "ccc_src", 10, "1e", usdc, aqua, 43, 1),
	}
	store := &orientedTradeStore{rows: rows}
	ts := httpTestServer(t, v1.New(v1.Options{History: store}))

	served, sizes := drainHistory(t, ts, orientationQuery(aqua, usdc, 1))
	assertServedExactlyOnce(t, served, rows, sizes)
	if len(sizes) == 0 || sizes[0] != len(rows) {
		t.Errorf("first page = %v, want the whole group (%d rows) on one page — the group is completed, then served whole",
			sizes, len(rows))
	}
}

// TestHistory_OverLimitTieGroupAtLimitTwo repeats it one size up, so the
// limit=1 case cannot be read as a boundary artefact.
func TestHistory_OverLimitTieGroupAtLimitTwo(t *testing.T) {
	t.Parallel()
	usdc := mustParseAsset(t, usdcClassicID)
	aqua := mustParseAsset(t, aquaClassicID)

	rows := []canonical.Trade{
		storedTrade(t, "aaa_src", 10, "2f", aqua, usdc, 1, 51),
		storedTrade(t, "bbb_src", 10, "2f", aqua, usdc, 1, 52),
		storedTrade(t, "ccc_src", 10, "2f", aqua, usdc, 1, 53), // unfetched at limit=2
		storedTrade(t, "ddd_src", 10, "2f", usdc, aqua, 54, 1), // flipped, highest source
	}
	store := &orientedTradeStore{rows: rows}
	ts := httpTestServer(t, v1.New(v1.Options{History: store}))

	served, sizes := drainHistory(t, ts, orientationQuery(aqua, usdc, 2))
	assertServedExactlyOnce(t, served, rows, sizes)
}

// TestHistory_TieGroupSweepLosesNothing walks every arrangement of four
// rows over two tie groups, both orientations, at limits 1..3 — the
// shapes a hand-built fixture keeps missing. Every row must be served
// EXACTLY once: losing one is the failure the merge exists to prevent,
// and repeating one silently inflates any total a client accumulates
// across pages.
func TestHistory_TieGroupSweepServesEachRowOnce(t *testing.T) {
	t.Parallel()
	usdc := mustParseAsset(t, usdcClassicID)
	aqua := mustParseAsset(t, aquaClassicID)
	sources := []string{"s1", "s2", "s3", "s4"}

	duplicated := 0
	store := &orientedTradeStore{}
	ts := httpTestServer(t, v1.New(v1.Options{History: store}))
	for mask := 0; mask < 256; mask++ {
		for limit := 1; limit <= 3; limit++ {
			rows := make([]canonical.Trade, 0, len(sources))
			for k, src := range sources {
				sec, tx := int64(10), "1c"
				if mask&(1<<(k+4)) != 0 {
					sec, tx = 20, "2d"
				}
				if mask&(1<<k) != 0 {
					rows = append(rows, storedTrade(t, src, sec, tx, usdc, aqua, 100, 1))
					continue
				}
				rows = append(rows, storedTrade(t, src, sec, tx, aqua, usdc, 1, 100))
			}
			store.reset(rows, nil)
			served, sizes := drainHistory(t, ts, orientationQuery(aqua, usdc, limit))

			seen := map[string]int{}
			for _, r := range served {
				seen[servedIdentity(r)]++
			}
			for _, r := range rows {
				switch n := seen[fixtureIdentity(r)]; {
				case n == 0:
					t.Errorf("mask=%d limit=%d: %s tx…%s never served (pages %v)",
						mask, limit, r.Source, r.TxHash[60:], sizes)
				case n > 1:
					duplicated++
				}
			}
		}
	}
	// ZERO, not "few". Pagination here is exactly-once: a page that ends
	// on a complete tie group resumes PAST the group by key rather than
	// at one row of it, so no row of that group can come back. A client
	// appending pages and summing volume is entitled to that, and a
	// change that re-introduces a single repeat is a change that makes
	// every integrator's total quietly wrong.
	if duplicated != 0 {
		t.Errorf("rows served more than once = %d, want 0 — pagination is exactly-once", duplicated)
	}
}

// servedIdentity is the trades primary key as it appears on the wire —
// the tuple that names ONE stored row. Source alone is not it: the same
// connector records many trades, and two of them can share a tx_hash
// suffix in a fixture while being different rows.
func servedIdentity(r v1.TradeRow) string {
	return fmt.Sprintf("%s|%d|%s|%d", r.Source, r.Ledger, r.TxHash, r.OpIndex)
}

// fixtureIdentity is [servedIdentity] for a row as the fixture holds it.
func fixtureIdentity(t canonical.Trade) string {
	return fmt.Sprintf("%s|%d|%s|%d", t.Source, t.Ledger, t.TxHash, t.OpIndex)
}

// assertServedExactlyOnce fails when the drain did not return each
// fixture row exactly one time, and when it returned anything the
// fixture does not hold.
func assertServedExactlyOnce(t *testing.T, served []v1.TradeRow, rows []canonical.Trade, sizes []int) {
	t.Helper()
	seen := map[string]int{}
	for _, r := range served {
		seen[servedIdentity(r)]++
	}
	for _, r := range rows {
		if n := seen[fixtureIdentity(r)]; n != 1 {
			t.Errorf("row %s served %d times, want exactly 1 (pages %v, %d rows drained)",
				fixtureIdentity(r), n, sizes, len(served))
		}
	}
	if len(seen) != len(rows) {
		t.Errorf("served %d distinct rows, fixture holds %d (pages %v)", len(seen), len(rows), sizes)
	}
}

// ─── The flip against the two other things that read these rows ───────

// TestHistory_FlippedRowNonstandardDecimals pins the pairing between the
// flipped trade slice and the request's legs.
//
// normalizeTradeRowPrices is handed the trades the page returned — now
// re-expressed — while `base` and `quote` are the REQUEST's legs. If
// that pairing were inverted the correction would run 10^-2 instead of
// 10^+2 and the price would read 0.00025 rather than 2.5, on the one
// surface whose whole point is the per-trade number.
func TestHistory_FlippedRowNonstandardDecimals(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	flagged := mustParseAsset(t, flaggedAsset)
	usd := mustParseAsset(t, "fiat:USD")
	// Stored the other way round: 250 USD (7dp) bought 100 tokens (9dp).
	stored := storedTradeAmounts(t, "aquarius", 10, "3a", usd, flagged, "2500000000", "100000000000")
	ts := httpTestServer(t, v1.New(v1.Options{
		History:             &orientedTradeStore{rows: []canonical.Trade{stored}},
		NonstandardDecimals: cache,
	}))

	page := getHistoryPage(t, ts, orientationQuery(flagged, usd, 50))
	if len(page.Data) != 1 {
		t.Fatalf("rows = %d, want 1", len(page.Data))
	}
	got := page.Data[0]
	if got.BaseAmount != "100000000000" || got.QuoteAmount != "2500000000" {
		t.Errorf("amounts = %s/%s, want 100000000000/2500000000", got.BaseAmount, got.QuoteAmount)
	}
	if got.Price != "2.5000000000" {
		t.Errorf("price = %s, want 2.5000000000 — 250 USD over 100 tokens, corrected for a 9dp base against a 7dp quote", got.Price)
	}
}

// TestHistory_FlippedInversionIsExactAtScale puts the swap at magnitudes
// where a float — or a reciprocal taken as a division rather than as a
// swap — drifts. Nothing here is rounded except the final render at ten
// fractional digits.
func TestHistory_FlippedInversionIsExactAtScale(t *testing.T) {
	t.Parallel()
	usdc := mustParseAsset(t, usdcClassicID)
	aqua := mustParseAsset(t, aquaClassicID)

	store := &orientedTradeStore{rows: []canonical.Trade{
		storedTradeAmounts(t, "sdex", 10, "4a", usdc, aqua, "1000000000000000001", "3"),
		storedTradeAmounts(t, "sdex", 20, "4b", usdc, aqua, "3", "1000000000000000001"),
		storedTradeAmounts(t, "sdex", 30, "4c", usdc, aqua, "7", "1"),
		storedTradeAmounts(t, "sdex", 40, "4d", usdc, aqua, "1", "3"),
	}}
	ts := httpTestServer(t, v1.New(v1.Options{History: store}))

	page := getHistoryPage(t, ts, orientationQuery(aqua, usdc, 50))
	if len(page.Data) != 4 {
		t.Fatalf("rows = %d, want 4", len(page.Data))
	}
	for i, want := range []struct{ base, quote, price string }{
		{"3", "1000000000000000001", "333333333333333333.6666666666"},
		{"1000000000000000001", "3", "0.0000000000"},
		{"1", "7", "7.0000000000"},
		{"3", "1", "0.3333333333"},
	} {
		got := page.Data[i]
		if got.BaseAmount != want.base || got.QuoteAmount != want.quote || got.Price != want.price {
			t.Errorf("row %d = %s/%s @ %s, want %s/%s @ %s",
				i, got.BaseAmount, got.QuoteAmount, got.Price, want.base, want.quote, want.price)
		}
	}
}

// storedTradeAmounts is [storedTrade] with decimal-string amounts, for
// the magnitudes an int64 cannot carry.
func storedTradeAmounts(t *testing.T, source string, sec int64, txSuffix string, base, quote canonical.Asset, baseAmt, quoteAmt string) canonical.Trade {
	t.Helper()
	tr := storedTrade(t, source, sec, txSuffix, base, quote, 1, 1)
	b, ok := new(big.Int).SetString(baseAmt, 10)
	if !ok {
		t.Fatalf("bad base amount %q", baseAmt)
	}
	q, ok := new(big.Int).SetString(quoteAmt, 10)
	if !ok {
		t.Fatalf("bad quote amount %q", quoteAmt)
	}
	tr.BaseAmount, tr.QuoteAmount = canonical.NewAmount(b), canonical.NewAmount(q)
	return tr
}

// ─── The cursor that steps past a complete tie group ──────────────────
//
// A cursor naming the last served ROW re-serves the rows of its group
// that the database orders above it, because the merge puts the
// requested orientation first on a tie while the database orders the
// group by `source`. A page that ends on a group proved COMPLETE in both
// directions therefore resumes past the whole group by key — same
// (ts, ledger, tx_hash), op_index stepped once, no source at all — which
// no row of the group can satisfy and every later row can.
//
// Cursor arithmetic has one failure mode a row-shaped cursor does not:
// a resume point that matches nothing, or that wraps. Both are below.

// TestHistory_PastGroupCursorPointingBeyondTheWindowTerminates drives
// the case the arithmetic makes possible: a cursor stepped past a
// complete group that is then asked about a window holding nothing after
// it. The page must come back empty, carry NO cursor, and stop —
// re-serving the group, or handing back a cursor to be asked again,
// would be an endless drain.
//
// A client narrowing `to` between pages is what produces it: the first
// request spans both groups, so the cursor is minted past the first; the
// second replays that cursor over a window that ends before the second
// group exists.
func TestHistory_PastGroupCursorPointingBeyondTheWindowTerminates(t *testing.T) {
	t.Parallel()
	usdc := mustParseAsset(t, usdcClassicID)
	aqua := mustParseAsset(t, aquaClassicID)

	// A complete two-source group at +10s, then one later row at +30s so
	// the first page ends on the group with rows still behind it.
	//
	// The FLIPPED row's source sorts BELOW the stored row's, which is the
	// arrangement that makes the two orderings disagree: the merge puts
	// the requested orientation first on a tie, so a last-row cursor
	// would name `aaa_src` and the database would hand `zzz_src` back on
	// the next page. Reverse these two and the case tests nothing.
	store := &orientedTradeStore{rows: []canonical.Trade{
		storedTrade(t, "zzz_src", 10, "c1", aqua, usdc, 1, 61),
		storedTrade(t, "aaa_src", 10, "c1", usdc, aqua, 62, 1),
		storedTrade(t, "ccc_src", 30, "c2", aqua, usdc, 1, 63),
	}}
	ts := httpTestServer(t, v1.New(v1.Options{History: store}))

	wide := getHistoryPage(t, ts, orientationQuery(aqua, usdc, 2))
	if len(wide.Data) != 2 {
		t.Fatalf("first page = %d rows, want the 2-row group", len(wide.Data))
	}
	if wide.Pagination == nil {
		t.Fatalf("first page carried no cursor, but a row remains behind it")
	}

	// Same cursor, window clipped to end before the +30s row.
	narrow := url.Values{
		"base":   {aqua.String()},
		"quote":  {usdc.String()},
		"from":   {orientationWindowStart.Format(time.RFC3339)},
		"to":     {orientationWindowStart.Add(20 * time.Second).Format(time.RFC3339)},
		"limit":  {"2"},
		"cursor": {wide.Pagination.Next},
	}.Encode()
	page := getHistoryPage(t, ts, narrow)
	if len(page.Data) != 0 {
		t.Errorf("page past the end = %d rows, want 0 — the cursor steps past the group, and nothing follows it in this window", len(page.Data))
	}
	if page.Pagination != nil {
		t.Errorf("page past the end carried a cursor (%q), want none — the drain must stop", page.Pagination.Next)
	}
	// And the same cursor over the ORIGINAL window still reaches the row
	// it was minted to reach: stepping past a group must not step past
	// anything else.
	rest, sizes := drainHistory(t, ts, orientationQuery(aqua, usdc, 2))
	assertServedExactlyOnce(t, rest, store.rows, sizes)
}

// TestHistory_PastGroupCursorDeclinesToWrapOpIndex pins the guard. At
// the maximum op_index the step would wrap to zero — a cursor pointing
// at the START of the transaction, which re-serves it forever — so the
// read keeps the last-row cursor instead. That cursor can repeat a row;
// it cannot loop, and it cannot lose one.
func TestHistory_PastGroupCursorDeclinesToWrapOpIndex(t *testing.T) {
	t.Parallel()
	usdc := mustParseAsset(t, usdcClassicID)
	aqua := mustParseAsset(t, aquaClassicID)

	rows := []canonical.Trade{
		storedTrade(t, "zzz_src", 10, "d1", aqua, usdc, 1, 71),
		storedTrade(t, "aaa_src", 10, "d1", usdc, aqua, 72, 1),
		storedTrade(t, "ccc_src", 30, "d2", aqua, usdc, 1, 73),
	}
	for i := range rows {
		rows[i].OpIndex = math.MaxUint32
	}
	store := &orientedTradeStore{rows: rows}
	ts := httpTestServer(t, v1.New(v1.Options{History: store}))

	// drainHistory fails the test if the drain does not terminate, which
	// is the shape a wrapped cursor takes.
	served, sizes := drainHistory(t, ts, orientationQuery(aqua, usdc, 2))
	seen := map[string]int{}
	for _, r := range served {
		seen[r.Source]++
	}
	for _, r := range rows {
		if seen[r.Source] == 0 {
			t.Errorf("source %s never served (pages %v) — the fallback cursor must not skip", r.Source, sizes)
		}
	}
}

// TestHistoryCursor_PastGroupMarkerIsNotASourceName pins the assumption
// the past-group cursor's wire form rests on: its marker cannot be
// mistaken for a source, so a cursor cannot be read as the wrong form.
// Source names are documented as [a-z0-9_-]; this checks the live
// registry rather than the documentation.
func TestHistoryCursor_PastGroupMarkerIsNotASourceName(t *testing.T) {
	t.Parallel()
	for name := range external.Registry {
		if name == "*" {
			t.Errorf("source %q collides with the past-group cursor marker", name)
		}
		if strings.ContainsAny(name, "*:") {
			t.Errorf("source %q carries a character the cursor grammar reserves", name)
		}
	}
}

// ─── The two permanent guards over the whole design ───────────────────
//
// Everything above tests a shape someone thought of. These two test the
// property instead, against a model of the store, and they are the ones
// to keep pointing at a change to the merge or the cursor.

// TestHistory_TieGroupLargerThanAnyPageIsServedWhole is the case a
// bounded completion budget got wrong.
//
// An over-limit tie group is completed by re-reading the truncated
// direction, and that re-read goes straight to the store's maximum
// rather than climbing a ladder. A ladder has a top, and a group past
// the top fell to a branch that cut THROUGH the group and minted a
// last-row cursor — so the flipped row whose source sorts below every
// stored source in the group was excluded by the database's own
// predicate and never served on any page. The branch is gone; this is
// the fixture that would bring it back.
func TestHistory_TieGroupLargerThanAnyPageIsServedWhole(t *testing.T) {
	t.Parallel()
	usdc := mustParseAsset(t, usdcClassicID)
	aqua := mustParseAsset(t, aquaClassicID)

	// One group: 300 rows in the requested orientation plus a single
	// flipped row whose source sorts BELOW all of them. Far past any
	// plausible page size, and past the ladder that used to bound the
	// completion.
	rows := make([]canonical.Trade, 0, 301)
	rows = append(rows, storedTrade(t, "aaa_low", 10, "0c", usdc, aqua, 100, 1))
	for i := 0; i < 300; i++ {
		rows = append(rows, storedTrade(t, fmt.Sprintf("s%03d", i), 10, "0c", aqua, usdc, 1, 100))
	}
	store := &orientedTradeStore{rows: rows}
	ts := httpTestServer(t, v1.New(v1.Options{History: store}))

	// limit=1 is client-reachable, and it is the smallest page that can
	// hold this group — which is to say it cannot, so the page runs to
	// the group's upper edge.
	served, sizes := drainHistory(t, ts, orientationQuery(aqua, usdc, 1))
	assertServedExactlyOnce(t, served, rows, sizes)
	if len(sizes) == 0 || sizes[0] != len(rows) {
		t.Errorf("first page = %v, want the whole %d-row group — a group is never split across a page edge",
			sizes, len(rows))
	}
	if store.maxRead <= len(rows) {
		t.Errorf("largest read asked of the store = %d, want more than the %d-row group — the completion re-read must outrun the group in one go",
			store.maxRead, len(rows))
	}
}

// TestHistory_ExactlyOnceUnderEitherSourceCollation is the property the
// spec now promises, checked against a model of the store rather than
// against a shape.
//
// The merge never compares `source`, because Go's byte order and the
// database's collation can disagree on it. The way to test that claim is
// to make the model disagree: run every drain twice, once with the
// source column ordered ascending and once DESCENDING, and require the
// same answer both times.
//
// What the reversed half catches, stated as the mutation that proves
// it rather than as a claim: order the merge's ties by Go `source` and
// resume at the last served row. That is exactly-once under the
// ascending model — Go's order and the database's agree there, so the
// last row of a group is also the database's last — and it serves rows
// twice under the reversed one. The ascending half alone passes it. Any
// design that reaches its resume point through a source comparison has
// that shape, including the "resume at the group's lowest source"
// alternative this endpoint deliberately does not use.
//
// It does NOT catch every source comparison. Ordering ties by source
// while still resuming PAST the group is harmless, because the cursor
// no longer depends on where inside a group the page ended — which is
// the point of stepping past it.
func TestHistory_ExactlyOnceUnderEitherSourceCollation(t *testing.T) {
	t.Parallel()
	usdc := mustParseAsset(t, usdcClassicID)
	aqua := mustParseAsset(t, aquaClassicID)
	// Names whose byte order and reversed order disagree, and which
	// carry the `-` and `_` a non-C collation weighs oddly.
	sources := []string{"a-x", "a_x", "blend", "blend_emitter", "sdex", "soroswap-router", "zz", "m1"}
	txs := []string{"0a", "0b", "0c"}
	secs := []int64{10, 20}

	collations := []struct {
		name string
		less func(a, b string) bool
	}{
		{"ascending", func(a, b string) bool { return a < b }},
		{"descending", func(a, b string) bool { return a > b }},
	}
	drains := 0
	for _, coll := range collations {
		// ONE server, one store, re-pointed at each fixture.
		store := &orientedTradeStore{}
		ts := httpTestServer(t, v1.New(v1.Options{History: store}))
		// Same seed per collation, so the two runs see identical
		// fixtures and any difference is the collation alone.
		rng := rand.New(rand.NewSource(20260905))
		for iter := 0; iter < 250; iter++ {
			held := map[string]bool{}
			rows := make([]canonical.Trade, 0, 8)
			for k := 0; k < 1+rng.Intn(8); k++ {
				src := sources[rng.Intn(len(sources))]
				sec := secs[rng.Intn(len(secs))]
				tx := txs[rng.Intn(len(txs))]
				// The primary key is (source, ledger, tx_hash, op_index,
				// ts) and holds no asset column, so one identity exists
				// in ONE orientation only. A fixture that broke that
				// would be testing something the store cannot produce.
				id := fmt.Sprintf("%s|%d|%s", src, sec, tx)
				if held[id] {
					continue
				}
				held[id] = true
				if rng.Intn(2) == 0 {
					rows = append(rows, storedTrade(t, src, sec, tx, aqua, usdc, 1, 100))
					continue
				}
				rows = append(rows, storedTrade(t, src, sec, tx, usdc, aqua, 100, 1))
			}
			if len(rows) == 0 {
				continue
			}
			for limit := 1; limit <= 5; limit++ {
				store.reset(rows, coll.less)
				served, sizes := drainHistory(t, ts, orientationQuery(aqua, usdc, limit))
				drains++

				count := map[string]int{}
				for _, r := range served {
					count[servedIdentity(r)]++
				}
				for _, r := range rows {
					if n := count[fixtureIdentity(r)]; n != 1 {
						t.Fatalf("collation=%s iter=%d limit=%d rows=%d: %s served %d times, want 1 (pages %v)",
							coll.name, iter, limit, len(rows), fixtureIdentity(r), n, sizes)
					}
				}
				if len(count) != len(rows) {
					t.Fatalf("collation=%s iter=%d limit=%d: served %d distinct rows, stored %d",
						coll.name, iter, limit, len(count), len(rows))
				}
			}
		}
	}
	t.Logf("%d drains across both source collations, every row served exactly once", drains)
}
