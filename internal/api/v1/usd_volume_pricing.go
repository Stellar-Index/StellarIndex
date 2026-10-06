package v1

import (
	"context"
	"math/big"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// UsdVolumePricingRefreshInterval is how often the background loop recomputes
// the axis; the window is one fixed hour-aligned 24h, so a request never scans trades.
const UsdVolumePricingRefreshInterval = 15 * time.Minute

const usdVolumePricingWindow = 24 * time.Hour

// Sources and bars match the usd-volume-coverage alert rules
// (configs/prometheus/rules.r1/usd-volume-coverage.yml).
var (
	usdVolumeExternalSources = []string{"binance", "kraken", "bitstamp", "coinbase", "massive", "exchangeratesapi", "ecb"}
	usdVolumeOnchainSources  = []string{"sdex", "soroswap", "aquarius", "phoenix", "comet"}
)

const (
	usdVolumeExternalBar = "0.999"
	usdVolumeOnchainBar  = "0.995"
)

// UsdVolumePricingReader is the storage seam; *timescale.Store satisfies it.
type UsdVolumePricingReader interface {
	UsdVolumePricingStats(ctx context.Context, from, to time.Time, sources []string) ([]timescale.UsdVolumePricingRow, error)
}

type usdVolumePricingSnapshot struct {
	rows       []timescale.UsdVolumePricingRow
	start, end time.Time
	asOf       time.Time
}

// UsdVolumePricingCache holds the last successful refresh; a failed refresh
// keeps the previous snapshot.
type UsdVolumePricingCache struct {
	mu     sync.RWMutex
	snap   *usdVolumePricingSnapshot
	reader UsdVolumePricingReader
	logger Logger
	now    func() time.Time
}

// NewUsdVolumePricingCache constructs an empty cache.
func NewUsdVolumePricingCache(reader UsdVolumePricingReader, logger Logger) *UsdVolumePricingCache {
	return &UsdVolumePricingCache{reader: reader, logger: logger, now: time.Now}
}

// Refresh recomputes the window [end-24h, end), end being the current UTC hour.
func (c *UsdVolumePricingCache) Refresh(ctx context.Context) error {
	now := c.now().UTC()
	end := now.Truncate(time.Hour)
	start := end.Add(-usdVolumePricingWindow)
	all := append(append([]string{}, usdVolumeExternalSources...), usdVolumeOnchainSources...)
	rows, err := c.reader.UsdVolumePricingStats(ctx, start, end, all)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("usd volume pricing refresh failed — serving last-good snapshot", "err", err)
		}
		return err
	}
	c.mu.Lock()
	c.snap = &usdVolumePricingSnapshot{rows: rows, start: start, end: end, asOf: now}
	c.mu.Unlock()
	return nil
}

func (c *UsdVolumePricingCache) view() *UsdVolumePricingAxisView {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	snap := c.snap
	c.mu.RUnlock()
	if snap == nil {
		return nil
	}
	return buildUsdVolumePricingView(snap)
}

// UsdVolumePricingAxisView is the USD volume pricing axis on GET /v1/coverage.
// It is not a source verdict and is outside complete_sources / total_sources:
// it measures valuation of captured trades, not capture.
type UsdVolumePricingAxisView struct {
	WindowStart WireTime `json:"window_start"`
	WindowEnd   WireTime `json:"window_end"`
	AsOf        WireTime `json:"as_of"`
	// LowerBound is true when any trade in the window has no usd_volume, so
	// every USD volume summed from the window is a lower bound.
	LowerBound bool `json:"lower_bound"`
	// Excluded names what the lower bound leaves out; empty when LowerBound is false.
	Excluded string                   `json:"excluded,omitempty"`
	Meaning  string                   `json:"meaning"`
	Sources  []UsdVolumePricingSource `json:"sources"`
}

// UsdVolumePricingSource is one source's exact trade counts in the window.
// Trades = Priced + Unpriced + Unroutable.
type UsdVolumePricingSource struct {
	Source string `json:"source"`
	Class  string `json:"class"`
	Trades int64  `json:"trades"`
	Priced int64  `json:"priced"`
	// Unpriced has NULL usd_volume on a routable pair; for on-chain sources it
	// includes thin markets, which are not yet separable.
	Unpriced int64 `json:"unpriced"`
	// Unroutable is unpriced with both classic legs from one issuer; excluded
	// from the ratio.
	Unroutable int64 `json:"unroutable"`
	// PricedRatio is priced / (trades - unroutable) as a decimal string;
	// omitted when that denominator is zero.
	PricedRatio string `json:"priced_ratio,omitempty"`
	Bar         string `json:"bar"`
	// MeetsBar is set for external venues only; on-chain is deferred until
	// per-row thin status is stored. Null when there is nothing to judge.
	MeetsBar *bool `json:"meets_bar,omitempty"`
}

const usdVolumePricingMeaning = "How many trades in the window carry a usd_volume. This is valuation coverage, " +
	"not capture: it never changes complete_sources / total_sources / lake_complete_sources. Any USD volume " +
	"summed from these trades is a LOWER BOUND whenever lower_bound is true. On-chain `unpriced` still includes " +
	"thin markets (the substance verdict is not stored per trade), so on-chain meets_bar is not yet published."

const usdVolumePricingExcluded = "trades with NULL usd_volume (unpriced, plus unroutable same-issuer classic pairs) " +
	"are excluded from every USD volume total"

func buildUsdVolumePricingView(snap *usdVolumePricingSnapshot) *UsdVolumePricingAxisView {
	class := map[string]string{}
	for _, s := range usdVolumeExternalSources {
		class[s] = "external"
	}
	for _, s := range usdVolumeOnchainSources {
		class[s] = "onchain"
	}
	v := &UsdVolumePricingAxisView{
		WindowStart: WireTime(snap.start), WindowEnd: WireTime(snap.end), AsOf: WireTime(snap.asOf),
		Meaning: usdVolumePricingMeaning,
		Sources: make([]UsdVolumePricingSource, 0, len(snap.rows)),
	}
	for _, r := range snap.rows {
		cl := class[r.Source]
		bar := usdVolumeOnchainBar
		if cl == "external" {
			bar = usdVolumeExternalBar
		}
		out := UsdVolumePricingSource{
			Source: r.Source, Class: cl, Trades: r.Trades, Priced: r.Priced,
			Unpriced: r.Unpriced, Unroutable: r.Unroutable, Bar: bar,
		}
		if denom := r.Trades - r.Unroutable; denom > 0 {
			ratio := big.NewRat(r.Priced, denom)
			out.PricedRatio = ratio.FloatString(6)
			if cl == "external" {
				barRat, _ := new(big.Rat).SetString(bar)
				met := ratio.Cmp(barRat) >= 0
				out.MeetsBar = &met
			}
		}
		if r.Unpriced > 0 || r.Unroutable > 0 {
			v.LowerBound = true
		}
		v.Sources = append(v.Sources, out)
	}
	if v.LowerBound {
		v.Excluded = usdVolumePricingExcluded
	}
	return v
}
