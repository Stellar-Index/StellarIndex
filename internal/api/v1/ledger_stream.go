package v1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

const (
	// ledgerStreamProducerQueueDepth is the per-connection buffer between producer and SSE writer; matches
	// tipStreamProducerQueueDepth, small so a wedged writer is detected promptly (next send blocks, ctx teardown).
	ledgerStreamProducerQueueDepth = 4

	// ledgerStreamPollInterval is how often the producer re-reads the ingest cursor: the indexer commits a ledger every ~5s and
	// 500ms surfaces each promptly; the read is shared across connections (see ledgerStreamTip), so cost does not scale with
	// clients.
	ledgerStreamPollInterval = 500 * time.Millisecond

	// ledgerStreamRefreshInterval forces an emit even when the ledger has NOT advanced, so lag_seconds stays current; otherwise a
	// stalled indexer would leave the client on a stale, healthy-looking lag.
	ledgerStreamRefreshInterval = 10 * time.Second

	// ledgerStreamTickTimeout bounds one per-tick cursors read: RequestTimeout excludes `/stream` paths, so without it a slow
	// ListCursors could hold this goroutine and a DB connection open indefinitely per connection. Mirrors observationsScanTimeout.
	ledgerStreamTickTimeout = 8 * time.Second

	// ledgerStreamTipTTL is how long one cursors read serves every open
	// connection, so the DB cost of the poll is flat in connection count.
	ledgerStreamTipTTL = ledgerStreamPollInterval / 2
)

// ledgerStreamTip is ledgerTip shared across every /v1/ledger/stream connection: callers join one in-flight read and a success
// is reused for ledgerStreamTipTTL; the caller's ctx bounds only its own wait.
func (s *Server) ledgerStreamTip(ctx context.Context) (LedgerTipView, bool, error) {
	s.ledgerStreamMu.Lock()
	if !s.ledgerStreamAt.IsZero() && time.Since(s.ledgerStreamAt) < ledgerStreamTipTTL {
		view, ok := s.ledgerStreamView, s.ledgerStreamOK
		s.ledgerStreamMu.Unlock()
		return view, ok, nil
	}
	s.ledgerStreamMu.Unlock()

	select {
	case <-ctx.Done():
		return LedgerTipView{}, false, ctx.Err()
	case r := <-s.refreshLedgerStreamTip(): //nolint:contextcheck // shared read must not die with whichever client started it
		if r.Err != nil {
			return LedgerTipView{}, false, r.Err
		}
		res, ok := r.Val.(ledgerStreamTipResult)
		if !ok {
			return LedgerTipView{}, false, errLedgerStreamTipPanicked
		}
		return res.view, res.ok, nil
	}
}

// refreshLedgerStreamTip starts or joins the one in-flight cursors read on its own bounded context: tied to the first caller's,
// one disconnect would fail the read for every waiter.
func (s *Server) refreshLedgerStreamTip() <-chan singleflight.Result {
	return s.ledgerStreamFlight.DoChan("tip", func() (any, error) {
		// A panic here would otherwise be re-raised on a bare goroutine and
		// take down the process; recovered, waiters see a nil Val.
		defer worker.Recover(s.logger, "api-ledger-stream-tip")
		ctx, cancel := context.WithTimeout(context.Background(), ledgerStreamTickTimeout)
		defer cancel()
		view, ok, err := s.ledgerTip(ctx)
		if err != nil {
			return nil, err
		}
		s.ledgerStreamMu.Lock()
		s.ledgerStreamView, s.ledgerStreamOK, s.ledgerStreamAt = view, ok, time.Now()
		s.ledgerStreamMu.Unlock()
		return ledgerStreamTipResult{view: view, ok: ok}, nil
	})
}

var errLedgerStreamTipPanicked = errors.New("ledger tip read panicked")

type ledgerStreamTipResult struct {
	view LedgerTipView
	ok   bool
}

// handleLedgerStream serves GET /v1/ledger/stream, the SSE counterpart of /v1/ledger/tip: a `ledger_update` per live-ingest advance, plus a
// refresh every ~10s if unchanged so lag_seconds never goes stale in a stall. The initial event is the current tip (no Last-Event-ID
// resume). Pre-stream errors are problem+json with the right status, as a non-200 cannot be set once the SSE body starts.
func (s *Server) handleLedgerStream(w http.ResponseWriter, r *http.Request) {
	// Admit against the concurrency caps FIRST, so a client at its stream cap does not pay for the
	// pre-flight cursors read before being rejected. release is idempotent and deferred so every
	// return path releases once; StreamFromChannelPreAdmitted below avoids acquiring a second slot.
	release, ok := streaming.TryAcquireStreamSlot(w, r)
	if !ok {
		return
	}
	defer release()

	if s.Cursors == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/ledger-tip-unavailable",
			"Ledger tip not available", http.StatusServiceUnavailable,
			"this deployment has no CursorsReader wired — check binary configuration")
		return
	}

	// First synchronous read: the last chance to return a non-200 before SSE mode. Bounded, as on
	// the tip stream's pre-flight; unbounded it would hold a handler goroutine and a pool
	// connection for as long as the client stayed connected.
	preflightCtx, cancelPreflight := context.WithTimeout(r.Context(), ledgerStreamTickTimeout)
	defer cancelPreflight()
	first, ok, err := s.ledgerStreamTip(preflightCtx)
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("ledgerTip failed (stream prelude)", "err", err)
		writeProblemErr(w, r, err,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	if !ok {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/ledger-tip-unavailable",
			"Ledger tip not available", http.StatusServiceUnavailable,
			"the live-ingest cursor has not been established yet — the indexer "+
				"has not committed its first ledger on this deployment")
		return
	}

	ch := make(chan streaming.Event, ledgerStreamProducerQueueDepth)
	prodCtx, cancelProd := context.WithCancel(r.Context())
	defer cancelProd()

	go s.runLedgerStreamProducer(prodCtx, ch, first)

	streaming.StreamFromChannelPreAdmitted(w, r, ch, s.streamOptions())
}

// runLedgerStreamProducer is the per-connection poll loop: initial event, then emit when the ledger advanced or
// ledgerStreamRefreshInterval elapsed; closes ch on ctx cancel; the per-tick decision lives in nextLedgerEvent.
func (s *Server) runLedgerStreamProducer(
	ctx context.Context,
	ch chan<- streaming.Event,
	first LedgerTipView,
) {
	// This producer runs in its OWN goroutine, so an unrecovered panic would terminate the WHOLE
	// process (middleware.Recoverer only wraps the handler goroutine) on an unauthenticated stream.
	// Recover so a panic tears down only this connection. Registered BEFORE `defer close(ch)` so
	// the close runs FIRST (LIFO) and the SSE writer ends the response cleanly, then this logs.
	// Never swallow it silently.
	defer s.recoverStreamProducer("ledger")
	defer close(ch)

	var gen streaming.Generator
	lastLedger := first.LatestLedger
	lastEmit := time.Now()

	if ev, ok := ledgerStreamEvent(&gen, first); ok {
		if !sendStreamEvent(ctx, ch, ev) {
			return
		}
	}

	ticker := time.NewTicker(ledgerStreamPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ev, view, ok := s.nextLedgerEvent(ctx, &gen, lastLedger, lastEmit)
			if !ok {
				continue
			}
			if !sendStreamEvent(ctx, ch, ev) {
				return
			}
			lastLedger = view.LatestLedger
			lastEmit = time.Now()
		}
	}
}

// nextLedgerEvent runs one producer tick; ok=false (tick skipped, heartbeats keep the connection alive) on a transient read
// failure, an unestablished cursor, or a ledger that neither advanced nor aged past the refresh.
func (s *Server) nextLedgerEvent(
	ctx context.Context,
	gen *streaming.Generator,
	lastLedger uint32,
	lastEmit time.Time,
) (streaming.Event, LedgerTipView, bool) {
	tickCtx, cancel := context.WithTimeout(ctx, ledgerStreamTickTimeout)
	defer cancel()
	view, ok, err := s.ledgerStreamTip(tickCtx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("ledgerTip failed (stream tick) — skipping emit", "err", err)
		}
		return streaming.Event{}, LedgerTipView{}, false
	}
	if !ok {
		return streaming.Event{}, LedgerTipView{}, false
	}
	advanced := view.LatestLedger > lastLedger
	refresh := time.Since(lastEmit) >= ledgerStreamRefreshInterval
	if !advanced && !refresh {
		return streaming.Event{}, LedgerTipView{}, false
	}
	ev, ok := ledgerStreamEvent(gen, view)
	if !ok {
		return streaming.Event{}, LedgerTipView{}, false
	}
	return ev, view, true
}

// sendStreamEvent pushes one event onto the connection channel, returning false if ctx cancelled first (client disconnect) so
// the producer tears down.
func sendStreamEvent(ctx context.Context, ch chan<- streaming.Event, ev streaming.Event) bool {
	select {
	case <-ctx.Done():
		return false
	case ch <- ev:
		return true
	}
}

// ledgerStreamPayload is the SSE data of a ledger_update; mirrors the /v1/ledger/tip envelope (data + as_of) so an SDK decodes
// polled and streamed responses with one type.
type ledgerStreamPayload struct {
	Data LedgerTipView `json:"data"`
	AsOf WireTime      `json:"as_of"`
}

// ledgerStreamEvent builds one ledger_update event; (_, false) on a JSON-marshal failure (a programming error in LedgerTipView)
// so the caller skips the emit and keeps the stream alive.
func ledgerStreamEvent(gen *streaming.Generator, view LedgerTipView) (streaming.Event, bool) {
	body, err := json.Marshal(ledgerStreamPayload{
		Data: view,
		AsOf: WireTime(time.Now().UTC()),
	})
	if err != nil {
		return streaming.Event{}, false
	}
	return streaming.Event{
		ID:   gen.Next(),
		Type: "ledger_update",
		Data: body,
	}, true
}

// recoverStreamProducer is the deferred panic guard for SSE producers; MUST be invoked as `defer
// s.recoverStreamProducer("<stream>")` from the producer itself, as recover() only works in a function the panicking goroutine
// deferred.
func (s *Server) recoverStreamProducer(stream string) {
	if r := recover(); r != nil {
		// Report rather than only log: this is the ONE counter the stellarindex_worker_panics_total
		// page rule reads, so a dead producer reaches an operator with a runbook. The stream name
		// becomes the worker label, telling the operator WHICH stream stopped.
		worker.Report(s.logger, "api-sse-"+stream, r)
	}
}
