package v1

import (
	"context"
	"io"
	"log/slog"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// /v1/anomalies (frozen_value), /v1/divergence and /v1/divergence/series
// (our_price) serve the aggregator's stored price for a market. They must
// apply the decision /v1/price serves under, on both legs, at read time.

type withholdingAnomalyReader struct{ rows []timescale.FreezeEventRow }

func (f *withholdingAnomalyReader) ListFreezeEvents(context.Context, bool, int) ([]timescale.FreezeEventRow, error) {
	return f.rows, nil
}

func (f *withholdingAnomalyReader) FreezeReasonCounts(context.Context, int) ([]timescale.FreezeReasonCount, error) {
	return nil, nil
}

func (f *withholdingAnomalyReader) FreezeDailyReasonCounts(context.Context, int) ([]timescale.FreezeDailyReasonCount, error) {
	return nil, nil
}

func (f *withholdingAnomalyReader) CountFiringFreezes(context.Context) (int64, error) {
	return int64(len(f.rows)), nil
}

type withholdingDivergenceReader struct {
	latest     []timescale.DivergenceRow
	points     []timescale.DivergenceSeriesPoint
	seriesRead bool
}

func (f *withholdingDivergenceReader) ListDivergenceLatest(context.Context, int, bool, int) ([]timescale.DivergenceRow, error) {
	return f.latest, nil
}

func (f *withholdingDivergenceReader) ListDivergenceSeries(context.Context, string, string, int) ([]timescale.DivergenceSeriesPoint, error) {
	f.seriesRead = true
	return f.points, nil
}

// withholdingScamGate flags the listed asset ids, on either leg.
type withholdingScamGate map[string]bool

func (g withholdingScamGate) Withheld(_ context.Context, base canonical.Asset, _ string) bool {
	return g[base.String()]
}

func (g withholdingScamGate) WithheldPair(_ context.Context, base, quote canonical.Asset, _ string) bool {
	return g[base.String()] || g[quote.String()]
}

// withholdingFlaggedAsset is a directory-flagged classic asset in this
// file's gate only.
const withholdingFlaggedAsset = "RIO-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"

func withholdingServer() *Server {
	return &Server{
		Options: Options{Scam: withholdingScamGate{withholdingFlaggedAsset: true}},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}
