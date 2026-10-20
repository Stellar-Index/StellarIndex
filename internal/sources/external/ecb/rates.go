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

// LatestEURRates returns ECB's daily reference rates as a raw EUR-base map ("1 EUR = X units of
// that currency", as published) with the date ECB stamped on the file. It reads the same document
// as [Poller.PollOnce] so the forex worker can fall back to ECB without a second fetch/XML path.
//
// EUR itself is NOT in the map, so callers converting to another base must add it. An empty
// `endpoint` means [DefaultEndpoint].
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
