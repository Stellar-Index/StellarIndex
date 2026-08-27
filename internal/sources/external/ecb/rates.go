package ecb

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
)

// LatestEURRates returns ECB's daily reference rates as a raw EUR-base
// map — each value is "1 EUR = X units of that currency", exactly as
// published — together with the date ECB stamped on the file.
//
// This is the same upstream document [Poller.PollOnce] reads, but
// returned as rates rather than as canonical Trades. It exists so the
// forex worker can use ECB as a FALLBACK rate source when its paid
// primary (massive) is unavailable, without duplicating the fetch and
// XML shape in a second package.
//
// EUR itself is NOT in the returned map: ECB publishes rates AGAINST
// the euro, so the euro has no cube of its own. Callers converting to a
// different base must add it explicitly.
//
// `endpoint` may be empty, in which case [DefaultEndpoint] is used.
func LatestEURRates(ctx context.Context, endpoint string) (map[string]float64, time.Time, error) {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	status, body, err := external.GetBody(ctx, external.GetRequest{
		URL: endpoint,
		Headers: map[string]string{
			"Accept":     "application/xml, text/xml",
			"User-Agent": "stellarindex/1.0",
		},
		LimitBytes: 1 * 1024 * 1024,
	})
	if err != nil {
		return nil, time.Time{}, err
	}
	if status != http.StatusOK {
		return nil, time.Time{}, fmt.Errorf("http %d: %s", status, string(body))
	}

	var env gesmesEnvelope
	if err := xml.Unmarshal(body, &env); err != nil {
		return nil, time.Time{}, fmt.Errorf("%w: %w", ErrMalformedResponse, err)
	}
	if len(env.Cube.Inner) == 0 || len(env.Cube.Inner[0].Rates) == 0 {
		return nil, time.Time{}, ErrNoRates
	}

	// Newest date cube first — ECB lists newest first in the daily file.
	day := env.Cube.Inner[0]
	ts, err := time.Parse("2006-01-02", day.Time)
	if err != nil {
		// Same belt-and-braces as PollOnce: the daily file always
		// carries a valid ISO date.
		ts = time.Now().UTC()
	}

	out := make(map[string]float64, len(day.Rates))
	for _, r := range day.Rates {
		if r.Currency == "" || r.Rate == "" {
			continue
		}
		v, perr := strconv.ParseFloat(r.Rate, 64)
		// Skip rather than fail the whole document: one unparseable or
		// non-positive cube must not cost us the other ~30 currencies.
		if perr != nil || v <= 0 {
			continue
		}
		out[r.Currency] = v
	}
	if len(out) == 0 {
		return nil, time.Time{}, ErrNoRates
	}
	return out, ts.UTC(), nil
}
