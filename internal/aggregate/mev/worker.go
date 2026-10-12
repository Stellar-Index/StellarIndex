package mev

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
)

// TradeScanner supplies recent on-chain trades with a taker; production is timescale.Store.TradesForArbScan.
type TradeScanner interface {
	TradesForArbScan(ctx context.Context, since time.Time, limit int) ([]canonical.Trade, []domain.MEVLegUSD, error)
}

// Sink persists a detected event; InsertMEVEvent is idempotent on StoredEvent.DedupKey (inserted=false if present) and a re-scan with a superset of legs replaces the stored evidence.
type Sink interface {
	InsertMEVEvent(ctx context.Context, e StoredEvent) (inserted bool, err error)
}

// OracleScanner supplies recent ON-CHAIN oracle updates; optional, nil skips the oracle-correlated detectors.
type OracleScanner interface {
	OracleUpdatesForMEVScan(ctx context.Context, since time.Time, limit int) ([]OracleRef, error)
}

// AuctionScanner supplies recent Blend liquidation fills; optional, nil skips cascade detection.
type AuctionScanner interface {
	BlendFillsForMEVScan(ctx context.Context, since time.Time, limit int) ([]AuctionFill, error)
}

// TxOrderResolver resolves tx hashes to intra-ledger tx_index from the raw lake (ADR-0034); optional, nil skips the sandwich detectors.
type TxOrderResolver interface {
	TxIndexes(ctx context.Context, hashes []string) (map[string]uint32, error)
}

// StoredEvent is the persistence-ready Candidate; an alias of [domain.MEVStoredEvent].
type StoredEvent = domain.MEVStoredEvent

// arbDetail is the JSON shape persisted to mev_events.detail for an
// arbitrage event — the evidence a reader needs to verify the cycle.
type arbDetail struct {
	Assets             []string `json:"assets"`
	Sources            []string `json:"sources"`
	Legs               []Leg    `json:"legs"`
	NotionalUSD        string   `json:"notional_usd,omitempty"`
	NotionalLowerBound bool     `json:"notional_usd_lower_bound,omitempty"`
	NotionalEstimated  bool     `json:"notional_usd_estimated,omitempty"`
	Note               string   `json:"note"`
}

// storedFrom converts a Candidate to its persistence form; candidates without Detail get the arbitrage evidence shape, and every detail carries assets and sources.
func storedFrom(c Candidate) (StoredEvent, error) {
	detail := c.Detail
	if detail == nil {
		detail = arbDetail{
			Assets:      c.Assets,
			Sources:     c.Sources,
			Legs:        c.Legs,
			NotionalUSD: c.NotionalUSD,

			NotionalLowerBound: c.NotionalLowerBound,
			NotionalEstimated:  c.NotionalEstimated,
			Note:               "Atomic cyclic trade by one taker in a single transaction — an arbitrage signature. Detection is structural; profit is not estimated (leg direction is ambiguous in the served rows).",
		}
	}
	dj, err := detailWithAssets(detail, c)
	if err != nil {
		return StoredEvent{}, err
	}
	txs := c.TxHashes
	if len(txs) == 0 {
		if c.TxHash == "" {
			return StoredEvent{}, fmt.Errorf("mev: %s candidate names no transaction", c.Kind)
		}
		txs = []string{c.TxHash}
	}
	accts := c.Accounts
	if accts == nil {
		accts = []string{c.Taker}
	}
	return StoredEvent{
		Kind:             c.Kind,
		Ledger:           c.Ledger,
		DetectedAtLedger: c.DetectedAtLedger,
		Timestamp:        c.Timestamp,
		AssetID:          c.AssetID,
		QuoteID:          c.QuoteID,
		TxHashes:         txs,
		Accounts:         accts,
		DedupKey:         c.DedupKey(),
		DetailJSON:       dj,
	}, nil
}

// detailWithAssets marshals a kind's detail and adds the candidate's `assets` / `sources` when the payload lacks them, so no kind loses
// per-asset evidence. A detail that is not a JSON object is an error.
func detailWithAssets(detail any, c Candidate) ([]byte, error) {
	dj, err := json.Marshal(detail)
	if err != nil {
		return nil, fmt.Errorf("mev: marshal detail: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(dj, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("mev: %s detail is not a JSON object", c.Kind)
	}
	for key, vals := range map[string][]string{"assets": c.Assets, "sources": c.Sources} {
		if _, has := fields[key]; has || len(vals) == 0 {
			continue
		}
		b, mErr := json.Marshal(vals)
		if mErr != nil {
			return nil, fmt.Errorf("mev: marshal detail %s: %w", key, mErr)
		}
		fields[key] = b
	}
	return json.Marshal(fields)
}

// Worker runs the MEV detectors each tick over the trailing window; idempotent via the dedup key, so overlapping windows are safe.
type Worker struct {
	scanner  TradeScanner
	sink     Sink
	oracles  OracleScanner  // optional
	auctions AuctionScanner // optional
	// order is optional and may be wired after construction (SetOrder) by
	// a background dial retry, while Run is already ticking on
	// another goroutine — hence atomic rather than a plain field.
	order     atomic.Pointer[TxOrderResolver]
	logger    *slog.Logger
	window    time.Duration
	scanLimit int
	obs       Observer
}

// Observer records per-run outcomes (nil is a no-op); Truncated fires once per input scan that hit ScanLimit.
type Observer interface {
	Run(outcome string, dur time.Duration, detected, inserted int)
	Truncated(input string)
}

// Input names reported to Observer.Truncated.
const (
	ScanInputTrades   = "trades"
	ScanInputOracles  = "oracle_updates"
	ScanInputAuctions = "auction_fills"
)

// WorkerConfig configures a Worker (Window 30m, ScanLimit 50_000; a capped scan is reported via Observer.Truncated). A nil Oracles/Auctions/Order seam disables its detectors rather than guessing. The worker never deletes mev_events: a pruned event cannot be re-derived from the trailing window.
type WorkerConfig struct {
	Window    time.Duration
	ScanLimit int
	Logger    *slog.Logger
	Observer  Observer
	Oracles   OracleScanner
	Auctions  AuctionScanner
	Order     TxOrderResolver
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
	w := &Worker{
		scanner:   scanner,
		sink:      sink,
		oracles:   cfg.Oracles,
		auctions:  cfg.Auctions,
		logger:    cfg.Logger,
		window:    cfg.Window,
		scanLimit: cfg.ScanLimit,
		obs:       obs,
	}
	if cfg.Order != nil {
		w.SetOrder(cfg.Order)
	}
	return w
}

// SetOrder wires or re-wires the tx-order resolver; safe to call concurrently with Run (a background lake dial retry arms the sandwich detectors).
func (w *Worker) SetOrder(o TxOrderResolver) {
	w.order.Store(&o)
}

// RunOnce scans the trailing window once, returning (detected, inserted); now is the window's upper bound. Auxiliary inputs are best-effort: a failure skips their detectors, never the run.
func (w *Worker) RunOnce(ctx context.Context, now time.Time) (detected, inserted int, err error) {
	start := now
	since := now.Add(-w.window)
	trades, usd, err := w.scanTrades(ctx, since)
	if err != nil {
		w.obs.Run("scan_error", time.Since(start), 0, 0)
		return 0, 0, fmt.Errorf("mev: scan trades: %w", err)
	}
	cands := DetectArbitrage(trades, usd)
	cands = append(cands, DetectWashTrades(trades, usd)...)

	oracles := w.scanOracles(ctx, since)
	cands = append(cands, w.orderedCandidates(ctx, trades, usd, oracles)...)
	cands = append(cands, w.cascadeCandidates(ctx, since, oracles)...)
	detected = len(cands)
	for _, c := range cands {
		ev, mErr := storedFrom(c)
		if mErr != nil {
			w.logger.Warn("mev: skip candidate", "tx", c.TxHash, "err", mErr)
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

// Run drives RunOnce on a ticker (immediately, then every interval, default 5m) until ctx is cancelled; failures are logged and retried next tick.
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

// scanTrades reads the window's newest scanLimit trades. A capped scan is reported and its oldest ledger dropped:
// the cap may cut it mid-transaction, and a dropped leg can lift the single-venue leg-count rejection.
func (w *Worker) scanTrades(ctx context.Context, since time.Time) ([]canonical.Trade, []domain.MEVLegUSD, error) {
	trades, usd, err := w.scanner.TradesForArbScan(ctx, since, w.scanLimit)
	if err != nil || len(trades) < w.scanLimit {
		return trades, usd, err
	}
	w.obs.Truncated(ScanInputTrades)
	w.logger.Warn("mev: trade scan hit the cap — detecting over the newest rows only",
		"cap", w.scanLimit, "window", w.window)
	cut := 0
	for cut < len(trades) && trades[cut].Ledger == trades[0].Ledger {
		cut++
	}
	if cut > len(usd) {
		return trades[cut:], nil, nil
	}
	return trades[cut:], usd[cut:], nil
}

// scanOracles fetches the window's on-chain oracle updates.
// Best-effort: nil scanner or an error → nil (the oracle-correlated
// detectors just don't run this tick).
func (w *Worker) scanOracles(ctx context.Context, since time.Time) []OracleRef {
	if w.oracles == nil {
		return nil
	}
	oracles, err := w.oracles.OracleUpdatesForMEVScan(ctx, since, w.scanLimit)
	if err != nil {
		if ctx.Err() == nil {
			w.logger.Warn("mev: oracle scan failed — skipping oracle-correlated detectors this tick", "err", err)
		}
		return nil
	}
	if len(oracles) >= w.scanLimit {
		w.obs.Truncated(ScanInputOracles)
	}
	return oracles
}

// orderedCandidates runs the tx_index-dependent detectors (sandwich + oracle_sandwich) when a lake order resolver is wired; lookups are
// prefiltered to OrderingTxHashes so the lake round-trip stays bounded.
func (w *Worker) orderedCandidates(ctx context.Context, trades []canonical.Trade, usd []domain.MEVLegUSD, oracles []OracleRef) []Candidate {
	orderPtr := w.order.Load()
	if orderPtr == nil {
		return nil
	}
	order := *orderPtr
	hashes := OrderingTxHashes(trades, oracles)
	if len(hashes) == 0 {
		return nil
	}
	txIdx, err := order.TxIndexes(ctx, hashes)
	if err != nil {
		if ctx.Err() == nil {
			w.logger.Warn("mev: lake tx-order lookup failed — skipping sandwich detectors this tick", "err", err, "hashes", len(hashes))
		}
		return nil
	}
	out := DetectSandwiches(trades, usd, txIdx)
	return append(out, DetectOracleSandwiches(trades, usd, oracles, txIdx)...)
}

// cascadeCandidates runs the liquidation-cascade detector when an
// auction scanner is wired. The oracle-correlation requirement means
// no oracles → no candidates, so it short-circuits.
func (w *Worker) cascadeCandidates(ctx context.Context, since time.Time, oracles []OracleRef) []Candidate {
	if w.auctions == nil || len(oracles) == 0 {
		return nil
	}
	fills, err := w.auctions.BlendFillsForMEVScan(ctx, since, w.scanLimit)
	if err != nil {
		if ctx.Err() == nil {
			w.logger.Warn("mev: auction-fill scan failed — skipping cascade detection this tick", "err", err)
		}
		return nil
	}
	if len(fills) >= w.scanLimit {
		w.obs.Truncated(ScanInputAuctions)
	}
	return DetectLiquidationCascades(fills, oracles)
}

type nopObserver struct{}

func (nopObserver) Run(string, time.Duration, int, int) {}

func (nopObserver) Truncated(string) {}
