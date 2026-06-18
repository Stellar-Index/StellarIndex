package mev

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
)

// TradeScanner supplies the recent on-chain trades the detector scans.
// Production: timescale.Store.TradesForArbScan (a ts-windowed, capped
// read of the trades hypertable, on-chain rows with a taker).
type TradeScanner interface {
	TradesForArbScan(ctx context.Context, since time.Time, limit int) ([]canonical.Trade, []string, error)
}

// Sink persists a detected event. InsertMEVEvent is idempotent on
// StoredEvent.DedupKey (ON CONFLICT DO NOTHING) — it returns
// inserted=false when the event was already present, so a re-scan of
// an overlapping window doesn't double-count. Production:
// timescale.Store.
type Sink interface {
	InsertMEVEvent(ctx context.Context, e StoredEvent) (inserted bool, err error)
}

// StoredEvent is the persistence-ready form of a Candidate — the shape
// the mev_events row is built from. DetailJSON is the row's `detail`
// jsonb (assets / sources / legs / notional as evidence).
type StoredEvent struct {
	Kind             string
	Ledger           uint32
	DetectedAtLedger uint32
	Timestamp        time.Time
	TxHashes         []string
	Accounts         []string
	NotionalUSD      string // "" → stored NULL
	DedupKey         string
	DetailJSON       []byte
}

// arbDetail is the JSON shape persisted to mev_events.detail for an
// arbitrage event — the evidence a reader needs to verify the cycle.
type arbDetail struct {
	Assets      []string `json:"assets"`
	Sources     []string `json:"sources"`
	Legs        []Leg    `json:"legs"`
	NotionalUSD string   `json:"notional_usd,omitempty"`
	Note        string   `json:"note"`
}

// storedFrom converts a detected Candidate into its persistence form,
// marshalling the evidence into the detail jsonb.
func storedFrom(c Candidate) (StoredEvent, error) {
	detail := arbDetail{
		Assets:      c.Assets,
		Sources:     c.Sources,
		Legs:        c.Legs,
		NotionalUSD: c.NotionalUSD,
		Note:        "Atomic cyclic trade by one taker in a single transaction — an arbitrage signature. Detection is structural; profit is not estimated (leg direction is ambiguous in the served rows).",
	}
	dj, err := json.Marshal(detail)
	if err != nil {
		return StoredEvent{}, fmt.Errorf("mev: marshal detail: %w", err)
	}
	return StoredEvent{
		Kind:             c.Kind,
		Ledger:           c.Ledger,
		DetectedAtLedger: c.DetectedAtLedger,
		Timestamp:        c.Timestamp,
		TxHashes:         []string{c.TxHash},
		Accounts:         []string{c.Taker},
		NotionalUSD:      c.NotionalUSD,
		DedupKey:         c.DedupKey(),
		DetailJSON:       dj,
	}, nil
}

// Worker runs the MEV detector on a schedule: each tick it scans the
// last `window` of trades, detects arbitrage cycles, and persists new
// ones. Idempotent via the dedup key, so overlapping windows are safe.
type Worker struct {
	scanner   TradeScanner
	sink      Sink
	logger    *slog.Logger
	window    time.Duration
	scanLimit int
	obs       Observer
}

// Observer records per-run outcomes. nil → no-op (NopObserver).
type Observer interface {
	Run(outcome string, dur time.Duration, detected, inserted int)
}

// WorkerConfig configures a Worker. Window defaults to 30m, ScanLimit
// to 50_000 — a 30-minute window of on-chain DEX trades is well under
// that cap, and the cap is a backstop against a runaway scan.
type WorkerConfig struct {
	Window    time.Duration
	ScanLimit int
	Logger    *slog.Logger
	Observer  Observer
}

// NewWorker builds a Worker. scanner + sink are required.
func NewWorker(scanner TradeScanner, sink Sink, cfg WorkerConfig) *Worker {
	if cfg.Window <= 0 {
		cfg.Window = 30 * time.Minute
	}
	if cfg.ScanLimit <= 0 {
		cfg.ScanLimit = 50_000
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	obs := cfg.Observer
	if obs == nil {
		obs = nopObserver{}
	}
	return &Worker{
		scanner:   scanner,
		sink:      sink,
		logger:    cfg.Logger,
		window:    cfg.Window,
		scanLimit: cfg.ScanLimit,
		obs:       obs,
	}
}

// RunOnce scans the trailing window once and persists new arbitrage
// events. Returns (detected, inserted): detected is candidates found,
// inserted is the new (non-duplicate) rows written. now is the upper
// bound of the scan window — injectable for tests.
func (w *Worker) RunOnce(ctx context.Context, now time.Time) (detected, inserted int, err error) {
	start := now
	since := now.Add(-w.window)
	trades, usd, err := w.scanner.TradesForArbScan(ctx, since, w.scanLimit)
	if err != nil {
		w.obs.Run("scan_error", time.Since(start), 0, 0)
		return 0, 0, fmt.Errorf("mev: scan trades: %w", err)
	}
	cands := DetectArbitrage(trades, usd)
	detected = len(cands)
	for _, c := range cands {
		ev, mErr := storedFrom(c)
		if mErr != nil {
			w.logger.Warn("mev: skip candidate (marshal)", "tx", c.TxHash, "err", mErr)
			continue
		}
		ok, iErr := w.sink.InsertMEVEvent(ctx, ev)
		if iErr != nil {
			w.obs.Run("write_error", time.Since(start), detected, inserted)
			return detected, inserted, fmt.Errorf("mev: insert event: %w", iErr)
		}
		if ok {
			inserted++
		}
	}
	w.obs.Run("ok", time.Since(start), detected, inserted)
	if inserted > 0 {
		w.logger.Info("mev: detection run", "detected", detected, "inserted", inserted, "scanned", len(trades))
	}
	return detected, inserted, nil
}

// Run drives RunOnce on a ticker until ctx is cancelled. It runs once
// immediately on start, then every `interval`. A failed run is logged
// and retried on the next tick — detection is best-effort analytics,
// not a correctness path. interval defaults to 5m.
func (w *Worker) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	run := func() {
		if _, _, err := w.RunOnce(ctx, time.Now().UTC()); err != nil && ctx.Err() == nil {
			w.logger.Warn("mev: detection run failed", "err", err)
		}
	}
	run()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			run()
		}
	}
}

type nopObserver struct{}

func (nopObserver) Run(string, time.Duration, int, int) {}
