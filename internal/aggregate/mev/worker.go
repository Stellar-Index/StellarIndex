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

// TradeScanner supplies the recent on-chain trades the detector scans.
// Production: timescale.Store.TradesForArbScan (a ts-windowed, capped
// read of the trades hypertable, on-chain rows with a taker).
type TradeScanner interface {
	TradesForArbScan(ctx context.Context, since time.Time, limit int) ([]canonical.Trade, []string, error)
}

// Sink persists a detected event. InsertMEVEvent is idempotent on
// StoredEvent.DedupKey — it returns inserted=false when the event was
// already present, so a re-scan of an overlapping window doesn't
// double-count; a re-scan whose legs contain the stored legs replaces
// the stored evidence. Production: timescale.Store.
type Sink interface {
	InsertMEVEvent(ctx context.Context, e StoredEvent) (inserted bool, err error)
}

// OracleScanner supplies recent ON-CHAIN oracle updates for the
// oracle_sandwich + liquidation_cascade detectors. Production:
// timescale.Store.OracleUpdatesForMEVScan. Optional — nil skips the
// oracle-correlated detectors.
type OracleScanner interface {
	OracleUpdatesForMEVScan(ctx context.Context, since time.Time, limit int) ([]OracleRef, error)
}

// AuctionScanner supplies recent Blend liquidation-auction fills for
// the cascade detector. Production:
// timescale.Store.BlendFillsForMEVScan. Optional — nil skips cascade
// detection.
type AuctionScanner interface {
	BlendFillsForMEVScan(ctx context.Context, since time.Time, limit int) ([]AuctionFill, error)
}

// TxOrderResolver resolves tx hashes to their intra-ledger
// application order (tx_index) — the signal the served trades table
// does not carry but the raw lake does (stellar.tx_hash_index,
// ADR-0034). Production: clickhouse.TxIndexReader. Optional — nil
// skips the ordering-dependent detectors (sandwich, oracle_sandwich).
type TxOrderResolver interface {
	TxIndexes(ctx context.Context, hashes []string) (map[string]uint32, error)
}

// StoredEvent is the persistence-ready form of a Candidate — the shape
// the mev_events row is built from. DetailJSON is the row's `detail`
// jsonb (assets / sources / legs / notional as evidence).
//
// Canonical definition lives in [domain.MEVStoredEvent] — see the
// [OracleRef] doc (inputs.go) for why this is an alias.
type StoredEvent = domain.MEVStoredEvent

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
// marshalling the evidence into the detail jsonb. Candidates without
// an explicit Detail get the arbitrage evidence shape (the original
// v1 behaviour); every kind's detail carries the candidate's assets
// and sources (detailWithAssets).
func storedFrom(c Candidate) (StoredEvent, error) {
	detail := c.Detail
	if detail == nil {
		detail = arbDetail{
			Assets:      c.Assets,
			Sources:     c.Sources,
			Legs:        c.Legs,
			NotionalUSD: c.NotionalUSD,
			Note:        "Atomic cyclic trade by one taker in a single transaction — an arbitrage signature. Detection is structural; profit is not estimated (leg direction is ambiguous in the served rows).",
		}
	}
	dj, err := detailWithAssets(detail, c)
	if err != nil {
		return StoredEvent{}, err
	}
	txs := c.TxHashes
	if txs == nil {
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

// detailWithAssets marshals a kind's detail and adds the candidate's
// `assets` / `sources` when the kind's own payload does not carry them,
// so no kind's detail loses the per-asset evidence only arbDetail used
// to keep. A detail that is not a JSON object is an error.
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

// Worker runs the MEV detectors on a schedule: each tick it scans the
// last `window` of served rows, runs every detector whose inputs are
// wired, and persists new candidates. Idempotent via the dedup key,
// so overlapping windows are safe.
type Worker struct {
	scanner  TradeScanner
	sink     Sink
	oracles  OracleScanner  // optional
	auctions AuctionScanner // optional
	// order is optional and may be wired after construction (SetOrder) by
	// a background dial retry (K024), while Run is already ticking on
	// another goroutine — hence atomic rather than a plain field.
	order     atomic.Pointer[TxOrderResolver]
	logger    *slog.Logger
	window    time.Duration
	scanLimit int
	obs       Observer
}

// Observer records per-run outcomes. nil → no-op (NopObserver).
// Truncated fires once per input scan (ScanInput*) that hit ScanLimit.
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

// WorkerConfig configures a Worker. Window defaults to 30m, ScanLimit
// to 50_000. The scanners return the NEWEST ScanLimit rows of the
// window; a scan that fills the cap is reported via Observer.Truncated.
//
// Oracles / Auctions / Order are optional inputs: each nil seam
// simply disables the detectors that need it (see the interface docs)
// — detection degrades honestly rather than guessing.
//
// There is no retention knob: the worker never deletes mev_events. The
// detectors only see a trailing window, so a pruned event cannot be
// re-derived, and the served tier keeps what it indexes (launch-plan D5).
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

// SetOrder wires (or re-wires) the tx-order resolver after construction.
// Safe to call concurrently with Run: a background ClickHouse dial retry
// (K024) arms the sandwich/oracle-sandwich detectors once the lake
// answers, rather than the worker either blocking start on that dial or
// giving up on it forever after one failure.
func (w *Worker) SetOrder(o TxOrderResolver) {
	w.order.Store(&o)
}

// RunOnce scans the trailing window once and persists new MEV events
// across every wired detector. Returns (detected, inserted): detected
// is candidates found, inserted is the new (non-duplicate) rows
// written. now is the upper bound of the scan window — injectable for
// tests. Auxiliary inputs (oracle updates, auction fills, lake
// tx-order) are best-effort: a failure there logs + skips the
// detectors that need it, it never fails the run.
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

// scanTrades reads the window's newest scanLimit trades. When the scan
// fills the cap it is reported, and the oldest ledger is dropped: the
// cap may have cut that ledger mid-transaction, and a partial
// transaction can pass the arbitrage checks a complete one fails (a
// dropped leg can lift the single-venue leg-count rejection).
func (w *Worker) scanTrades(ctx context.Context, since time.Time) ([]canonical.Trade, []string, error) {
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

// orderedCandidates runs the tx_index-dependent detectors (sandwich +
// oracle_sandwich) when a lake order resolver is wired. The resolver
// lookup is prefiltered to hashes that could matter
// (OrderingTxHashes), so the lake round-trip stays bounded.
func (w *Worker) orderedCandidates(ctx context.Context, trades []canonical.Trade, usd []string, oracles []OracleRef) []Candidate {
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
