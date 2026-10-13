// Package vendorcsv replays a vendor's daily XLM/USD close series from an owner-exported CSV into
// the trades table for the span no exchange feed covers (2014-08 to 2017-01). It is history only:
// no streamer, no live path, no network access.
//
// A row dated D is the vendor's 00:00 UTC snapshot of the close of D-1, so it is stored one second
// before D with the close as the quote amount for exactly one XLM. The rows are registered with
// IncludeInVWAP=false and weight 0: they surface per point through `sources` and never enter a
// VWAP, a total or usd_volume.
package vendorcsv

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/scale"
)

// SourceName is stamped on every trade this package emits; a bar under it is never an exchange fill.
const SourceName = "coingecko_daily"

// Day is the only granularity the export carries.
const Day = 24 * time.Hour

const (
	dateColumn  = "event_date"
	closeColumn = "close_price_usd"
	dateLayout  = "2006-01-02 15:04:05 UTC"
	pairSymbol  = "crypto:XLM/fiat:USD"
	// candleSymbol seeds the synthetic tx_hash; with scale's legacy suffix it must fit 32 bytes.
	candleSymbol  = "XLMUSD"
	quoteDecimals = 8
)

// oneXLM is every bar's base amount at the registry's 8-decimal off-chain scale.
var oneXLM = big.NewInt(100_000_000)

// SpanEnd is the exclusive end of the vendor series: the first venue series owns the week and month
// rollup buckets from here on, and those rollups have no source filter.
var SpanEnd = time.Date(2017, 1, 1, 0, 0, 0, 0, time.UTC)

// Close is one dated vendor close: Day is the row's midnight UTC date, Close the price at 8 decimals.
type Close struct {
	Day   time.Time
	Close *big.Int
}

// Backfiller replays the export at Path; it implements [external.Backfiller].
type Backfiller struct {
	Path string
	// Now is the clock the closed-candle check uses; nil means time.Now.
	Now func() time.Time
}

// Name implements [external.Connector].
func (b *Backfiller) Name() string { return SourceName }

// Class implements [external.Connector].
func (b *Backfiller) Class() external.Class { return external.ClassAggregator }

// Backfill returns one trade per dated close whose stamp lies in [from, to) and whose day has closed.
// A missing day is omitted, never interpolated.
func (b *Backfiller) Backfill(ctx context.Context, pair canonical.Pair, from, to time.Time, granularity time.Duration) ([]canonical.Trade, error) {
	if granularity != Day {
		return nil, fmt.Errorf("vendorcsv.Backfill: only 1d is supported, got %v", granularity)
	}
	if !from.Before(to) {
		return nil, fmt.Errorf("vendorcsv.Backfill: from %s is not before to %s", from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	if to.After(SpanEnd) {
		return nil, fmt.Errorf("vendorcsv.Backfill: %s only backfills before %s: -to %s lies past it", SourceName, SpanEnd.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	if pair.String() != pairSymbol {
		return nil, fmt.Errorf("vendorcsv.Backfill: only %s is supported, got %s", pairSymbol, pair.String())
	}
	if b.Path == "" {
		return nil, errors.New("vendorcsv.Backfill: Path is empty")
	}
	f, err := os.Open(b.Path)
	if err != nil {
		return nil, fmt.Errorf("vendorcsv.Backfill: %w", err)
	}
	defer func() { _ = f.Close() }()
	closes, err := ParseCloses(f)
	if err != nil {
		return nil, fmt.Errorf("vendorcsv.Backfill: %s: %w", b.Path, err)
	}
	now := time.Now
	if b.Now != nil {
		now = b.Now
	}
	at := now().UTC()
	out := make([]canonical.Trade, 0, len(closes))
	for _, c := range closes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		closeSec := c.Day.Unix() - 1
		ts := time.Unix(closeSec, 0).UTC()
		if ts.Before(from) || !ts.Before(to) || !scale.CandleClosed(c.Day, to, at) {
			continue
		}
		hash, err := scale.CandleTxHash(candleSymbol, closeSec, Day)
		if err != nil {
			return nil, fmt.Errorf("vendorcsv.Backfill: %w", err)
		}
		t := canonical.Trade{
			Source:      SourceName,
			TxHash:      hash,
			Timestamp:   ts,
			Pair:        pair,
			BaseAmount:  canonical.NewAmount(new(big.Int).Set(oneXLM)),
			QuoteAmount: canonical.NewAmount(c.Close),
		}
		if err := t.Validate(); err != nil {
			return nil, fmt.Errorf("vendorcsv.Backfill: row %s: %w", c.Day.Format("2006-01-02"), err)
		}
		out = append(out, t)
	}
	return out, nil
}

// ParseCloses reads the export: a header naming event_date and close_price_usd, then rows strictly
// increasing by day. A blank, zero or negative close is dropped; an unparsable date, a time of day
// other than midnight, a non-decimal close or a day out of order is an error.
func ParseCloses(r io.Reader) ([]Close, error) {
	cr := csv.NewReader(r)
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("vendorcsv: header: %w", err)
	}
	dateIdx, closeIdx, err := columnIndexes(header)
	if err != nil {
		return nil, err
	}
	var out []Close
	var last time.Time
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("vendorcsv: line %d: %w", line, err)
		}
		day, err := parseDay(rec[dateIdx], line, last)
		if err != nil {
			return nil, err
		}
		last = day
		raw := strings.TrimSpace(rec[closeIdx])
		if raw == "" {
			continue
		}
		v, err := roundedScaledInt(raw, quoteDecimals)
		if err != nil {
			return nil, fmt.Errorf("vendorcsv: line %d: close: %w", line, err)
		}
		if v.Sign() <= 0 {
			continue
		}
		out = append(out, Close{Day: day, Close: v})
	}
	return out, nil
}

// roundedScaledInt scales a plain decimal string by 10^decimals, rounding half up: the export carries
// float artefacts such as 0.002204999999999999 that truncation would land one unit low.
func roundedScaledInt(s string, decimals int) (*big.Int, error) {
	dots, digits := 0, 0
	for i, c := range s {
		switch {
		case c == '-' && i == 0:
			// A negative close is skipped by the caller, not a parse error.
		case c == '.':
			dots++
		case c >= '0' && c <= '9':
			digits++
		default:
			return nil, fmt.Errorf("not a plain decimal: %q", s)
		}
	}
	r, ok := new(big.Rat).SetString(s)
	if dots > 1 || digits == 0 || !ok {
		return nil, fmt.Errorf("not a plain decimal: %q", s)
	}
	r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)))
	neg := r.Sign() < 0
	r.Abs(r)
	q, rem := new(big.Int).QuoRem(r.Num(), r.Denom(), new(big.Int))
	if rem.Lsh(rem, 1).Cmp(r.Denom()) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if neg {
		q.Neg(q)
	}
	return q, nil
}

// columnIndexes locates the two columns the export is read by; a BOM on the first header is ignored.
func columnIndexes(header []string) (dateIdx, closeIdx int, err error) {
	dateIdx, closeIdx = -1, -1
	for i, h := range header {
		switch strings.TrimSpace(strings.TrimPrefix(h, "\uFEFF")) {
		case dateColumn:
			dateIdx = i
		case closeColumn:
			closeIdx = i
		}
	}
	if dateIdx < 0 || closeIdx < 0 {
		return 0, 0, fmt.Errorf("vendorcsv: header lacks %s or %s: %q", dateColumn, closeColumn, header)
	}
	return dateIdx, closeIdx, nil
}

// parseDay accepts a midnight UTC snapshot strictly after last.
func parseDay(raw string, line int, last time.Time) (time.Time, error) {
	day, err := time.Parse(dateLayout, strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}, fmt.Errorf("vendorcsv: line %d: %w", line, err)
	}
	day = day.UTC()
	if h, m, s := day.Clock(); h != 0 || m != 0 || s != 0 {
		return time.Time{}, fmt.Errorf("vendorcsv: line %d: %q is not a midnight snapshot", line, raw)
	}
	if !last.IsZero() && !day.After(last) {
		return time.Time{}, fmt.Errorf("vendorcsv: line %d: %s does not follow %s", line, day.Format("2006-01-02"), last.Format("2006-01-02"))
	}
	return day, nil
}
