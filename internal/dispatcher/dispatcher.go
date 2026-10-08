// Package dispatcher routes the ledger-meta values from internal/ledgerstream to the decoders registered by
// internal/sources/<venue>. It is the SINGLE production ingest codepath (docs/architecture/ingest-pipeline.md):
// every trade and oracle update in Timescale goes through Dispatcher.ProcessLedger.
//
// Decoders carry all protocol logic; the per-ledger walk hits three seams:
//
//   - [Decoder]: Soroban contract events; the first decoder whose Matches() is true owns each event.
//   - [OpDecoder]: classic operations (SDEX offers, path payments); EVERY matching OpDecoder runs, since
//     one op can be facts in several domains (a path payment is a trade and a movement).
//   - [ContractCallDecoder]: event-less Soroban contracts, matched by (contract_id, function_name) and
//     decoded from the InvokeContract args (Band relay / force_relay).
//
// Decode errors are logged and counted, never fatal. A new source registers against the seam matching its
// wire shape. The event and contract-call seams also sniff for discovery (sighting only, never attribution;
// docs/architecture/oracle-manipulation-defense.md), so an event-less Band-shaped oracle is flagged before
// any decoder for it exists.
package dispatcher

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical/discovery"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/entrywalk"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder is what a source package implements to join event dispatch: export a NewDecoder() and register it
// at startup. Name is stamped into metrics and Trade/OracleUpdate.Source; Matches is a cheap byte-equality
// topic check (no SCVal parsing); Decode may emit nothing for intermediate events of a correlated sequence
// (Soroswap swap+sync, Phoenix 8-field). A Decode error is counted and the next event proceeds.
type Decoder interface {
	Name() string
	Matches(ev events.Event) bool
	Decode(ev events.Event) ([]consumer.Event, error)
}

// Drainer is an OPTIONAL interface a stateful [Decoder] implements to flush
// correlation groups still buffered when a bounded stream ends. Without it a
// group that only an age sweep or a later event would emit (a pre-upgrade
// phoenix swap, a soroswap swap with no following sync) is lost with the
// range's last events. Drain also empties the buffers.
type Drainer interface {
	Drain() []consumer.Event
}

// Drain flushes dec if it is a [Drainer]; any other value yields nil.
func Drain(dec any) []consumer.Event {
	if dr, ok := dec.(Drainer); ok {
		return dr.Drain()
	}
	return nil
}

// StateWriteKeyConsumer is an optional [Decoder] extension declaring the contracts whose events need
// events.Event.StateWriteKeys. ProcessLedger resolves the value-changing write keys (state_write_keys.go) only
// for those: the meta walk and XDR marshalling are measurable overhead, and one decoder (redstone) reads them.
// Other events carry nil, which consumers must read as "unknown", not "no writes".
type StateWriteKeyConsumer interface {
	StateWriteContracts() []string
}

// stateWriteContracts collects the union of every registered decoder's
// declared [StateWriteKeyConsumer] contract set. Recomputed per
// ProcessLedger call — the decoder list is tiny and fixed after
// startup, so this is a handful of type asserts per ledger.
func (d *Dispatcher) stateWriteContracts() map[string]bool {
	var set map[string]bool
	for _, dec := range d.decoders {
		swc, ok := dec.(StateWriteKeyConsumer)
		if !ok {
			continue
		}
		for _, cid := range swc.StateWriteContracts() {
			if set == nil {
				set = make(map[string]bool)
			}
			set[cid] = true
		}
	}
	return set
}

// OpDecoder decodes classic Stellar operations (ManageOffer, PathPayment, …) rather than Soroban events;
// SDEX is the main user. OpContext carries the op and its result so the decoder needn't re-walk the envelope.
// Errors are skip-and-count, as for [Decoder]. Unlike [Decoder], op-type sets may overlap: every matching
// OpDecoder runs, so each must emit only its own domain's facts.
type OpDecoder interface {
	Name() string
	// Matches is a cheap predicate on the op (typically checks
	// op.Body.Type). Called before Decode.
	Matches(op xdr.Operation) bool
	// Decode emits zero or more canonical outputs from one op +
	// its result. See OpContext for the per-op fields available.
	Decode(ctx OpContext) ([]consumer.Event, error)
}

// OpContext carries everything an OpDecoder needs to decode one
// classic operation: the op itself, its result, and the tx-level
// metadata (ledger, close time, tx hash, source account). Built by
// the dispatcher during ProcessLedger.
type OpContext struct {
	Ledger   uint32
	ClosedAt time.Time
	TxHash   string
	// TxSource is the strkey G-address of the transaction source
	// account — the account charged the fee, often the one whose
	// offer was placed. Ops can override via their own SourceAccount
	// (see OpSource).
	TxSource string
	// OpSource is the strkey of the per-op source account when the
	// op carries one, otherwise empty (meaning "tx-level source").
	// Classic trades don't strictly need this (the claim atoms
	// identify the counterparty), but it's useful for attribution.
	OpSource string
	OpIndex  int
	Op       xdr.Operation
	OpResult xdr.OperationResult
}

// ContractCallDecoder observes Soroban InvokeContract calls whether or not the contract emits an event. Band's
// relay() / force_relay() update storage but publish nothing (docs/protocols/band.md), so the call args are
// the authoritative payload. Matching is by (contract_id, function_name), cheap string compares; the source
// package decodes the args. Errors are skip-and-count, as for [Decoder].
type ContractCallDecoder interface {
	Name() string
	// Matches reports whether this decoder owns the given call.
	// contractID is the C-strkey of the invoked contract;
	// functionName is the Symbol the caller targets (e.g. "relay").
	Matches(contractID, functionName string) bool
	// Decode emits zero or more canonical outputs from one
	// InvokeContract call. See ContractCallContext for the fields
	// available at decode time.
	Decode(ctx ContractCallContext) ([]consumer.Event, error)
}

// ExecutionCorroborationRequirer is an optional [ContractCallDecoder] extension: the dispatcher drops any
// matched call whose [ContractCallContext.ExecutionCorroborated] is false instead of decoding it. Routing walks
// the submitter-supplied auth tree, and the host does not require every declared authorization to execute, so
// a successful tx can carry an auth entry naming an oracle with FORGED price args that never ran. Price
// oracles implement it (Band: its call args ARE the price); decoders that don't (trade-volume routers) trust
// the walked tree.
type ExecutionCorroborationRequirer interface {
	// RequiresExecutionCorroboration reports whether this decoder's
	// calls must be execution-corroborated. Returning false is
	// equivalent to not implementing the interface at all.
	RequiresExecutionCorroboration() bool
}

// RefusesUncorroborated reports whether dec must skip a matched call whose
// ExecutionCorroborated flag is false. Shared by the live dispatcher and the
// lake re-derive so both apply the same gate.
func RefusesUncorroborated(dec ContractCallDecoder, corroborated bool) bool {
	r, ok := dec.(ExecutionCorroborationRequirer)
	return ok && r.RequiresExecutionCorroboration() && !corroborated
}

// executionCorroborated reports whether call is the op's top-level executed
// invocation (top; nil for a non-InvokeContract op) rather than an auth-tree
// declaration that may never have run.
func executionCorroborated(top, call *invokeCall) bool {
	return top != nil && sameInvocation(top, call)
}

// ContractCallContext is what a ContractCallDecoder needs to decode one InvokeContract call, built by
// ProcessLedger for every successful one. Args are base64 SCVal blobs (the events.Event.OpArgs / Topic
// format), unwrapped with internal/scval.Parse.
type ContractCallContext struct {
	Ledger       uint32
	ClosedAt     time.Time
	TxHash       string
	TxSource     string
	OpSource     string
	OpIndex      int
	ContractID   string // C-strkey of invoked contract
	FunctionName string // Symbol the caller invoked
	Args         []string
	// CallPath identifies this call's position in the op's
	// auth tree. Empty for the top-level call; non-empty for
	// sub-invocations (per-step indices in pre-order traversal,
	// e.g. [0,1] = second sub-call of the first sub-call of root).
	// Per ADR-0052 (docs/adr/0052-contract-call-tree-routing.md).
	// Decoders that need to dedup overlapping calls in the same tx
	// (rare) can build a stable identifier as (TxHash, OpIndex, CallPath).
	CallPath []int
	// CallPathContracts is the ordered contract C-strkeys from the top-level invocation down to this call,
	// index-aligned with CallPath's depth ([ContractID] for a top-level call). [0] is the outermost (e.g. an
	// aggregator) and the last always equals ContractID, so a decoder can record WHO wrapped the call.
	CallPathContracts []string
	// AuthOccurrence is how many byte-identical calls (same contract,
	// function, args) precede this one in the SAME auth entry: 0 for the
	// first. Each node of one entry authorizes its own execution, so a
	// second identical node is a second call; the same call repeated in a
	// DIFFERENT entry (co-signing) keeps its ordinal and stays a duplicate.
	// A decoder keying rows on call content adds this to tell them apart.
	AuthOccurrence int
	// ExecutionCorroborated is true iff this call equals the op's top-level executed InvokeContract call (same
	// contract, function and args). Anything else was only DECLARED in the submitter-supplied auth tree and may
	// never have run; a decoder whose output is a manipulation surface ([ExecutionCorroborationRequirer]) must
	// refuse such calls.
	ExecutionCorroborated bool
}

// LedgerEntryChangeDecoder observes raw [xdr.LedgerEntryChange] rows from each LCM regardless of which
// transaction or fee-meta block produced them (ADR-0021), for sources that derive state from ledger-entry
// deltas, e.g. the AccountEntry observer (internal/sources/accounts/). Matches is the cheap pre-filter on the
// entry's Data discriminant; errors are skip-and-count, as for [Decoder].
type LedgerEntryChangeDecoder interface {
	Name() string
	Matches(change xdr.LedgerEntryChange) bool
	Decode(ctx LedgerEntryChangeContext) ([]consumer.Event, error)
}

// LedgerEntryChangeContext carries everything a
// [LedgerEntryChangeDecoder] needs to decode one entry-change
// delta: identity of the change + the tx-level metadata that
// produced it. Built by the dispatcher during ProcessLedger.
//
// FeeMeta-block changes carry an empty TxHash and OpIndex == -1
// to distinguish from per-op changes (the fee debit on the source
// account is technically tx-level, not op-level).

// EntryWalkVersion identifies the entry-change walk ORDER behind [LedgerEntryChangeContext.IntraLedgerSeq].
// 1: per-tx walk, failed txs skipped. 2: ledger-wide three-phase walk, failed txs included. 3: each block in
// entrywalk.Canonical (ledger key) order, since core's export order is hash-map iteration and varies.
//
// Bump it only when a change RENUMBERS existing positions; the eviction phase appends after phases 1–3, so it
// did not. Positions are persisted and compared across binaries: account_observations and its siblings guard
// on `(walk_version, intra_ledger_seq) <= EXCLUDED`, so a renumbering shipped without a bump silently drops
// every correction (repair: reconstruct-final-then-seed, timescale.SeedIntraLedgerSeq). ledger_entries_current_v2
// carries no walk version, so there the range must be deleted and reprojected.
// Procedure: docs/operations/runbooks/entry-walk-renumbering.md.
const EntryWalkVersion = 3

type LedgerEntryChangeContext struct {
	Ledger   uint32
	ClosedAt time.Time
	TxHash   string
	OpIndex  int

	// IntraLedgerSeq is this change's position in the ledger's canonical walk, in LEDGER-WIDE PHASE order (all
	// fees, then all apply-phase meta, then P23 post-apply fee refunds), mirroring the SDK's LedgerChangeReader and
	// how core commits a ledger. So the HIGHEST value for an entry is its final state; a per-tx walk would publish
	// a fee-phase balance as ledger-final. Balance writers guard their upsert on it so an out-of-order worker
	// never overwrites a later change; only within-ledger monotonicity matters, and gaps are harmless.
	// Positions are comparable only within one [EntryWalkVersion]; read it before any corrective re-derive.
	IntraLedgerSeq uint32

	Change xdr.LedgerEntryChange
}

// Dispatcher owns the registered decoders: construct with New(), register once at startup, then call
// ProcessLedger per LedgerCloseMeta. ProcessLedger is not concurrency-safe (the ledgerstream callback
// serialises it); the stats counters are, under statsMu, for the statsflush reader.
type Dispatcher struct {
	decoders             []Decoder
	opDecoders           []OpDecoder
	contractCallDecoders []ContractCallDecoder
	entryDecoders        []LedgerEntryChangeDecoder

	// discoverySink, when non-nil, receives every SEP-41-shaped event
	// observed by [dispatchOne]. The dispatcher calls Sniff on each
	// event and forwards [discovery.Hit] records via Push. A nil sink
	// disables discovery — the dispatcher behaves as if the hook
	// weren't there. See [Dispatcher.SetDiscoverySink].
	discoverySink DiscoverySink

	// rawEventSink, when non-nil, receives EVERY Soroban contract event [dispatchOne] sees, claimed or not,
	// feeding the soroban_events landing zone (ADR-0029) so decoder backfills become SQL, not MinIO re-walks.
	// See [Dispatcher.SetRawEventSink].
	rawEventSink RawEventSink

	// logger serves the decoder-panic guard (recordDecoderPanic) and unreadable evicted keys (walkEvictedKeys),
	// the two signals that must carry a ledger coordinate; everything else is a counter. Nil falls back to
	// slog.Default(); see [Dispatcher.SetLogger].
	logger *slog.Logger

	// statsMu guards the counter fields below (eventsSeen through uncorroboratedCalls): ProcessLedger writes them
	// while statsflush reads Stats(), and unguarded that is a fatal concurrent map access. Critical sections are
	// one `++` or one snapshot copy, so the hot path pays an uncontended lock per matched input.
	statsMu sync.Mutex

	// Per-source events_seen — bumped every time a decoder's
	// Matches() claims an input (event / contract call / entry
	// change / op). Counts the denominator of "decoder error rate"
	// the statsflush flusher writes to decoder_stats_5m. Bumped
	// pre-Decode, so a decoder that matches and then errors still
	// shows up in the events_seen count — which is what makes the
	// error-rate signal meaningful.
	eventsSeen map[string]int

	// Error counters — read via Stats(). Production wiring in
	// cmd/stellarindex-indexer increments obs.SourceDecodeErrorsTotal
	// per source name on decode failures; internal counters here are
	// for test assertions.
	decodeErrors  map[string]int
	unmatchedHits int
	// txReadErrors is the count of malformed transactions skipped
	// during ProcessLedger. Bumped when LedgerTransactionReader.Read
	// returns a non-EOF error — the tx is dropped (one bad tx must
	// not abort the whole ledger) but we want operators to see the
	// signal rather than have it disappear silently. A silent skip
	// would hide a slow corruption (ingest seeing a real drop rate)
	// until a downstream price gap triggered a manual investigation.
	txReadErrors int

	// txEventReadErrors counts txs whose GetTransactionEvents() errored, as the SDK does for an unsupported
	// TransactionMeta version. On a meta bump every Soroban event would vanish while soroban_events and the census
	// dropped in lock-step and the ADR-0033 reconcile still read "complete"; a sustained climb means Soroban
	// ingestion is broken whatever the completeness verdict says.
	txEventReadErrors int

	// entryMetaUnsupported counts txs whose apply-phase entry-change walk was skipped for an unhandled meta
	// version. Unreachable today (galexie regenerates meta at replay, verified across protocols 1→19), so non-zero
	// means an archive from an old core or meta past V4, and every classic balance / trustline / offer / LP
	// change in those txs is invisible.
	entryMetaUnsupported int

	// evictedKeysUnreadable counts ledgers whose evicted-key list could
	// not be read, so none of their state-archival evictions reached the
	// entry decoders — each evicted balance then stays served as live.
	evictedKeysUnreadable int

	// ledgerUpgradeEntries counts upgrade entries (protocol version, base
	// reserve, config settings, ...) seen in closed ledgers. No decoder
	// reads them; the count makes a protocol change visible.
	ledgerUpgradeEntries int

	// uncorroboratedCalls is the per-source count of calls an [ExecutionCorroborationRequirer] decoder matched but
	// the dispatcher dropped as only DECLARED in the auth tree. Non-zero on an oracle is a price-forgery attempt
	// or a routing-shape change (a relayer nesting relay() under a wrapper); either needs review.
	uncorroboratedCalls map[string]int
}

// New constructs a Dispatcher with the given Soroban-event
// decoders. Registration order determines first-match precedence
// — earlier wins. Classic-op decoders register via AddOpDecoder
// after construction and are not first-match (see AddOpDecoder).
func New(decoders ...Decoder) *Dispatcher {
	return &Dispatcher{
		decoders:            decoders,
		eventsSeen:          map[string]int{},
		decodeErrors:        map[string]int{},
		uncorroboratedCalls: map[string]int{},
	}
}

// AddOpDecoder registers a classic-operation decoder. Use for
// SDEX and any future non-Soroban path. Called once at startup;
// not safe concurrent with ProcessLedger. Every registered decoder
// whose Matches claims an op decodes it, in registration order.
func (d *Dispatcher) AddOpDecoder(od OpDecoder) {
	d.opDecoders = append(d.opDecoders, od)
}

// AddContractCallDecoder registers a decoder that observes Soroban
// InvokeContract calls directly (i.e. bypasses events). Required
// for sources that don't emit on-chain events — Band's Soroban
// StandardReference is the canonical case. Registration order
// determines first-match precedence.
func (d *Dispatcher) AddContractCallDecoder(ccd ContractCallDecoder) {
	d.contractCallDecoders = append(d.contractCallDecoders, ccd)
}

// AddEntryDecoder registers a LedgerEntryChange decoder (ADR-0021) once at startup, not concurrently with
// ProcessLedger; registration order is first-match precedence. Unmatched changes are not counted in
// unmatchedHits: every tx produces several, and they would dominate the metric.
func (d *Dispatcher) AddEntryDecoder(ld LedgerEntryChangeDecoder) {
	d.entryDecoders = append(d.entryDecoders, ld)
}

// AddDecoder registers a Soroban event Decoder after construction, with [New]'s first-match precedence; once
// at startup, not concurrently with ProcessLedger. It exists for the supply observers configured per asset in
// `[supply] watched_sep41_contracts`, which don't fit the per-source `cfg.Ingestion.EnabledSources`.
func (d *Dispatcher) AddDecoder(dec Decoder) {
	d.decoders = append(d.decoders, dec)
}

// DiscoverySink receives one Push per hit from [discovery.Sniff] (SEP-41 topic), [discovery.SniffOracleEvent]
// (oracle-suggestive topic) or [discovery.SniffOracleCall] (oracle-suggestive function name, the Band-alike
// case). Push MUST NOT block: it runs on the ingest hot path. The standard implementation buffers Hits to a
// worker calling discovery.Recorder.Record (internal/canonical/discovery); one sink serves all three.
type DiscoverySink interface {
	Push(hit discovery.Hit)
}

// SetDiscoverySink installs the sink the dispatcher calls on every
// watched-shape sighting (event-path SEP-41 / oracle-event, and
// call-path oracle-call — see [DiscoverySink]). Nil disables all
// three hooks. Not safe concurrent with ProcessLedger; called once
// at startup.
func (d *Dispatcher) SetDiscoverySink(sink DiscoverySink) {
	d.discoverySink = sink
}

// RawEventSink receives one PushEvent per Soroban contract event [dispatchOne] sees, BEFORE the decoder chain,
// so events a decoder rejects still land in soroban_events (ADR-0029). PushEvent MAY block: the standard
// implementation (internal/sources/sorobanevents) buffers to a worker calling
// [timescale.Store.InsertSorobanEventsBatch], and a full buffer slows dispatch so the per-ledger backfill
// cursor cannot outrun durable writes. Dropping on full is unsafe: a fill walk lost ~0.43% of rows across 8
// chunks with no recovery, as the cursor was already past them.
type RawEventSink interface {
	PushEvent(ev events.Event)
}

// SetRawEventSink installs the sink the dispatcher calls on every
// Soroban contract event. Nil disables the hook. Not safe
// concurrent with ProcessLedger; called once at startup. See
// ADR-0029 for the design rationale.
func (d *Dispatcher) SetRawEventSink(sink RawEventSink) {
	d.rawEventSink = sink
}

// SetLogger installs the logger the decoder-panic guard and the
// eviction-read failure write to. Nil leaves the fallback in place (slog.Default()), so a
// dispatcher built without one still reports a recovered panic — just
// without the binary/format the operator configured. Not safe
// concurrent with ProcessLedger; called once at startup.
func (d *Dispatcher) SetLogger(logger *slog.Logger) {
	d.logger = logger
}

// Stats is a snapshot of the dispatcher's internal counters. Keyed
// by source name for decode errors; includes an "unmatched" total
// for events no decoder claimed. Zero-copy read — caller should
// treat as immutable.
type Stats struct {
	// EventsSeen is the per-source count of inputs (events,
	// contract calls, entry changes, ops) that a decoder's Matches()
	// claimed. The denominator of "decoder error rate" the
	// decoder_stats_5m hypertable carries; without it, errors are an
	// uninterpretable count rather than a rate.
	EventsSeen   map[string]int
	DecodeErrors map[string]int
	OrphanEvents map[string]int
	// UnknownContractDrops is the per-source count of a fully decoded
	// event dropped because its contract (pair/pool) had no token
	// mapping in the decoder's registry — a lost trade, not a decode
	// error, but with no other signal. Collected the same
	// way as OrphanEvents: a duck-typed interface, since the drop
	// happens inside Decode with a nil error and the dispatcher's own
	// DecodeErrors counter never sees it.
	UnknownContractDrops map[string]int
	// NonDirectionalSwaps is the per-source count of a fully decoded
	// swap+sync pair recognized as non-directional (a single-side reserve
	// move, not a trade) — an expected, non-error class (ADR-0033), not
	// lost data like OrphanEvents/UnknownContractDrops. Collected the same
	// duck-typed way.
	NonDirectionalSwaps map[string]int
	UnmatchedHits       int
	// TxReadErrors counts malformed transactions skipped during
	// ProcessLedger. Operators reading the snapshot can spot a
	// sustained climb that would otherwise be invisible (the bad
	// tx is dropped and the ledger continues; without this counter
	// the only signal would be a downstream price gap days later).
	TxReadErrors int
	// TxEventReadErrors counts transactions whose GetTransactionEvents()
	// failed (e.g. an unsupported future TransactionMeta version) —
	// every such tx's Soroban events are dropped. A non-zero value means
	// Soroban ingestion is broken even if the completeness reconcile
	// still reads "complete".
	TxEventReadErrors int
	// EntryMetaUnsupported counts transactions whose apply-phase entry
	// change walk was skipped for an unhandled TransactionMeta version.
	// A sustained climb means the LedgerEntry supply observers are
	// blind while every component table simply stops advancing.
	EntryMetaUnsupported int
	// EvictedKeysUnreadable counts ledgers whose evicted-key list failed
	// to read; every eviction in them is missing from the served state.
	EvictedKeysUnreadable int
	// LedgerUpgradeEntries counts ledger-upgrade entries seen. They are
	// observed, not decoded: a non-zero delta marks a network-wide
	// parameter change (protocol, base reserve, Soroban config).
	LedgerUpgradeEntries int
	// UncorroboratedCalls is the per-source count of oracle-class
	// ContractCall invocations dropped before Decode because they were
	// only DECLARED in the auth tree, never executed. Non-zero on
	// an oracle source ("band") means a price-forgery attempt was
	// rejected OR the legitimate routing shape changed — either warrants
	// review, and both would be invisible without this counter.
	UncorroboratedCalls map[string]int
}

func (d *Dispatcher) Stats() Stats {
	// Snapshot the counter fields under statsMu so this read can't
	// race the dispatch goroutine's `++` mutations. Kept to
	// a tight copy of the maps + scalars; the orphan walk below calls
	// into decoder code and is deliberately OUTSIDE the lock (the
	// decoders slice is set once at startup, and holding statsMu
	// across decoder calls would needlessly widen the critical
	// section the dispatch path contends on).
	d.statsMu.Lock()
	seenCopied := make(map[string]int, len(d.eventsSeen))
	for k, v := range d.eventsSeen {
		seenCopied[k] = v
	}
	decodeCopied := make(map[string]int, len(d.decodeErrors))
	for k, v := range d.decodeErrors {
		decodeCopied[k] = v
	}
	uncorrCopied := make(map[string]int, len(d.uncorroboratedCalls))
	for k, v := range d.uncorroboratedCalls {
		uncorrCopied[k] = v
	}
	unmatched := d.unmatchedHits
	txReadErrs := d.txReadErrors
	txEventReadErrs := d.txEventReadErrors
	entryMetaUnsup := d.entryMetaUnsupported
	evictedUnreadable := d.evictedKeysUnreadable
	upgradeEntries := d.ledgerUpgradeEntries
	d.statsMu.Unlock()

	orphanCopied := map[string]int{}
	unknownContractCopied := map[string]int{}
	nonDirectionalCopied := map[string]int{}
	for _, dec := range d.decoders {
		if reporter, ok := dec.(interface{ EvictedOrphans() int }); ok {
			if n := reporter.EvictedOrphans(); n > 0 {
				orphanCopied[dec.Name()] = n
			}
		}
		if reporter, ok := dec.(interface{ UnknownContractDrops() int }); ok {
			if n := reporter.UnknownContractDrops(); n > 0 {
				unknownContractCopied[dec.Name()] = n
			}
		}
		if reporter, ok := dec.(interface{ SkippedNonDirectional() int }); ok {
			if n := reporter.SkippedNonDirectional(); n > 0 {
				nonDirectionalCopied[dec.Name()] = n
			}
		}
	}
	return Stats{
		EventsSeen:            seenCopied,
		DecodeErrors:          decodeCopied,
		OrphanEvents:          orphanCopied,
		UnknownContractDrops:  unknownContractCopied,
		NonDirectionalSwaps:   nonDirectionalCopied,
		UnmatchedHits:         unmatched,
		TxReadErrors:          txReadErrs,
		TxEventReadErrors:     txEventReadErrs,
		EntryMetaUnsupported:  entryMetaUnsup,
		EvictedKeysUnreadable: evictedUnreadable,
		LedgerUpgradeEntries:  upgradeEntries,
		UncorroboratedCalls:   uncorrCopied,
	}
}

// ProcessLedger walks lcm's transactions, routes each artefact to its decoder and returns all outputs; it
// blocks until done. passphrase must match the ledger's network (the SDK hashes txs with it). A bad LCM
// errors immediately; per-tx read errors bump Stats().TxReadErrors (logged at WARN by statsflush) and
// per-event decode errors are skipped, so the call succeeds with fewer outputs.
func (d *Dispatcher) ProcessLedger(lcm xdr.LedgerCloseMeta, passphrase string) ([]consumer.Event, error) { //nolint:gocognit,gocyclo,funlen // dispatch-heavy; splitting would reduce linearity
	reader, err := ingest.NewLedgerTransactionReaderFromLedgerCloseMeta(passphrase, lcm)
	if err != nil {
		return nil, fmt.Errorf("dispatcher: build reader for ledger %d: %w",
			lcm.LedgerSequence(), err)
	}
	defer func() { _ = reader.Close() }()

	ledgerSeq := lcm.LedgerSequence()
	// ClosedAt as RFC 3339 so the events.Event JSON shape matches
	// stellar-rpc's getEvents response exactly — decoders that parse
	// it (events.Event.EventClosedAt) work without transport-
	// awareness.
	closedAt := lcm.ClosedAt().UTC().Format(time.RFC3339)

	var outputs []consumer.Event
	parsedClosedAt := mustParseRFC3339(closedAt)

	// Read the whole ledger's transactions up front. The entry-change walk
	// needs every tx before it can emit anything, because on-chain the fee
	// phase for ALL txs precedes the apply phase for ALL txs — see
	// [Dispatcher.walkLedgerEntryChanges].
	// LedgerTransaction only holds slices into lcm, so retaining a ledger's
	// worth costs no extra decode.
	txs := make([]ingest.LedgerTransaction, 0, 64)
	for {
		tx, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Skip the transaction but keep going; one malformed tx
			// should not abort the whole ledger. Bump the counter so
			// `Stats().TxReadErrors` surfaces the drop — a silent skip
			// here would hide a slow corruption rate.
			d.statsMu.Lock()
			d.txReadErrors++
			d.statsMu.Unlock()
			continue
		}
		txs = append(txs, tx)
	}

	// ─── LedgerEntryChange walk (ADR-0021) ───────────────────────
	// Runs over the WHOLE ledger — including failed txs, whose fee
	// debits are committed on chain — and in the
	// chain's three-phase order, followed by the ledger's state-archival
	// evictions. Skipped cheaply when no entry decoders are registered.
	if len(d.entryDecoders) > 0 {
		outputs = append(outputs,
			d.walkLedgerEntryChanges(lcm, txs, ledgerSeq, parsedClosedAt)...)
	}
	// Outside the guard: upgrades are observed even with no entry decoders.
	d.noteLedgerUpgrades(lcm.UpgradesProcessing(), ledgerSeq)

	for i := range txs {
		tx := txs[i]
		if !tx.Result.Successful() {
			// Failed transactions don't produce real price signal.
			// stellar-extract/trades.go does the same. Their entry
			// changes ARE walked — above, before this loop.
			continue
		}

		txHash := hex.EncodeToString(tx.Result.TransactionHash[:])

		// ─── Soroban InvokeContract calls (once per tx) ──────
		// One walk, indexed by opIdx, feeds events.Event.OpArgs (Redstone; scoped by the provenance gate below) and
		// ContractCallDecoder routing (Band). Non-InvokeContract ops get a nil slot.
		invokeCalls := extractInvokeContractCalls(tx.Envelope.Operations())
		txSource, _ := accountIDToStrkey(tx.Envelope.SourceAccount().ToAccountId())
		ops := tx.Envelope.Operations()

		// ─── Soroban contract events ─────────────────────────
		// We process per-OPERATION events only. Tx-level events
		// (txEvents.TransactionEvents — CAP-67 V4 fee/diagnostic events)
		// are intentionally OUT OF SCOPE: they carry no price/supply
		// signal, and the census (census.go) makes the identical choice
		// so the ADR-0033 reconcile stays consistent. If that ever
		// changes, change BOTH sites together.
		txEvents, terr := tx.GetTransactionEvents()
		switch {
		case terr != nil:
			// An unsupported future TransactionMeta version makes
			// this fail for every tx, silently dropping all Soroban
			// events. Count it so the break is visible instead of
			// masquerading as a clean (empty) ledger.
			d.statsMu.Lock()
			d.txEventReadErrors++
			d.statsMu.Unlock()
		case len(txEvents.OperationEvents) > 0:
			needsWrites := d.stateWriteContracts()
			for opIdx, opEvents := range txEvents.OperationEvents {
				var call *invokeCall
				if opIdx < len(invokeCalls) {
					call = invokeCalls[opIdx]
				}
				// State-write enrichment: the contract-data entries whose VALUE this op changed, filtered per event to its
				// own contract (Redstone's subset attribution; state_write_keys.go). Resolved lazily, once per op, only for
				// events of a [StateWriteKeyConsumer]'s contracts: the meta walk is pure overhead otherwise.
				var opWrites []contractDataWrite
				opWritesResolved := false
				for evIdx, ce := range opEvents {
					ev := contractEventToEventsEvent(ce, ledgerSeq, txHash, opIdx, evIdx, closedAt, nil)
					if ev == nil {
						continue
					}
					// ─── OpArgs provenance gate ──────────────────
					// The top-level args belong to the invoked contract only, so attach them only to its own events. For a
					// sub-invoked contract they are attacker-chosen: a wrapper could call adapter.write_prices with the real
					// payload while steering redstone's feed_ids attribution via its own args. An args-requiring decoder then
					// refuses (redstone: ErrMissingOpArgs, counted). The lake extractor applies the same rule
					// (clickhouse/extract.go opArgsByIndex).
					if call != nil && call.ContractID == ev.ContractID {
						ev.OpArgs = call.Args
					}
					if needsWrites[ev.ContractID] {
						if !opWritesResolved {
							opWrites = opContractDataWriteKeys(tx, opIdx)
							opWritesResolved = true
						}
						ev.StateWriteKeys = stateWriteKeysFor(opWrites, ev.ContractID)
					}
					outs, err := d.dispatchOne(*ev)
					if err != nil {
						continue
					}
					outputs = append(outputs, outs...)
				}
			}
		}

		// ─── Soroban InvokeContract call routing ─────────────
		// Walks each op's FULL auth tree, not just the top level: most Soroswap traffic reaches the router as a
		// sub-invocation of an aggregator, and a top-level-only walk misses ~99.99% of router calls
		// (docs/adr/0052-contract-call-tree-routing.md). Decoders match on (contract_id, function_name, args) and
		// the emitted CallPath records the node's position.
		if d.contractCallPathActive() {
			callTrees := extractInvokeContractCallTrees(ops)
			for opIdx, calls := range callTrees {
				if len(calls) == 0 {
					continue
				}
				opSource := ""
				if opIdx < len(ops) && ops[opIdx].SourceAccount != nil {
					opSource, _ = accountIDToStrkey(ops[opIdx].SourceAccount.ToAccountId())
				}
				// The op's top-level EXECUTED InvokeContract call (nil for other ops or an unrenderable address). A routed
				// call is execution-corroborated iff it IS this call; everything else came from the submitter-supplied auth
				// tree and may never have executed.
				var topCall *invokeCall
				if opIdx < len(invokeCalls) {
					topCall = invokeCalls[opIdx]
				}
				for _, call := range calls {
					corroborated := executionCorroborated(topCall, call)
					ccCtx := ContractCallContext{
						Ledger:                ledgerSeq,
						ClosedAt:              parsedClosedAt,
						TxHash:                txHash,
						TxSource:              txSource,
						OpSource:              opSource,
						OpIndex:               opIdx,
						CallPath:              call.CallPath,
						CallPathContracts:     call.CallPathContracts,
						AuthOccurrence:        call.AuthOccurrence,
						ContractID:            call.ContractID,
						FunctionName:          call.FunctionName,
						Args:                  call.Args,
						ExecutionCorroborated: corroborated,
					}
					outs, err := d.dispatchContractCall(ccCtx)
					if err != nil {
						continue
					}
					outputs = append(outputs, outs...)
				}
			}
		}

		// ─── Classic operations (SDEX and friends) ───────────
		if len(d.opDecoders) == 0 {
			continue // skip op walking if no classic decoders registered
		}
		opResults, haveResults := tx.Result.Result.OperationResults()
		if !haveResults {
			continue
		}

		for opIdx, op := range ops {
			if opIdx >= len(opResults) {
				break
			}
			opSource := ""
			if op.SourceAccount != nil {
				opSource, _ = accountIDToStrkey(op.SourceAccount.ToAccountId())
			}
			opCtx := OpContext{
				Ledger:   ledgerSeq,
				ClosedAt: parsedClosedAt,
				TxHash:   txHash,
				TxSource: txSource,
				OpSource: opSource,
				OpIndex:  opIdx,
				Op:       op,
				OpResult: opResults[opIdx],
			}
			outs, _ := d.dispatchOp(opCtx)
			outputs = append(outputs, outs...)
		}
	}
	return outputs, nil
}

// walkLedgerEntryChanges dispatches EVERY LedgerEntryChange in the ledger to the entry-decoder chain, in the
// order stellar-core committed them, and returns the outputs. Two balance-observation requirements:
//
//  1. FAILED TXS ARE INCLUDED: core commits their fee debit. Skipping them over-reports the balance and
//     diverges from the lake's clickhouse.extractEntryChanges (ADR-0034 re-derive needs the two to agree).
//  2. THE WALK IS LEDGER-WIDE AND PHASED, as the SDK's LedgerChangeReader: a per-tx walk would rank tx2's fee
//     after tx1's ops on a shared account and publish a fee-phase balance as final; omitting phase 3 does
//     the same on P23+. We don't suppress txInternalError() changes at LedgerVersion <= 12 (inert in V4).
//
// Phases: 1 every tx's FeeChanges (core charges all fees first); 2 every tx's apply-phase meta; 3 every tx's
// PostTxApplyFeeChanges (P23 Soroban refunds; LCM V2 only); 4 the ledger's EVICTED keys as synthetic
// Removed changes ([walkEvictedKeys]). Ledger upgrades are not walked (no TxHash, no consumer; the lake
// walker agrees); [ProcessLedger] counts and logs them ([noteLedgerUpgrades]).
//
// IntraLedgerSeq advances for every walked change; positions are scoped to [EntryWalkVersion]. Only V3/V4
// meta is handled: galexie regenerates meta at replay (48 sampled ledgers across protocols 1→19 were all
// V4). Anything else is COUNTED in the default arm, else entry observation would stop silently.
func (d *Dispatcher) walkLedgerEntryChanges(lcm xdr.LedgerCloseMeta, txs []ingest.LedgerTransaction, ledgerSeq uint32, closedAt time.Time) []consumer.Event {
	var seq uint32
	dispatchFor := func(txHash string) func(int, xdr.LedgerEntryChange) []consumer.Event {
		return func(opIdx int, change xdr.LedgerEntryChange) []consumer.Event {
			ctx := LedgerEntryChangeContext{
				Ledger:         ledgerSeq,
				ClosedAt:       closedAt,
				TxHash:         txHash,
				OpIndex:        opIdx,
				IntraLedgerSeq: seq,
				Change:         change,
			}
			seq++
			outs, err := d.dispatchEntryChange(ctx)
			if err != nil {
				return nil
			}
			return outs
		}
	}

	var outputs []consumer.Event
	// ── Phase 1: the fee phase for every tx, in tx-set apply order.
	// OpIndex is -1 to distinguish from per-op changes.
	for i := range txs {
		dispatch := dispatchFor(entryChangeTxHash(&txs[i]))
		outputs = append(outputs, walkChangeSet(txs[i].FeeChanges, -1, dispatch)...)
	}
	// ── Phase 2: the apply phase for every tx, in the same order.
	for i := range txs {
		dispatch := dispatchFor(entryChangeTxHash(&txs[i]))
		switch txs[i].UnsafeMeta.V {
		case 3:
			v3 := txs[i].UnsafeMeta.MustV3()
			outputs = append(outputs, walkChangeSet(v3.TxChangesBefore, -1, dispatch)...)
			outputs = append(outputs, walkOperations(v3.Operations, dispatch)...)
			outputs = append(outputs, walkChangeSet(v3.TxChangesAfter, -1, dispatch)...)
		case 4:
			v4 := txs[i].UnsafeMeta.MustV4()
			outputs = append(outputs, walkChangeSet(v4.TxChangesBefore, -1, dispatch)...)
			outputs = append(outputs, walkV4Operations(v4.Operations, dispatch)...)
			outputs = append(outputs, walkChangeSet(v4.TxChangesAfter, -1, dispatch)...)
		default:
			// Unreachable on production input (see the meta-version note
			// above), and deliberately not an error: the fee phase for
			// this tx already walked and phase 3 still will, so failing
			// here would discard real observations. But it must not be
			// SILENT — an unwalked apply phase looks exactly like a
			// ledger in which nothing happened.
			d.statsMu.Lock()
			d.entryMetaUnsupported++
			d.statsMu.Unlock()
		}
	}
	// ── Phase 3: the post-apply fee phase (P23 Soroban fee refunds) for
	// every tx, in the same order. Empty on pre-P23 ledgers — the SDK only
	// populates PostTxApplyFeeChanges from LCM V2. op_index -1: it is a
	// tx-level change, like the fee phase it mirrors.
	for i := range txs {
		dispatch := dispatchFor(entryChangeTxHash(&txs[i]))
		outputs = append(outputs, walkChangeSet(txs[i].PostTxApplyFeeChanges, -1, dispatch)...)
	}
	// ── Phase 4: the ledger's STATE-ARCHIVAL EVICTIONS. Not
	// transaction-scoped, so empty TxHash and OpIndex -1 like the fee
	// blocks, and last in the walk because core evicts at ledger close,
	// after every transaction has applied. See [walkEvictedKeys].
	outputs = append(outputs, d.walkEvictedKeys(lcm, ledgerSeq, dispatchFor(""))...)
	return outputs
}

// noteLedgerUpgrades counts and logs a ledger's upgrade entries. It never
// dispatches them: no decoder consumes upgrade changes.
func (d *Dispatcher) noteLedgerUpgrades(ups []xdr.UpgradeEntryMeta, ledgerSeq uint32) {
	if len(ups) == 0 {
		return
	}
	types := make([]string, len(ups))
	for i := range ups {
		types[i] = ups[i].Upgrade.Type.String()
	}
	d.statsMu.Lock()
	d.ledgerUpgradeEntries += len(ups)
	d.statsMu.Unlock()
	d.log().Info("dispatcher: ledger carries upgrades", "ledger", ledgerSeq, "types", types)
}

// walkEvictedKeys dispatches one synthetic Removed change per ledger key core EVICTED at close (CAP-62), the
// missing half of state archival. An expired entry appears in no tx meta; unwalked, an evicted SAC balance
// would stand forever and served supply drift ABOVE the truth. Removed is absorbing (re-ingest rewrites the
// same row) and a later Restored reverses it; unwatched key types fall out at Matches.
//
// The lake walker gives evicted keys the same positions but writes `removed` only for deleted entries
// (entry_walk_parity_test.go pins them together). Unreadable keys are counted and logged with the ledger
// number, since each dropped eviction overstates a balance and that ledger needs a replay.
func (d *Dispatcher) walkEvictedKeys(lcm evictedKeysSource, ledgerSeq uint32, dispatch func(int, xdr.LedgerEntryChange) []consumer.Event) []consumer.Event {
	keys, err := lcm.EvictedLedgerKeys()
	if err != nil {
		d.statsMu.Lock()
		d.evictedKeysUnreadable++
		d.statsMu.Unlock()
		d.log().Warn("dispatcher: evicted ledger keys unreadable — this ledger's state-archival evictions skipped",
			"ledger", ledgerSeq, "err", err)
		return nil
	}
	var outs []consumer.Event
	for i := range keys {
		outs = append(outs, dispatch(-1, xdr.LedgerEntryChange{
			Type:    xdr.LedgerEntryChangeTypeLedgerEntryRemoved,
			Removed: &keys[i],
		})...)
	}
	return outs
}

// evictedKeysSource is the slice of xdr.LedgerCloseMeta the eviction phase
// reads; a test can stand in a source whose read fails.
type evictedKeysSource interface {
	EvictedLedgerKeys() ([]xdr.LedgerKey, error)
}

// entryChangeTxHash is the hex tx hash used to stamp entry-change contexts.
// Kept separate from the ProcessLedger-local encoding so the walk computes
// it identically in every phase.
func entryChangeTxHash(tx *ingest.LedgerTransaction) string {
	return hex.EncodeToString(tx.Result.TransactionHash[:])
}

// walkChangeSet dispatches each LedgerEntryChange in the slice
// at the given opIndex (-1 for tx-level / fee-meta blocks), in
// entrywalk.Canonical order so IntraLedgerSeq does not depend on which
// export the ledger was read from.
func walkChangeSet(changes []xdr.LedgerEntryChange, opIdx int, dispatch func(int, xdr.LedgerEntryChange) []consumer.Event) []consumer.Event {
	var outs []consumer.Event
	changes = entrywalk.Canonical(changes)
	for i := range changes {
		outs = append(outs, dispatch(opIdx, changes[i])...)
	}
	return outs
}

func walkOperations(ops []xdr.OperationMeta, dispatch func(int, xdr.LedgerEntryChange) []consumer.Event) []consumer.Event {
	var outs []consumer.Event
	for opIdx := range ops {
		outs = append(outs, walkChangeSet(ops[opIdx].Changes, opIdx, dispatch)...)
	}
	return outs
}

func walkV4Operations(ops []xdr.OperationMetaV2, dispatch func(int, xdr.LedgerEntryChange) []consumer.Event) []consumer.Event {
	var outs []consumer.Event
	for opIdx := range ops {
		outs = append(outs, walkChangeSet(ops[opIdx].Changes, opIdx, dispatch)...)
	}
	return outs
}

// bumpEventsSeen increments events_seen under statsMu, pre-Decode, on all four seams; the decoder runs
// lock-free. It lazily initialises the map because the decoder-panic guard calls it too, and a nil-map write
// inside recover would turn a contained fault back into a crash.
func (d *Dispatcher) bumpEventsSeen(name string) {
	d.statsMu.Lock()
	if d.eventsSeen == nil {
		d.eventsSeen = map[string]int{}
	}
	d.eventsSeen[name]++
	d.statsMu.Unlock()
}

// bumpDecodeError increments the per-source decode-error counter
// under statsMu. Called when a matched decoder's Decode
// returns an error — or panics; see bumpEventsSeen for why
// the map is lazily initialised.
func (d *Dispatcher) bumpDecodeError(name string) {
	d.statsMu.Lock()
	if d.decodeErrors == nil {
		d.decodeErrors = map[string]int{}
	}
	d.decodeErrors[name]++
	d.statsMu.Unlock()
}

// bumpUnmatched increments the unmatched-events counter under
// statsMu.
func (d *Dispatcher) bumpUnmatched() {
	d.statsMu.Lock()
	d.unmatchedHits++
	d.statsMu.Unlock()
}

// bumpUncorroborated increments the per-source count of oracle-class
// ContractCall invocations dropped for lack of execution corroboration
// under statsMu. Lazily initialises the map so a
// Dispatcher built without New() still counts rather than panicking.
func (d *Dispatcher) bumpUncorroborated(name string) {
	d.statsMu.Lock()
	if d.uncorroboratedCalls == nil {
		d.uncorroboratedCalls = map[string]int{}
	}
	d.uncorroboratedCalls[name]++
	d.statsMu.Unlock()
}

// contractCallPathActive reports whether ProcessLedger walks the auth trees at all: true when a
// ContractCallDecoder is registered OR a discovery sink is installed, since the oracle-call discovery hook
// (docs/architecture/oracle-manipulation-defense.md §"Event-less discovery") runs only inside that walk and
// must not depend on Band being registered. Neither means zero per-op overhead.
func (d *Dispatcher) contractCallPathActive() bool {
	return len(d.contractCallDecoders) > 0 || d.discoverySink != nil
}

// dispatchContractCall runs one call through the contract-call decoder chain; first match owns it. Every call
// first goes through [discovery.SniffOracleCall] (a map lookup on FunctionName), the event-less twin of
// dispatchOne's discovery: Band-alikes publish no event, so a topic sniffer never sees them. It runs whether
// or not a decoder matches (docs/architecture/oracle-manipulation-defense.md §"Event-less discovery").
func (d *Dispatcher) dispatchContractCall(ctx ContractCallContext) (outs []consumer.Event, err error) {
	if d.discoverySink != nil {
		if hit, ok := discovery.SniffOracleCall(discovery.OracleCallInput{
			ContractID:        ctx.ContractID,
			FunctionName:      ctx.FunctionName,
			Ledger:            ctx.Ledger,
			ObservedAtRFC3339: ctx.ClosedAt.UTC().Format(time.RFC3339),
		}); ok {
			d.discoverySink.Push(hit)
		}
	}
	// Decoder-panic guard — installed AFTER the discovery
	// hook so it covers Matches+Decode and nothing else. See
	// recordDecoderPanic for why a panic becomes this seam's ordinary
	// decode error.
	var (
		current string
		seen    bool
	)
	defer func() {
		if r := recover(); r != nil {
			outs, err = nil, d.recordDecoderPanic(current, seen, r,
				panicSite{Ledger: ctx.Ledger, TxHash: ctx.TxHash, OpIndex: ctx.OpIndex})
		}
	}()
	for _, ccd := range d.contractCallDecoders {
		current, seen = ccd.Name(), false
		if !ccd.Matches(ctx.ContractID, ctx.FunctionName) {
			continue
		}
		// Execution-corroboration gate: refuse a call the auth tree merely DECLARED, before Decode reads its args
		// as a price, and count it so forgery (or a routing-shape change) is visible.
		if RefusesUncorroborated(ccd, ctx.ExecutionCorroborated) {
			d.bumpUncorroborated(ccd.Name())
			return nil, nil
		}
		d.bumpEventsSeen(ccd.Name())
		seen = true
		got, derr := ccd.Decode(ctx)
		if derr != nil {
			d.bumpDecodeError(ccd.Name())
			return nil, derr
		}
		return got, nil
	}
	return nil, nil
}

// RouteContractCall is the test-harness entry point for
// contract-call decoders, symmetric with Route / RouteOp.
func (d *Dispatcher) RouteContractCall(ctx ContractCallContext) ([]consumer.Event, error) {
	return d.dispatchContractCall(ctx)
}

// dispatchEntryChange runs one [xdr.LedgerEntryChange] through the
// entry-decoder chain. First matching decoder owns it. Mismatches
// are silently dropped — entry changes are high-volume (every
// successful tx produces several) so an unmatched-counter would
// dominate the metric.
func (d *Dispatcher) dispatchEntryChange(ctx LedgerEntryChangeContext) (outs []consumer.Event, err error) {
	// Decoder-panic guard — see recordDecoderPanic.
	var (
		current string
		seen    bool
	)
	defer func() {
		if r := recover(); r != nil {
			outs, err = nil, d.recordDecoderPanic(current, seen, r,
				panicSite{Ledger: ctx.Ledger, TxHash: ctx.TxHash, OpIndex: ctx.OpIndex})
		}
	}()
	for _, ld := range d.entryDecoders {
		current, seen = ld.Name(), false
		if !ld.Matches(ctx.Change) {
			continue
		}
		d.bumpEventsSeen(ld.Name())
		seen = true
		got, derr := ld.Decode(ctx)
		if derr != nil {
			d.bumpDecodeError(ld.Name())
			return nil, derr
		}
		return got, nil
	}
	return nil, nil
}

// RouteEntryChange is the test-harness entry point for
// LedgerEntryChange decoders, symmetric with Route / RouteOp /
// RouteContractCall. Per ADR-0021.
func (d *Dispatcher) RouteEntryChange(ctx LedgerEntryChangeContext) ([]consumer.Event, error) {
	return d.dispatchEntryChange(ctx)
}

// dispatchOp offers one operation to EVERY registered op decoder.
// Op decoders are per-domain observers (sdex trades, classic
// movements), and their op-type sets overlap on path payments, so
// first-match routing would silently drop one domain's rows. A
// decoder's error or panic costs only its own output for this op: it is
// counted per decoder and returned joined, beside the others' outputs.
func (d *Dispatcher) dispatchOp(ctx OpContext) ([]consumer.Event, error) {
	var (
		outs []consumer.Event
		errs []error
	)
	for _, od := range d.opDecoders {
		got, err := d.decodeOp(od, ctx)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		outs = append(outs, got...)
	}
	return outs, errors.Join(errs...)
}

// decodeOp runs one op decoder on one op, behind its own panic guard
// (see recordDecoderPanic) so a fault is confined to it.
func (d *Dispatcher) decodeOp(od OpDecoder, ctx OpContext) (outs []consumer.Event, err error) {
	var (
		name string
		seen bool
	)
	defer func() {
		if r := recover(); r != nil {
			outs, err = nil, d.recordDecoderPanic(name, seen, r,
				panicSite{Ledger: ctx.Ledger, TxHash: ctx.TxHash, OpIndex: ctx.OpIndex})
		}
	}()
	name = od.Name()
	if !od.Matches(ctx.Op) {
		return nil, nil
	}
	d.bumpEventsSeen(name)
	seen = true
	got, derr := od.Decode(ctx)
	if derr != nil {
		d.bumpDecodeError(name)
		return nil, derr
	}
	return got, nil
}

// RouteOp is the test-harness entry point for classic-op
// decoders, symmetric with Route for Soroban events.
func (d *Dispatcher) RouteOp(ctx OpContext) ([]consumer.Event, error) {
	return d.dispatchOp(ctx)
}

// Route feeds one event through the Matches/Decode chain for test harnesses and fixture replay. It errors only
// when the matching decoder fails; unclaimed events count in Stats().UnmatchedHits and return (nil, nil).
func (d *Dispatcher) Route(ev events.Event) ([]consumer.Event, error) {
	return d.dispatchOne(ev)
}

// dispatchOne runs one event through the Matches/Decode chain; it errors only when the matching decoder
// fails, and unclaimed events are counted and dropped. Before decoding, the event goes to [discovery.Sniff]
// and [discovery.SniffOracleEvent] (disjoint symbol sets; docs/architecture/oracle-manipulation-defense.md
// §"Event-shaped discovery") and to the [RawEventSink] (ADR-0029), so events a decoder rejects still reach
// discovered_assets and soroban_events.
func (d *Dispatcher) dispatchOne(ev events.Event) (outs []consumer.Event, err error) {
	if d.discoverySink != nil {
		if hit, ok := discovery.Sniff(ev); ok {
			d.discoverySink.Push(hit)
		}
		if hit, ok := discovery.SniffOracleEvent(ev); ok {
			d.discoverySink.Push(hit)
		}
	}
	// Raw-event capture (ADR-0029) — runs BEFORE per-source decoders
	// so even events a decoder later rejects (malformed body, etc.)
	// still land in soroban_events. PushEvent applies back-pressure
	// when the sink's buffer is full: the dispatcher slows down to
	// match storage throughput so the producer's cursor never
	// outruns durable writes.
	if d.rawEventSink != nil {
		d.rawEventSink.PushEvent(ev)
	}
	// Decoder-panic guard — installed AFTER the discovery and
	// raw-event hooks so it covers Matches+Decode and nothing else: a
	// fault in the lake sink is NOT a decoder bug and must keep its
	// existing (ledger-level) handling. See recordDecoderPanic.
	var (
		current string
		seen    bool
	)
	defer func() {
		if r := recover(); r != nil {
			outs, err = nil, d.recordDecoderPanic(current, seen, r,
				panicSite{Ledger: ev.Ledger, TxHash: ev.TxHash, OpIndex: ev.OperationIndex})
		}
	}()
	for _, dec := range d.decoders {
		current, seen = dec.Name(), false
		if !dec.Matches(ev) {
			continue
		}
		d.bumpEventsSeen(dec.Name())
		seen = true
		got, derr := dec.Decode(ev)
		if derr != nil {
			d.bumpDecodeError(dec.Name())
			return nil, derr
		}
		return got, nil
	}
	d.bumpUnmatched()
	return nil, nil
}

// contractEventToEventsEvent flattens an xdr.ContractEvent into events.Event, returning nil for
// non-contract (diagnostic) events. ProcessLedger passes nil opArgs and attaches them after conversion, once
// the provenance gate knows the emitter; the parameter serves callers that already established provenance.
// evIdx becomes EventIndex, keeping (ledger, tx_hash, op_index, event_index) unique per event (ADR-0033).
func contractEventToEventsEvent(ce xdr.ContractEvent, ledgerSeq uint32, txHash string, opIdx, evIdx int, closedAt string, opArgs []string) *events.Event {
	if ce.Type != xdr.ContractEventTypeContract {
		return nil
	}
	if ce.ContractId == nil {
		return nil
	}
	// Topic + Value are ScVals — base64 them so the events.Event
	// format matches stellar-rpc getEvents output byte-for-byte.
	// Decoders byte-equality-match on these, so any deviation from
	// the RPC shape would silently break topic routing.
	body := ce.Body
	if body.V != 0 {
		// Only V=0 is currently defined; anything else is a protocol
		// bump we haven't audited.
		return nil
	}
	v0, ok := body.GetV0()
	if !ok {
		return nil
	}

	topic := make([]string, 0, len(v0.Topics))
	for i := range v0.Topics {
		raw, err := v0.Topics[i].MarshalBinary()
		if err != nil {
			return nil
		}
		topic = append(topic, base64.StdEncoding.EncodeToString(raw))
	}
	rawVal, err := v0.Data.MarshalBinary()
	if err != nil {
		return nil
	}

	contractID, err := contractIDToStrkey(*ce.ContractId)
	if err != nil {
		return nil
	}

	return &events.Event{
		Type:                     "contract",
		Ledger:                   ledgerSeq,
		LedgerClosedAt:           closedAt,
		ContractID:               contractID,
		OperationIndex:           opIdx,
		EventIndex:               evIdx,
		TxHash:                   txHash,
		InSuccessfulContractCall: true,
		Topic:                    topic,
		Value:                    base64.StdEncoding.EncodeToString(rawVal),
		OpArgs:                   opArgs,
	}
}

// invokeCall is one Soroban InvokeContract call: C-strkey contract, raw Symbol function, base64 SCVal args
// (the events.Event.OpArgs format). CallPath is its auth-tree position: empty is top-level, else each int
// indexes the parent's SubInvocations ([0,1] = second sub of the first sub), used to dedup or tag
// attribution (docs/adr/0052-contract-call-tree-routing.md).
type invokeCall struct {
	ContractID   string
	FunctionName string
	Args         []string
	CallPath     []int // empty for top-level; pre-order path for sub-invocations
	// CallPathContracts is the ordered ancestor+self contract chain —
	// see ContractCallContext.CallPathContracts for the full contract.
	// Always length >= 1 (this call's own ContractID is always the
	// last element) when non-nil.
	CallPathContracts []string
	// AuthOccurrence — see ContractCallContext.AuthOccurrence.
	AuthOccurrence int
}

// buildInvokeCallFromArgs projects an [xdr.InvokeContractArgs] (the top-level HostFunction or an auth-tree
// node) into an [invokeCall], or nil if the address won't encode as a C-strkey. path is copied, so callers
// may reuse it. ancestorChain is the ancestors ABOVE this node (outermost first); this call's contract is
// appended, so pass the slice as received, not already extended.
func buildInvokeCallFromArgs(ic *xdr.InvokeContractArgs, path []int, ancestorChain []string) *invokeCall {
	contractStrkey := ""
	switch ic.ContractAddress.Type {
	case xdr.ScAddressTypeScAddressTypeContract:
		cid := ic.ContractAddress.MustContractId()
		if s, err := contractIDToStrkey(cid); err == nil {
			contractStrkey = s
		}
	case xdr.ScAddressTypeScAddressTypeAccount:
		// InvokeContract against an account address is invalid at
		// the protocol level. Defensive skip.
		return nil
	}
	if contractStrkey == "" {
		return nil
	}
	args := make([]string, 0, len(ic.Args))
	argsOK := true
	for j := range ic.Args {
		raw, err := ic.Args[j].MarshalBinary()
		if err != nil {
			argsOK = false
			break
		}
		args = append(args, base64.StdEncoding.EncodeToString(raw))
	}
	if !argsOK {
		args = nil
	}
	var copyPath []int
	if len(path) > 0 {
		copyPath = make([]int, len(path))
		copy(copyPath, path)
	}
	chain := make([]string, len(ancestorChain)+1)
	copy(chain, ancestorChain)
	chain[len(ancestorChain)] = contractStrkey
	return &invokeCall{
		ContractID:        contractStrkey,
		FunctionName:      string(ic.FunctionName),
		Args:              args,
		CallPath:          copyPath,
		CallPathContracts: chain,
	}
}

// walkAuthTree appends every InvokeContract call reachable from node (itself, then descendants, pre-order
// DFS) to out, extending path with each child index and, for ContractFn nodes, ancestorChain with the node's
// contract (ContractCallContext.CallPathContracts). CreateContract / CreateContractV2 nodes have no function
// to match, so they emit nothing and leave ancestorChain unchanged, but their SubInvocations are walked.
func walkAuthTree(node *xdr.SorobanAuthorizedInvocation, path []int, ancestorChain []string, out *[]*invokeCall) {
	childChain := ancestorChain
	if node.Function.Type == xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeContractFn {
		ic := node.Function.MustContractFn()
		if call := buildInvokeCallFromArgs(&ic, path, ancestorChain); call != nil {
			*out = append(*out, call)
			childChain = call.CallPathContracts
		}
	}
	for i := range node.SubInvocations {
		childPath := make([]int, len(path)+1)
		copy(childPath, path)
		childPath[len(path)] = i
		walkAuthTree(&node.SubInvocations[i], childPath, childChain, out)
	}
}

// authRootCall builds the invokeCall for an auth entry's root node, or
// nil when that root is a create-contract (non-ContractFn) authorization.
// Path and ancestor chain are deliberately nil: the result is used only
// for identity comparison against the top-level call, never emitted.
func authRootCall(node *xdr.SorobanAuthorizedInvocation) *invokeCall {
	if node.Function.Type != xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeContractFn {
		return nil
	}
	ic := node.Function.MustContractFn()
	return buildInvokeCallFromArgs(&ic, nil, nil)
}

// walkAuthEntries flattens an op's auth entries, classifying EACH independently as the op's top-level call
// or a nested call to re-root under it. An all-or-nothing re-root breaks a co-signed tx whose entry 0 is the
// top-level call and entry 1 a deeper call: entries 1..n would export as the ENTRY POINT (call_kind
// 'top_level' in soroswap_router/decode.go callPosition), so TagTradesRoutedVia's 'sub_invocation' join
// would under-report the wrapper on /v1/aggregators, and two calls could share the
// (TxHash, OpIndex, CallPath) dedup key.
func walkAuthEntries(auth []xdr.SorobanAuthorizationEntry, top *invokeCall) []*invokeCall {
	var calls []*invokeCall

	topIdx := -1
	if top != nil {
		for j := range auth {
			if root := authRootCall(&auth[j].RootInvocation); root != nil &&
				containsCall([]*invokeCall{top}, root) {
				topIdx = j
				break
			}
		}
	}

	if topIdx < 0 {
		for j := range auth {
			if top == nil {
				// No top-level call to root under (create-contract host
				// function, or an unrenderable contract address). Walk
				// the entries as their own roots.
				walkAuthEntry(&auth[j].RootInvocation, nil, nil, &calls)
				continue
			}
			// The auth roots are NESTED calls: the top-level call needed
			// no auth of its own. Re-root them under it so nothing but
			// the real root carries CallPath [].
			walkAuthEntry(&auth[j].RootInvocation, []int{j}, top.CallPathContracts, &calls)
		}
		if top != nil {
			calls = append([]*invokeCall{top}, calls...)
		}
		return calls
	}

	// One entry IS the top-level call: walk it as the root so it and its
	// authorized subtree carry their true paths.
	walkAuthEntry(&auth[topIdx].RootInvocation, nil, nil, &calls)

	// Every other entry is a separately-authorized subtree whose real
	// sub-invocation index the auth tree does not carry. Re-root them
	// under the top-level call at indices PAST its own sub-invocation
	// space, so they cannot collide with a genuine sibling path while
	// still reading as depth>=1 rather than as the entry point.
	next := len(auth[topIdx].RootInvocation.SubInvocations)
	for j := range auth {
		if j == topIdx {
			continue
		}
		walkAuthEntry(&auth[j].RootInvocation, []int{next}, top.CallPathContracts, &calls)
		next++
	}
	return calls
}

// walkAuthEntry walks one auth entry's tree onto out and numbers each
// call's AuthOccurrence among that entry's calls only.
func walkAuthEntry(root *xdr.SorobanAuthorizedInvocation, path []int, ancestorChain []string, out *[]*invokeCall) {
	start := len(*out)
	walkAuthTree(root, path, ancestorChain, out)
	entry := (*out)[start:]
	for i, c := range entry {
		for _, prev := range entry[:i] {
			if sameInvocation(prev, c) {
				c.AuthOccurrence++
			}
		}
	}
}

// containsCall reports whether calls already holds the same invocation as
// c — same contract, same function, same argument encoding. Identity is
// deliberately NOT CallPath: the question it answers is "did the auth walk
// already emit the top-level call under some path", and emitting it twice
// would double every downstream decode.
func containsCall(calls []*invokeCall, c *invokeCall) bool {
	for _, existing := range calls {
		if sameInvocation(existing, c) {
			return true
		}
	}
	return false
}

// sameInvocation reports whether a and b invoke the same contract and
// function with the same argument encoding, regardless of tree position.
func sameInvocation(a, b *invokeCall) bool {
	if a.ContractID != b.ContractID || a.FunctionName != b.FunctionName || len(a.Args) != len(b.Args) {
		return false
	}
	for i := range a.Args {
		if a.Args[i] != b.Args[i] {
			return false
		}
	}
	return true
}

// extractInvokeContractCallTrees returns, parallel to ops, every call reachable from each op: the top-level
// invocation plus every nested call in its auth tree (nil for other ops). It is the ContractCallDecoder
// routing source (ADR-0052); [extractInvokeContractCalls] is top-level only, for OpArgs. The auth tree holds
// every call needing authorization (every token transfer in a DEX flow) and mirrors the call tree.
//
// The top-level call is always emitted with CallPath == []. An auth entry's root is the subtree NEEDING
// authorization, often a nested call, so when no walked call matches the top level it is prepended and
// entry j is re-rooted at [j]: the auth-entry ordinal, not the host's sub-call ordinal, but stable across
// re-derives and not claiming to be the root. Dedup on (contract, function, args) keeps a top-level call that
// is its own auth root from emitting twice (a duplicate trade row). Cross-entry duplicates are left to
// consumers; AuthOccurrence tells a repeated execution in one entry from a re-listing by another.
func extractInvokeContractCallTrees(ops []xdr.Operation) [][]*invokeCall { //nolint:gocognit // dispatch-heavy; splitting would reduce linearity
	if len(ops) == 0 {
		return nil
	}
	out := make([][]*invokeCall, len(ops))
	for i, op := range ops {
		if op.Body.Type != xdr.OperationTypeInvokeHostFunction {
			continue
		}
		ihf, ok := op.Body.GetInvokeHostFunctionOp()
		if !ok {
			continue
		}
		if ihf.HostFunction.Type != xdr.HostFunctionTypeHostFunctionTypeInvokeContract {
			continue
		}

		var calls []*invokeCall
		topIC := ihf.HostFunction.MustInvokeContract()
		top := buildInvokeCallFromArgs(&topIC, nil, nil)

		if len(ihf.Auth) > 0 {
			calls = walkAuthEntries(ihf.Auth, top)
		} else if top != nil {
			// No auth array — the op didn't need user auth for any
			// downstream call (rare for token-moving paths but allowed
			// by the protocol). Only the top-level call is recorded.
			calls = append(calls, top)
		}

		if len(calls) > 0 {
			out[i] = calls
		}
	}
	return out
}

// extractInvokeContractCalls returns, parallel to ops, the top-level invokeCall of each InvokeContract op
// (nil otherwise). Called once per tx by ProcessLedger for OpArgs enrichment and call routing.
func extractInvokeContractCalls(ops []xdr.Operation) []*invokeCall { //nolint:gocognit // dispatch-heavy; splitting would reduce linearity
	if len(ops) == 0 {
		return nil
	}
	out := make([]*invokeCall, len(ops))
	for i, op := range ops {
		if op.Body.Type != xdr.OperationTypeInvokeHostFunction {
			continue
		}
		ihf, ok := op.Body.GetInvokeHostFunctionOp()
		if !ok {
			continue
		}
		if ihf.HostFunction.Type != xdr.HostFunctionTypeHostFunctionTypeInvokeContract {
			continue
		}
		ic, ok := ihf.HostFunction.GetInvokeContract()
		if !ok {
			continue
		}
		// ContractAddress may be account-typed in rare composed
		// calls; Band's real target is always a ScAddressTypeContract.
		// accountIDToStrkey / contract-address strkey are handled by
		// separate helpers; we mirror dispatcher.contractIDToStrkey
		// here via the address-kind switch for safety.
		contractStrkey := ""
		switch ic.ContractAddress.Type {
		case xdr.ScAddressTypeScAddressTypeContract:
			cid := ic.ContractAddress.MustContractId()
			if s, err := contractIDToStrkey(cid); err == nil {
				contractStrkey = s
			}
		case xdr.ScAddressTypeScAddressTypeAccount:
			// InvokeContract against an account address is invalid at
			// the protocol level, but we defensively skip rather than
			// emitting a malformed strkey.
			continue
		}
		if contractStrkey == "" {
			continue
		}
		args := make([]string, 0, len(ic.Args))
		argsOK := true
		for j := range ic.Args {
			raw, err := ic.Args[j].MarshalBinary()
			if err != nil {
				// Marshal failure on a locally-sourced ScVal means a
				// broken envelope or SDK drift. Surface as "no args";
				// decoders that require them (Redstone, Band) will
				// surface their own error and skip.
				argsOK = false
				break
			}
			args = append(args, base64.StdEncoding.EncodeToString(raw))
		}
		if !argsOK {
			args = nil
		}
		out[i] = &invokeCall{
			ContractID:   contractStrkey,
			FunctionName: string(ic.FunctionName),
			Args:         args,
		}
	}
	return out
}

// contractIDToStrkey encodes a 32-byte ContractId into its C-strkey
// form (56 chars). SDK's strkey package owns the canonical encoding.
func contractIDToStrkey(cid xdr.ContractId) (string, error) {
	return strkey.Encode(strkey.VersionByteContract, cid[:])
}

// accountIDToStrkey encodes an xdr.AccountId to its G-strkey form.
// Returns the empty string if the account isn't an Ed25519 — classic
// Stellar accounts always are, but muxed shapes can surprise us.
func accountIDToStrkey(aid xdr.AccountId) (string, error) {
	if aid.Type != xdr.PublicKeyTypePublicKeyTypeEd25519 {
		return "", fmt.Errorf("accountIDToStrkey: unsupported account type %d", aid.Type)
	}
	pub := aid.Ed25519
	return strkey.Encode(strkey.VersionByteAccountID, pub[:])
}

// mustParseRFC3339 parses a closedAt string; panics on malformed
// input because the dispatcher itself formatted it upstream. Any
// failure here is a programming bug, not runtime input.
func mustParseRFC3339(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic("dispatcher: malformed closedAt (self-generated): " + err.Error())
	}
	return t
}
