// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
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

func aquaUSDC(t *testing.T) (aqua, usdc canonical.Asset) {
	t.Helper()
	return mustParseAsset(t, aquaClassicID), mustParseAsset(t, usdcClassicID)
}

// priceString renders a nullable wire price for comparison and messages.
func priceString(p *string) string {
	if p == nil {
		return "null"
	}
	return *p
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
// fixture does not hold. It reports whether the drain was clean.
func assertServedExactlyOnce(t *testing.T, served []v1.TradeRow, rows []canonical.Trade, sizes []int) bool {
	t.Helper()
	ok := true
	seen := map[string]int{}
	for _, r := range served {
		seen[servedIdentity(r)]++
	}
	for _, r := range rows {
		if n := seen[fixtureIdentity(r)]; n != 1 {
			t.Errorf("row %s served %d times, want exactly 1 (pages %v, %d rows drained)",
				fixtureIdentity(r), n, sizes, len(served))
			ok = false
		}
	}
	if len(seen) != len(rows) {
		t.Errorf("served %d distinct rows, fixture holds %d (pages %v)", len(seen), len(rows), sizes)
		ok = false
	}
	return ok
}

// ─── The flip against the two other things that read these rows ───────

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
