// Package dispatcher consumes ledger-meta values from
// internal/ledgerstream and routes per-transaction artefacts to
// decoders registered by the internal/sources/<venue> packages.
// Per docs/architecture/ingest-pipeline.md this is the SINGLE
// production ingest codepath — every trade / oracle update that
// lands in Timescale goes through Dispatcher.ProcessLedger.
//
// Dispatcher is intentionally small. The decoders carry all
// protocol-specific logic (topic matching, SCVal parsing,
// correlation buffers for swap+sync or 8-field swaps); the
// dispatcher's per-ledger walk hits three seams in order:
//
//   - [Decoder] (Soroban contract events) — flatten
//     tx.GetTransactionEvents() to events.Event values; the
//     first registered Decoder whose Matches() returns true
//     owns each event. Used by every Soroban source that
//     publishes contract events: soroswap, aquarius, phoenix,
//     comet, reflector (all 3 variants), redstone.
//   - [OpDecoder] (classic XDR operations) — for op types that
//     don't surface as Soroban events; iterate the tx envelope's
//     operations and invoke each OpDecoder whose op-type filter
//     matches. Used by internal/sources/sdex for
//     ManageSellOffer / ManageBuyOffer / CreatePassiveSellOffer /
//     PathPayment* trade extraction.
//   - [ContractCallDecoder] (event-less Soroban contracts) — for
//     contracts that update storage on a known function call but
//     emit zero events. Match by (contract_id, function_name);
//     decoder reads from the InvokeContract op's args. Used by
//     internal/sources/band (relay / force_relay).
//
// All three seams share the "first-match-wins, non-fatal errors
// are logged and counted" contract — see each interface's doc
// comment. Adding a new source is registering against whichever
// seam fits the venue's wire shape; the dispatcher itself stays
// unchanged.
//
// Two of the three seams also carry a discovery hook (sighting-only,
// never attribution — internal/canonical/discovery,
// docs/architecture/generic-oracle-sep-onboarding.md): the event
// seam sniffs topic[0] against both the SEP-41 and a broader
// oracle-suggestive symbol set; the ContractCallDecoder seam sniffs
// (contract_id, function_name) against an oracle-suggestive call
// allow-list, so an event-less oracle in the Band shape gets flagged
// even before any decoder for it exists.
package dispatcher

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/stellar/go-stellar-sdk/ingest"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical/discovery"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder is the contract every source package implements to
// participate in dispatch. Adding a new source is one file:
// export a NewDecoder() returning a value that satisfies this
// interface, then register it with the Dispatcher at startup.
//
// Methods are:
//
//   - Name: canonical source name, stamped into metrics +
//     canonical.Trade.Source / canonical.OracleUpdate.Source.
//   - Matches: byte-equality predicate on the event topic. Cheap —
//     avoid SCVal parsing.
//   - Decode: process one event; optionally emit consumer.Event
//     values (Trade / OracleUpdate wrappers). Sources with
//     correlation state (Soroswap swap+sync, Phoenix 8-field) may
//     return no outputs for intermediate events and emit on
//     completion.
//
// Decode's error is non-fatal — the dispatcher counts it (via the
// caller's metrics hook) and moves on to the next event.
type Decoder interface {
	Name() string
	Matches(ev events.Event) bool
	Decode(ev events.Event) ([]consumer.Event, error)
}

// StateWriteKeyConsumer is an OPTIONAL interface a [Decoder]
// additionally implements to declare that its Decode reads
// events.Event.StateWriteKeys for events of specific contracts.
// StateWriteContracts returns the contract C-strkeys whose events need
// the enrichment (typically the decoder's own gated contract set).
//
// ProcessLedger resolves an operation's value-changing contract-data
// write keys (state_write_keys.go) ONLY for events whose contract is in
// some registered decoder's declared set — the meta walk + per-entry
// XDR marshalling is measurable overhead, and computing it for every
// event-bearing op taxed the whole ledger walk for a signal exactly one
// decoder (redstone) consumes. Events outside every declared set carry
// nil StateWriteKeys, which consumers already must treat as "unknown",
// not "no writes" (events.Event.StateWriteKeys doc).
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

// OpDecoder is the contract for decoders that operate on classic
// Stellar operations (ManageOffer, PathPayment, …) rather than
// Soroban contract events. SDEX is the primary user; any future
// classic-path source (e.g. liquidity-pool trades outside Soroban)
// follows the same shape.
//
// One transaction has many operations and each op has its own
// result. The dispatcher passes both in an OpContext so the
// decoder can correlate them without re-walking the envelope.
//
// Same non-fatal-error contract as [Decoder]: Decode returning an
// error is a "skip + count" signal, not "stop dispatching."
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

// ContractCallDecoder is the contract for decoders that observe
// Soroban InvokeContract calls *regardless of whether the contract
// emits an event*. The canonical use case is Band's Soroban
// StandardReference: its `relay()` / `force_relay()` methods update
// storage but publish no events (verified
// docs/discovery/oracles/band.md) — a conventional event-based
// Decoder would never run on a Band update. ContractCallDecoder
// observes the InvokeContract op itself, decoding the call's
// arguments as the authoritative payload.
//
// Matching is by (contract_id, function_name) — cheap string
// compares, no SCVal parsing on the hot path. The source package
// supplies the args decoding.
//
// Same non-fatal-error contract as [Decoder] and [OpDecoder]:
// returning an error is a "skip + count" signal, not
// "stop dispatching."
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

// ContractCallContext carries everything a ContractCallDecoder
// needs to decode one Soroban InvokeContract call: identity of the
// contract + function, base64-encoded argument slice, and tx-level
// metadata. Built by the dispatcher during ProcessLedger for every
// successful InvokeContract op.
//
// Args are base64-encoded SCVal blobs — same format as
// events.Event.OpArgs / events.Event.Topic — so decoders use
// internal/scval.Parse to unwrap.
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
	// Per task #48 + docs/architecture/contract-call-coverage-audit.md.
	// Decoders that need to dedup overlapping calls in the same tx
	// (rare) can build a stable identifier as (TxHash, OpIndex, CallPath).
	CallPath []int
	// CallPathContracts is the ordered chain of contract C-strkeys
	// from the top-level invocation down to and including THIS call
	// (index-aligned with CallPath's depth: length 1 for a top-level
	// call — CallPathContracts == [ContractID] — length N+1 for a
	// call N levels deep). CallPathContracts[0] is the outermost
	// invoked contract (e.g. an aggregator); CallPathContracts[len-1]
	// always equals ContractID. Built by the same auth-tree walk as
	// CallPath (walkAuthTree) — see ROADMAP #11 / the
	// contract-call-coverage-audit.md "sequencing" note: the walk
	// itself shipped as task #48 Phase 1; this field is the ancestor-
	// identity enrichment that lets a decoder record WHO wrapped the
	// call, not just at what tree depth.
	CallPathContracts []string
}

// LedgerEntryChangeDecoder is the contract for decoders that
// observe raw [xdr.LedgerEntryChange] rows from each LCM,
// regardless of which transaction or fee-meta block produced them.
// Per ADR-0021. Used by sources that derive their state from
// ledger-entry deltas rather than events / ops / contract calls.
//
// The canonical use case is the AccountEntry observer
// (internal/sources/accounts/) which watches operator-configured
// G-strkeys for balance + home_domain changes. Same
// non-fatal-error contract as [Decoder] / [OpDecoder] /
// [ContractCallDecoder]: returning an error is a "skip + count"
// signal, not "stop dispatching."
//
// Matches is the cheap pre-filter — typically checks the entry's
// Data discriminant (e.g. AccountEntry vs Trustline vs ContractCode).
// Decode receives the full context (including tx-level metadata)
// and emits zero or more canonical outputs.
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

// EntryWalkVersion identifies the ledger entry-change walk ORDER that
// produced a [LedgerEntryChangeContext.IntraLedgerSeq]. Bump it whenever the
// walk emits changes in a different sequence.
//
//	1  original: per-transaction walk (fee, apply) tx by tx; failed txs
//	   skipped entirely; no post-apply fee phase.
//	2  ledger-wide three-phase walk (all fees, all apply-phase, all
//	   post-apply fees), failed txs included — C2-023/C2-040/C2-032/R-A01-1,
//	   audit-2026-07-23.
//
// WHY THIS MATTERS, and it is not academic. intra_ledger_seq is PERSISTED
// and COMPARED ACROSS BINARY VERSIONS by two guards:
//
//   - account_observations (and its four sibling *_observations tables):
//     `WHERE intra_ledger_seq <= EXCLUDED.intra_ledger_seq` (migration 0111);
//   - ledger_entries_current_v2: the ReplacingMergeTree version
//     `(ledger_seq << 32) | intra_ledger_seq`.
//
// Both assume the two positions being compared are drawn from the same
// numbering. A version bump RENUMBERS every ledger, so a legacy row can
// OUTRANK a correction: if the v1 walk gave an account's final balance
// position 6, and the v2 walk correctly places that same final balance at
// position 3, the guard evaluates `6 <= 3` = false and the corrected write
// is SILENTLY DROPPED — permanently. Replaying the re-derive does not help,
// because it re-computes the same lower position every time.
//
// THE REPAIR PATH for a version bump is therefore NOT "replay the changes".
// It is RECONSTRUCT-FINAL-THEN-SEED: derive the FINAL per-(key, ledger)
// state and write ONE row per key per ledger stamped
// timescale.SeedIntraLedgerSeq (= math.MaxUint32), which the `<=` guard
// always admits and a re-run re-admits idempotently. It is only sound for a
// reconstructed FINAL state — stamping the sentinel on a change-by-change
// replay would tie every change in the ledger at MaxUint32 and re-open C2-6.
// See migration 0120 and
// docs/operations/runbooks/entry-walk-renumbering.md.
//
// On the ClickHouse side the equivalent repair is the existing
// delete-then-replay per range (the master plan's re-derive procedure): a
// lower RMT version cannot displace a higher one either, so the partition
// must be dropped before re-ingest.
const EntryWalkVersion = 2

type LedgerEntryChangeContext struct {
	Ledger   uint32
	ClosedAt time.Time
	TxHash   string
	OpIndex  int

	// IntraLedgerSeq is the position of this change within the ledger's
	// canonical entry-change walk — a per-ledger monotonic counter assigned
	// in LEDGER-WIDE PHASE order, not per-transaction order:
	//
	//	phase 1  every tx's fee changes            (tx-set apply order)
	//	phase 2  every tx's apply-phase meta       (tx-changes-before,
	//	                                            per-op changes in
	//	                                            op_index/change_index
	//	                                            order, tx-changes-after)
	//	phase 3  every tx's post-apply fee changes (P23 Soroban refunds)
	//
	// This mirrors the SDK's canonical ingest.LedgerChangeReader state
	// machine (feeChangesState → metaChangesState → postTxApplyState), which
	// is how stellar-core actually commits a ledger: all fees are charged
	// before any transaction is applied, and P23 moved the Soroban fee refund
	// into a third phase applied after all transactions execute. So the
	// HIGHEST value for a given ledger entry is its FINAL intra-ledger state.
	//
	// This doc previously described a PER-TRANSACTION order ("within each tx:
	// fee-changes, …") and claimed it was "the same canonical within-ledger
	// order entry_change_reader.go uses". Both were false: the per-tx walk
	// ranked tx1's apply-phase change BELOW tx2's fee change, so the balance
	// upsert published a fee-phase balance as the ledger-final balance
	// (C2-032, audit-2026-07-23).
	//
	// The balance-observation writers persist this alongside the value and
	// guard their last-writer-wins upsert on it
	// (intra_ledger_seq <= EXCLUDED.intra_ledger_seq) so an out-of-order
	// PersistEvents worker can never overwrite a later intra-ledger change
	// with an earlier one (audit-2026-07-16 C2-6). Counter resets per ledger;
	// correctness only needs monotonicity WITHIN a ledger (rows from different
	// ledgers never share the observation PK). Unmatched changes still consume
	// a value (gaps are harmless — only relative order matters).
	//
	// POSITIONS ARE SCOPED TO [EntryWalkVersion] — a position is only
	// comparable against another produced by the SAME walk version. Read that
	// constant before writing any corrective re-derive.
	IntraLedgerSeq uint32

	Change xdr.LedgerEntryChange
}

// Dispatcher owns the registered decoders. Construct with New(),
// register exactly once at startup, then call ProcessLedger per
// xdr.LedgerCloseMeta delivered by internal/ledgerstream.
//
// Not safe for concurrent ProcessLedger calls — caller should
// serialize. (The ledgerstream callback model naturally
// serializes, so this is the intended usage.) The internal stats
// counters ARE safe for a concurrent Stats() reader (the statsflush
// goroutine) — they're guarded by statsMu (F-1317).
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

	// rawEventSink, when non-nil, receives EVERY Soroban contract
	// event observed by [dispatchOne], regardless of whether a
	// per-source decoder claimed it. Powers the `soroban_events`
	// raw-event landing zone (ADR-0029) — every event the dispatcher
	// routes is also captured as a row so future per-source decoder
	// backfills become SQL queries rather than MinIO re-walks. A nil
	// sink disables the hook; the dispatcher behaves as if it weren't
	// there. See [Dispatcher.SetRawEventSink].
	rawEventSink RawEventSink

	// statsMu guards every read + write of the counter fields below
	// (eventsSeen / decodeErrors / unmatchedHits / txReadErrors).
	// ProcessLedger mutates them on the dispatch goroutine while the
	// statsflush goroutine reads them via Stats(); without this lock
	// the concurrent map access is a fatal `concurrent map read and
	// map write` panic (F-1317). Critical sections are kept tiny — a
	// single `++` under Lock, or one snapshot copy under Lock — so the
	// dispatch hot path pays only an uncontended mutex per matched
	// input.
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
	// signal rather than have it disappear silently. Pre-2026-05-10
	// the silent skip meant a slow corruption (ingest seeing a real
	// drop rate) was invisible until a downstream price gap
	// triggered a manual investigation.
	txReadErrors int

	// txEventReadErrors counts transactions whose GetTransactionEvents()
	// returned an error during ProcessLedger. The SDK returns an error
	// for an unsupported TransactionMeta version — so on a future
	// protocol meta bump EVERY tx's Soroban events would silently vanish
	// (the event-dispatch block is gated on err==nil). Without this
	// counter that break is invisible: soroban_events rows + the census
	// count would both drop to zero in lock-step and the ADR-0033
	// reconcile would still read "complete" (G15-06). A sustained climb
	// here means Soroban ingestion is broken regardless of what the
	// completeness verdict says.
	txEventReadErrors int

	// entryMetaUnsupported counts transactions whose apply-phase
	// LedgerEntryChange walk was skipped because their TransactionMeta
	// carried a version this walk does not handle. Unreachable on
	// production input today (galexie's captive core re-generates meta
	// at replay time — verified across protocols 1→19 on 2026-08-04),
	// so a non-zero value means either an archive re-derived by an old
	// core binary or a protocol that bumped meta past V4. Either way
	// every classic balance / trustline / offer / LP change in those
	// transactions is invisible, and without this counter that is
	// indistinguishable from a ledger in which nothing happened.
	entryMetaUnsupported int
}

// New constructs a Dispatcher with the given Soroban-event
// decoders. Registration order determines first-match precedence
// — earlier wins. Classic-op decoders register via AddOpDecoder
// after construction.
func New(decoders ...Decoder) *Dispatcher {
	return &Dispatcher{
		decoders:     decoders,
		eventsSeen:   map[string]int{},
		decodeErrors: map[string]int{},
	}
}

// AddOpDecoder registers a classic-operation decoder. Use for
// SDEX and any future non-Soroban path. Called once at startup;
// not safe concurrent with ProcessLedger.
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

// AddEntryDecoder registers a decoder that observes raw
// LedgerEntryChange rows. Per ADR-0021. Called once at startup;
// not safe concurrent with ProcessLedger.
//
// Registration order determines first-match precedence — same
// shape as the other three hooks. A change that no decoder
// matches is silently skipped (entry changes are not counted
// against `unmatchedHits` because they're high-volume — every
// successful tx produces several — and the unmatched count would
// dominate the metric).
func (d *Dispatcher) AddEntryDecoder(ld LedgerEntryChangeDecoder) {
	d.entryDecoders = append(d.entryDecoders, ld)
}

// AddDecoder registers a Soroban event-stream Decoder after
// construction. Mirrors the behaviour of the variadic [New]
// constructor — registration order determines first-match
// precedence. Called once at startup; not safe concurrent with
// ProcessLedger.
//
// Most event-stream decoders register via [New] (the trade /
// oracle decoders selected by `cfg.Ingestion.EnabledSources`).
// AddDecoder exists for the supply-side observers that opt in
// per a separate config block (`[supply] watched_sep41_contracts`)
// — those don't fit the EnabledSources model because they're
// per-asset rather than per-source.
func (d *Dispatcher) AddDecoder(dec Decoder) {
	d.decoders = append(d.decoders, dec)
}

// DiscoverySink is the side-effect interface the dispatcher uses to
// notify the auto-discovery layer about watched-shape sightings.
// Push is called once per hit from any of three sniffers:
// [discovery.Sniff] (topic[0] is one of the four SEP-41 event
// symbols), [discovery.SniffOracleEvent] (topic[0] is in the broader
// oracle-suggestive symbol set), or [discovery.SniffOracleCall]
// (an InvokeContract call's function name is in the oracle-suggestive
// call allow-list — the event-less-oracle / Band-alike case). Push
// MUST be non-blocking — the dispatcher runs on the ingest hot
// path and a slow Push would back-pressure the entire pipeline.
//
// The standard implementation buffers Hit records to a channel and
// drains them in a worker goroutine that calls
// discovery.Recorder.Record against the storage layer. See
// internal/canonical/discovery for the Recorder contract +
// in-memory variant; the binary-side async adapter wires the two.
// One sink instance serves all three sniffers — there is no
// parallel discovery storage/reporting surface.
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

// RawEventSink is the side-effect interface the dispatcher uses to
// notify the catch-all soroban_events landing zone (ADR-0029) about
// every Soroban contract event it sees. PushEvent is called exactly
// once per event observed by [dispatchOne], BEFORE the per-source
// decoder chain runs (so an event a decoder later rejects still
// lands in soroban_events — operators see every contract emitting
// events, not just ones we successfully decode).
//
// PushEvent MAY block to apply back-pressure. The standard
// implementation buffers Rows to a channel and drains them in a
// worker goroutine that calls
// [timescale.Store.InsertSorobanEventsBatch] (see
// internal/sources/sorobanevents); when that buffer fills, the
// dispatcher slows down to match the worker's drain rate so the
// backfill cursor (which advances per produced ledger) cannot
// outrun durable writes. The previous non-blocking buffer-full-drop
// semantics were proved unsafe by the 2026-05-26 fill walk, which
// dropped ~0.43% of rows across 8 chunks without a recovery path
// (the cursor was already past the dropped ledgers, so -resume
// short-circuited).
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
	EventsSeen    map[string]int
	DecodeErrors  map[string]int
	OrphanEvents  map[string]int
	UnmatchedHits int
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
	// still reads "complete" (G15-06).
	TxEventReadErrors int
	// EntryMetaUnsupported counts transactions whose apply-phase entry
	// change walk was skipped for an unhandled TransactionMeta version.
	// A sustained climb means the LedgerEntry supply observers are
	// blind while every component table simply stops advancing.
	EntryMetaUnsupported int
}

func (d *Dispatcher) Stats() Stats {
	// Snapshot the counter fields under statsMu so this read can't
	// race the dispatch goroutine's `++` mutations (F-1317). Kept to
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
	unmatched := d.unmatchedHits
	txReadErrs := d.txReadErrors
	txEventReadErrs := d.txEventReadErrors
	entryMetaUnsup := d.entryMetaUnsupported
	d.statsMu.Unlock()

	orphanCopied := map[string]int{}
	for _, dec := range d.decoders {
		reporter, ok := dec.(interface{ EvictedOrphans() int })
		if !ok {
			continue
		}
		if n := reporter.EvictedOrphans(); n > 0 {
			orphanCopied[dec.Name()] = n
		}
	}
	return Stats{
		EventsSeen:           seenCopied,
		DecodeErrors:         decodeCopied,
		OrphanEvents:         orphanCopied,
		UnmatchedHits:        unmatched,
		TxReadErrors:         txReadErrs,
		TxEventReadErrors:    txEventReadErrs,
		EntryMetaUnsupported: entryMetaUnsup,
	}
}

// ProcessLedger walks lcm's transactions, extracts Soroban events,
// and routes each one to the matching decoder. Returns the
// collected outputs across all events in the ledger.
//
// passphrase must match the network the ledger came from
// (mainnet / testnet). The SDK uses it to compute transaction
// hashes during iteration.
//
// Errors:
//   - A failure to construct the transaction reader (bad LCM)
//     returns an error immediately.
//   - Per-transaction read errors are skipped with an internal
//     counter bump on `Stats().TxReadErrors`. The statsflush
//     periodic snapshot logs at WARN whenever the delta in a
//     flush window > 0 — operators see the silent-corruption
//     signal instead of having it disappear (the pre-2026-05-10
//     behaviour).
//   - Per-event decode errors are skipped; the caller sees a
//     successful return with fewer outputs.
//
// Caller controls goroutine placement. This function blocks until
// the ledger is fully processed.
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
	// [Dispatcher.walkLedgerEntryChanges] (C2-032, audit-2026-07-23).
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
			// `Stats().TxReadErrors` surfaces the drop — silent skip
			// here meant a slow corruption rate was invisible
			// pre-2026-05-10.
			d.statsMu.Lock()
			d.txReadErrors++
			d.statsMu.Unlock()
			continue
		}
		txs = append(txs, tx)
	}

	// ─── LedgerEntryChange walk (ADR-0021) ───────────────────────
	// Runs over the WHOLE ledger — including failed txs, whose fee
	// debits are committed on chain (C2-023/C2-040) — and in the
	// chain's two-phase order. Skipped cheaply when no entry decoders
	// are registered.
	if len(d.entryDecoders) > 0 {
		outputs = append(outputs,
			d.walkLedgerEntryChanges(txs, ledgerSeq, parsedClosedAt)...)
	}

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
		// Walk operations once, build an invokeCalls slice keyed by
		// opIdx. This powers three downstream consumers:
		//   1. events.Event.OpArgs for event-path decoders that
		//      need the tx's args (Redstone) — scoped by the OpArgs
		//      provenance gate below to events of the invoked
		//      contract itself.
		//   2. ContractCallDecoder routing (Band and any future
		//      source that doesn't emit events).
		//   3. No-op for non-InvokeContract ops (classic, wasm
		//      upload, etc.) — the slot is nil.
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
			// G15-06: an unsupported future TransactionMeta version makes
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
				// State-write enrichment (sibling of the OpArgs plumb
				// below): the contract-data entries whose VALUE this op
				// changed, from tx meta — filtered per event to the
				// event's own contract. Redstone's exact subset
				// attribution reads them; see state_write_keys.go.
				// Resolved LAZILY, once per op, and only when an event's
				// contract belongs to a decoder that declared interest
				// via [StateWriteKeyConsumer] — the meta walk + XDR
				// marshalling is pure overhead for every other op.
				var opWrites []contractDataWrite
				opWritesResolved := false
				for evIdx, ce := range opEvents {
					ev := contractEventToEventsEvent(ce, ledgerSeq, txHash, opIdx, evIdx, closedAt, nil)
					if ev == nil {
						continue
					}
					// ─── OpArgs provenance gate ──────────────────
					// The op's top-level InvokeContract args belong to
					// the CALLEE of that top-level call and to nobody
					// else. Attach them only to events emitted by the
					// invoked contract itself. Events emitted by OTHER
					// contracts in the same op (sub-invocations reached
					// through a wrapper/aggregator) get NO args: a
					// wrapper's top-level args are attacker-chosen free
					// text relative to the sub-call that actually
					// emitted the event, and pre-gate they were attached
					// to every event the op produced — letting a wrapper
					// call adapter.write_prices with the real payload
					// while steering redstone's feed_ids attribution via
					// its own top-level args. Post-gate an args-requiring
					// decoder refuses honestly (redstone:
					// ErrMissingOpArgs, counted via the decode-error
					// counter) instead of trusting foreign args.
					// The lake extractor applies the identical rule at
					// write time (clickhouse/extract.go opArgsByIndex).
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
		// Per task #48: walks the FULL auth tree of each op (top-level
		// invocation PLUS every transitively-nested sub-call), not
		// just the top-level. This is the canonical source for
		// ContractCallDecoder routing because most Soroswap traffic
		// reaches the router as a sub-invocation of an aggregator
		// contract — the pre-#48 top-level-only walk missed ~99.99%
		// of router calls (see
		// docs/architecture/contract-call-coverage-audit.md).
		//
		// Each decoder's Matches() runs per call in the tree; on a
		// match, Decode() emits an event whose CallPath identifies
		// the node's position. Decoders are stateless w.r.t. tree
		// position — they care only about (contract_id,
		// function_name, args).
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
				for _, call := range calls {
					ccCtx := ContractCallContext{
						Ledger:            ledgerSeq,
						ClosedAt:          parsedClosedAt,
						TxHash:            txHash,
						TxSource:          txSource,
						OpSource:          opSource,
						OpIndex:           opIdx,
						CallPath:          call.CallPath,
						CallPathContracts: call.CallPathContracts,
						ContractID:        call.ContractID,
						FunctionName:      call.FunctionName,
						Args:              call.Args,
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
			outs, err := d.dispatchOp(opCtx)
			if err != nil {
				continue
			}
			outputs = append(outputs, outs...)
		}
	}
	return outputs, nil
}

// walkLedgerEntryChanges walks EVERY LedgerEntryChange in the ledger and
// dispatches each to the entry-decoder chain, in the order stellar-core
// committed them. Returns the collected outputs across every matched change.
//
// Two properties this function exists to guarantee — both are correctness
// requirements of the balance-observation surface, not cosmetics:
//
//  1. FAILED TXS ARE INCLUDED (C2-023/C2-040, audit-2026-07-23). A failed
//     transaction still debits its fee on chain, and stellar-core commits
//     that fee change; only the operation changes are rolled back (a failed
//     tx's meta carries no op changes, so nothing rolled-back is replayed).
//     Skipping failed txs therefore over-reported the observed balance by
//     the fee and put the live observer permanently out of step with the
//     lake, whose clickhouse.extractEntryChanges has always walked every tx.
//     ADR-0034's "re-derive the LedgerEntry supply observers from the lake"
//     promise is only meetable if the two agree.
//
//  2. THE WALK IS THREE-PHASE, LEDGER-WIDE (C2-032, and R-A01-1 for the
//     third phase). stellar-core commits a ledger in ledger-wide phases,
//     not tx by tx, and this walk follows the SDK's canonical
//     ingest.LedgerChangeReader state machine (feeChangesState →
//     metaChangesState → postTxApplyState) — see the phase table below.
//     "Follows", not "mirrors exactly": the SDK additionally suppresses
//     changes for txInternalError() at LedgerVersion <= 12, which this
//     walk does not, so where the two diverge we emit MORE rather than
//     fewer changes (inert in practice — such txs carry no operations in
//     the V4 meta production actually delivers). Corrected 2026-08-04;
//     the word "exactly" invited readers to treat the walks as
//     interchangeable.
//     Walking per tx (fee, apply, then the next tx's fee) mis-ranked an
//     account touched by tx1's ops and tx2's fee: IntraLedgerSeq put the fee
//     last, and IntraLedgerSeq is exactly the tiebreak that makes the FINAL
//     intra-ledger change win the balance upsert. That published a
//     fee-phase balance as the ledger-final balance. Omitting phase 3 has
//     the same shape on P23+ ledgers: the refund is the LAST thing that
//     touches the fee-source account, so dropping it publishes the
//     pre-refund balance as final.
//
// The three phases, in emission order:
//
//	phase 1  every tx's FeeChanges             processFeeSeqNum charges ALL
//	                                           fees before applying ANY tx
//	phase 2  every tx's apply-phase meta       TxChangesBefore, per-op
//	                                           changes, TxChangesAfter
//	phase 3  every tx's PostTxApplyFeeChanges  P23 moved the Soroban fee
//	                                           REFUND out of TxChangesAfter
//	                                           into a ledger-wide phase run
//	                                           after all txs execute; LCM V2
//	                                           only, so empty pre-P23
//
// LEDGER UPGRADES (the SDK's 4th state, upgradeChangesState) are deliberately
// NOT walked: they are not transaction-scoped, carry no TxHash, and no
// LedgerEntryChangeDecoder consumes them today. The lake walker makes the
// identical choice, so the two stay in step.
//
// IntraLedgerSeq is the per-ledger monotonic position, advanced for every
// walked change (matched or not) so relative order is preserved; gaps from
// unmatched changes are harmless (C2-6).
//
// POSITIONS ARE WALK-VERSION-SCOPED. IntraLedgerSeq is only comparable
// against another position produced by the SAME walk version. This change
// renumbered every ledger, so a legacy row can outrank a correction — see
// [EntryWalkVersion] and migration 0120 for the invariant and the repair
// path.
//
// Meta-version handling: V3 + V4 share the same Operations / TxChanges
// shape for entry-change purposes; the only difference is the wrapping
// type. Anything else lands in the default arm and is COUNTED, not
// silently skipped — see below.
//
// V0/V1/V2 are unreachable on production input, but NOT for the reason
// this comment used to give. It claimed "V1/V2 don't carry post-Soroban
// operation metadata so their apply phase is skipped … which is correct".
// That is false: xdr.TransactionMetaV1{TxChanges, Operations} and
// TransactionMetaV2{TxChangesBefore, Operations, TxChangesAfter} both
// carry full per-operation LedgerEntryChanges, which is exactly where
// every pre-Soroban trustline / offer / account change lives, and the
// SDK's own LedgerChangeReader handles V1 and V2 in its apply phase. The
// real reason we never see them is that galexie's captive stellar-core
// RE-GENERATES TransactionMeta at replay time in the newest format its
// binary supports: only the LedgerCloseMeta wrapper is epoch-native.
// Verified 2026-08-04 by decoding 48 production ledgers from both
// datastores this code reads (r1 galexie-archive + the aws-public-
// blockchain cold tier) spanning protocols 1→19 and 8,000+ transactions:
// every single tx carried meta V4, including ledger 1,000,023 at
// protocol 1. r1's own lake agrees — 70.5 billion apply-phase
// ledger_entry_changes rows below the protocol-20 activation, back to
// ledger 3.
//
// So the default arm is defence against two futures, not a live gap:
// an archive re-derived by a pre-CAP-67 core binary, and a protocol that
// bumps meta past V4. Both would otherwise stop entry-change observation
// dead while every table simply stopped advancing — the exact
// "masquerading as clean ledgers" failure the sibling tx-event path was
// taught to count in G15-06 (cold audit 2026-08-04).
func (d *Dispatcher) walkLedgerEntryChanges(txs []ingest.LedgerTransaction, ledgerSeq uint32, closedAt time.Time) []consumer.Event {
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
		for j := range txs[i].FeeChanges {
			outputs = append(outputs, dispatch(-1, txs[i].FeeChanges[j])...)
		}
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
		for j := range txs[i].PostTxApplyFeeChanges {
			outputs = append(outputs, dispatch(-1, txs[i].PostTxApplyFeeChanges[j])...)
		}
	}
	return outputs
}

// entryChangeTxHash is the hex tx hash used to stamp entry-change contexts.
// Kept separate from the ProcessLedger-local encoding so the two-phase walk
// computes it identically in both phases.
func entryChangeTxHash(tx *ingest.LedgerTransaction) string {
	return hex.EncodeToString(tx.Result.TransactionHash[:])
}

// walkChangeSet dispatches each LedgerEntryChange in the slice
// at the given opIndex (-1 for tx-level / fee-meta blocks).
func walkChangeSet(changes []xdr.LedgerEntryChange, opIdx int, dispatch func(int, xdr.LedgerEntryChange) []consumer.Event) []consumer.Event {
	var outs []consumer.Event
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

// bumpEventsSeen increments the per-source events_seen counter under
// statsMu (F-1317). Called pre-Decode on every matched input across
// all four dispatch seams. The lock is held only for the single map
// write so the decoder's own work runs lock-free.
func (d *Dispatcher) bumpEventsSeen(name string) {
	d.statsMu.Lock()
	d.eventsSeen[name]++
	d.statsMu.Unlock()
}

// bumpDecodeError increments the per-source decode-error counter
// under statsMu (F-1317). Called when a matched decoder's Decode
// returns an error.
func (d *Dispatcher) bumpDecodeError(name string) {
	d.statsMu.Lock()
	d.decodeErrors[name]++
	d.statsMu.Unlock()
}

// bumpUnmatched increments the unmatched-events counter under
// statsMu (F-1317).
func (d *Dispatcher) bumpUnmatched() {
	d.statsMu.Lock()
	d.unmatchedHits++
	d.statsMu.Unlock()
}

// contractCallPathActive reports whether ProcessLedger needs to walk
// each op's full InvokeContract auth tree at all. True when at least
// one ContractCallDecoder is registered (the pre-existing condition)
// OR a discovery sink is installed (docs/architecture/generic-oracle-sep-onboarding.md
// §3(b)(2)) — the oracle-call discovery hook lives in
// dispatchContractCall, which is only ever invoked from inside that
// walk, so without this widened condition an event-less-oracle
// sighting would silently depend on Band (or some other
// ContractCallDecoder) happening to be registered in the running
// binary. False for both means the walk is skipped entirely — the
// "cheap prefilter first" property: zero per-op overhead when
// neither consumer wants call-tree data.
func (d *Dispatcher) contractCallPathActive() bool {
	return len(d.contractCallDecoders) > 0 || d.discoverySink != nil
}

// dispatchContractCall runs one InvokeContract op through the
// contract-call decoder chain. First matching decoder owns it.
//
// Discovery hook (docs/architecture/generic-oracle-sep-onboarding.md
// §3(b)(2)): BEFORE the decoder pass, every call is run through
// [discovery.SniffOracleCall] — a cheap map lookup on FunctionName,
// no arg decoding. This is the event-less-oracle symmetric hook to
// dispatchOne's event-path discovery: Band's relay()/force_relay()
// update storage without publishing an event, so a topic-only
// sniffer can never see a Band-alike. Runs regardless of whether any
// ContractCallDecoder ultimately matches — same "record a sighting
// even if nothing downstream claims it" discipline as the event
// path — see the widened gate in ProcessLedger that keeps this hook
// live even when zero ContractCallDecoders are registered.
func (d *Dispatcher) dispatchContractCall(ctx ContractCallContext) ([]consumer.Event, error) {
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
	for _, ccd := range d.contractCallDecoders {
		if !ccd.Matches(ctx.ContractID, ctx.FunctionName) {
			continue
		}
		d.bumpEventsSeen(ccd.Name())
		outs, err := ccd.Decode(ctx)
		if err != nil {
			d.bumpDecodeError(ccd.Name())
			return nil, err
		}
		return outs, nil
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
func (d *Dispatcher) dispatchEntryChange(ctx LedgerEntryChangeContext) ([]consumer.Event, error) {
	for _, ld := range d.entryDecoders {
		if !ld.Matches(ctx.Change) {
			continue
		}
		d.bumpEventsSeen(ld.Name())
		outs, err := ld.Decode(ctx)
		if err != nil {
			d.bumpDecodeError(ld.Name())
			return nil, err
		}
		return outs, nil
	}
	return nil, nil
}

// RouteEntryChange is the test-harness entry point for
// LedgerEntryChange decoders, symmetric with Route / RouteOp /
// RouteContractCall. Per ADR-0021.
func (d *Dispatcher) RouteEntryChange(ctx LedgerEntryChangeContext) ([]consumer.Event, error) {
	return d.dispatchEntryChange(ctx)
}

// dispatchOp runs one operation through the op-decoder chain.
func (d *Dispatcher) dispatchOp(ctx OpContext) ([]consumer.Event, error) {
	for _, od := range d.opDecoders {
		if !od.Matches(ctx.Op) {
			continue
		}
		d.bumpEventsSeen(od.Name())
		outs, err := od.Decode(ctx)
		if err != nil {
			d.bumpDecodeError(od.Name())
			return nil, err
		}
		return outs, nil
	}
	return nil, nil
}

// RouteOp is the test-harness entry point for classic-op
// decoders, symmetric with Route for Soroban events.
func (d *Dispatcher) RouteOp(ctx OpContext) ([]consumer.Event, error) {
	return d.dispatchOp(ctx)
}

// Route feeds one event through the Matches/Decode chain and
// returns the emitted consumer.Events. Returns an error only when
// the matching decoder fails Decode — mismatch is silent (events
// no decoder claimed are counted in Stats().UnmatchedHits and
// return (nil, nil)).
//
// Exposed for test-harness and fixture-replay use; ProcessLedger
// calls Route internally for every event it extracts.
func (d *Dispatcher) Route(ev events.Event) ([]consumer.Event, error) {
	return d.dispatchOne(ev)
}

// dispatchOne runs one event through the Matches/Decode chain and
// returns outputs. Returns an error only when the matching decoder
// fails Decode — mismatch is silent (events not claimed by any
// decoder are counted and dropped).
//
// Discovery hooks: BEFORE the decoder pass, every event is run
// through [discovery.Sniff] (SEP-41-shaped) AND
// [discovery.SniffOracleEvent] (broader oracle-suggestive topic
// set — docs/architecture/generic-oracle-sep-onboarding.md §3(b)(1)).
// Either hit is forwarded to the configured [DiscoverySink] (when
// set); an event can trip at most one of the two (the symbol sets
// are disjoint). Discovery runs first so even events a decoder later
// rejects (e.g. malformed body) still appear in discovered_assets —
// operators want to see every contract emitting a watched event,
// not just ones we successfully decode.
//
// Raw-event hook (ADR-0029): every event is also forwarded to the
// configured [RawEventSink] (when set). Runs BEFORE the decoder
// pass for the same reason as the discovery hooks — and ALSO
// regardless of whether topic[0] decodes to a watched symbol (this
// hook is the catch-all for every Soroban contract event,
// powering the `soroban_events` raw-event landing zone).
func (d *Dispatcher) dispatchOne(ev events.Event) ([]consumer.Event, error) {
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
	for _, dec := range d.decoders {
		if !dec.Matches(ev) {
			continue
		}
		d.bumpEventsSeen(dec.Name())
		outs, err := dec.Decode(ev)
		if err != nil {
			d.bumpDecodeError(dec.Name())
			return nil, err
		}
		return outs, nil
	}
	d.bumpUnmatched()
	return nil, nil
}

// contractEventToEventsEvent flattens an xdr.ContractEvent into
// our transport-neutral events.Event. Returns nil for non-contract
// events (e.g. diagnostic) — the decoders never match on those, so
// we drop them before routing rather than handing them through.
//
// opArgs carries the base64-encoded SCVal arguments of the
// InvokeContract call that produced this op's events, if any.
// ProcessLedger passes nil and attaches args AFTER conversion, once the
// event's own contract is known — args are attached only when the op's
// invoked contract equals the emitting contract (the OpArgs provenance
// gate; see the ProcessLedger event loop). The parameter remains for
// callers that have already established provenance.
//
// evIdx is the position of this event within the operation's
// contract-event list (the caller's range index). It becomes
// events.Event.EventIndex and ultimately the soroban_events
// event_index column, making (ledger, tx_hash, op_index, event_index)
// unique per event — without it multi-event ops collide on the PK
// (ADR-0033).
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

// invokeCall is the per-op snapshot of a Soroban InvokeContract
// call. Contract ID is a C-strkey, function name is the raw
// Symbol string, args are base64-encoded SCVal blobs matching
// the events.Event.OpArgs wire format.
//
// CallPath identifies the position of this call in the tx's auth
// tree. Empty == top-level (the op's direct invocation).
// Non-empty == sub-invocation; each int is the index into the
// parent's SubInvocations slice (e.g. [0] = first sub of root,
// [0,1] = second sub of the first sub of root). Used by
// ContractCallDecoder consumers to dedup or tag attribution
// across overlapping calls in the same tx — see task #48 +
// docs/architecture/contract-call-coverage-audit.md.
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
}

// buildInvokeCallFromArgs projects an [xdr.InvokeContractArgs]
// (the canonical "contract + function + args" tuple shared by the
// top-level HostFunction and every SorobanAuthorizedInvocation
// auth-tree node) into our internal [invokeCall]. Returns nil if
// the contract address can't be encoded to a C-strkey (defensively
// skip rather than emit a malformed strkey — see the original
// extractInvokeContractCalls switch).
//
// `path` is copied into the returned struct so the caller can
// safely reuse the slice across recursive walks. `ancestorChain` is
// the ordered contract C-strkeys of every ancestor ABOVE this node
// (outermost first); this call's own contract is appended to form
// the returned invokeCall's CallPathContracts, so the caller passes
// the SAME slice it received (not one it has already extended).
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

// walkAuthTree appends every InvokeContract call reachable from
// `node` (the node itself + every transitively-nested sub-invocation)
// to `out`, in pre-order depth-first traversal. `path` is the
// CallPath to this node; `ancestorChain` is the ordered contract
// C-strkeys of every ContractFn ancestor above this node (outermost
// first) — each recursive call extends `path` with the child index
// and, for ContractFn nodes, extends `ancestorChain` with this
// node's own contract so descendants can report the full chain down
// to themselves (ContractCallContext.CallPathContracts).
//
// CreateContract / CreateContractV2 nodes are skipped as invokeCalls
// — they don't represent ContractCallDecoder-relevant call shapes
// (no function name to match) — but their SubInvocations are still
// walked with the ancestorChain UNCHANGED: a contract-creation node
// has no "contract being invoked" identity to add to the chain, so a
// ContractCallDecoder-relevant call nested beneath one (e.g. a
// constructor invoking another contract) reports the chain as it was
// immediately above the creation node. Only
// SOROBAN_AUTHORIZED_FUNCTION_TYPE_CONTRACT_FN entries become
// invokeCalls or extend the chain.
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

// walkAuthEntries turns an op's auth entries into the flat call list,
// classifying EACH entry independently as "is the op's top-level call"
// or "is a nested call that must be re-rooted under it".
//
// Independence is the whole point. Until 2026-08-04 the re-rooting was
// all-or-nothing: every entry was first walked with a nil path, and the
// re-root pass ran only when NO walked call matched the top-level
// invocation. That is correct for the two homogeneous shapes (all roots
// nested, or the single root IS the top-level) but wrong for the mixed
// one — a co-signed tx where entry 0 authorizes the top-level call and
// entry 1 authorizes a deeper call signed by a different party. There
// containsCall found entry 0's match, the re-root pass was skipped, and
// entries 1..n kept CallPath nil, i.e. were exported as the op's ENTRY
// POINT. Downstream that becomes call_depth 0 / call_kind 'top_level'
// (soroswap_router/decode.go callPosition), so a router wrapped by an
// aggregator is written as though it were the entry point, and
// TagTradesRoutedVia's `call_kind = 'sub_invocation'` join misses —
// under-reporting the real wrapper's volume on /v1/aggregators and
// over-reporting the generic bucket. It also let two calls in one op
// share the (TxHash, OpIndex, CallPath) identity that ContractCallContext
// documents as a stable dedup key. Found by cold audit 2026-08-04.
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
				// the entries as their own roots — the pre-#48 shape.
				walkAuthTree(&auth[j].RootInvocation, nil, nil, &calls)
				continue
			}
			// The auth roots are NESTED calls: the top-level call needed
			// no auth of its own. Re-root them under it so nothing but
			// the real root carries CallPath [] (C2-060).
			walkAuthTree(&auth[j].RootInvocation, []int{j}, top.CallPathContracts, &calls)
		}
		if top != nil {
			calls = append([]*invokeCall{top}, calls...)
		}
		return calls
	}

	// One entry IS the top-level call: walk it as the root so it and its
	// authorized subtree carry their true paths.
	walkAuthTree(&auth[topIdx].RootInvocation, nil, nil, &calls)

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
		walkAuthTree(&auth[j].RootInvocation, []int{next}, top.CallPathContracts, &calls)
		next++
	}
	return calls
}

// containsCall reports whether calls already holds the same invocation as
// c — same contract, same function, same argument encoding. Identity is
// deliberately NOT CallPath: the question it answers is "did the auth walk
// already emit the top-level call under some path", and emitting it twice
// would double every downstream decode (C2-060).
func containsCall(calls []*invokeCall, c *invokeCall) bool {
	for _, existing := range calls {
		if existing.ContractID != c.ContractID || existing.FunctionName != c.FunctionName {
			continue
		}
		if len(existing.Args) != len(c.Args) {
			continue
		}
		same := true
		for i := range existing.Args {
			if existing.Args[i] != c.Args[i] {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return false
}

// extractInvokeContractCallTrees returns, per operation, the full
// list of contract-call snapshots reachable from that op — the
// top-level invocation PLUS every transitively-nested sub-call from
// the op's Soroban auth tree. Result is indexed parallel to ops;
// nil slot for non-InvokeContract ops.
//
// This is the canonical source for ContractCallDecoder routing per
// task #48. The pre-existing [extractInvokeContractCalls] returns
// top-level only (used by the events.Event.OpArgs enrichment path,
// where we attach args to events emitted at the same op_index).
//
// The auth tree is the right call-tree source because:
//   - Every contract call that requires user authorization (which
//     includes every token transfer in a DEX flow) is in the tree.
//   - The recursive structure mirrors the actual Soroban call tree.
//
// The TOP-LEVEL call is always emitted, with CallPath == [] (C2-060,
// audit-2026-07-23). The comment this replaces asserted "the root of
// each auth entry IS the top-level call — no separate dedup needed when
// the auth array is non-empty", and the code only fell back to
// [xdr.HostFunction.MustInvokeContract] when `ihf.Auth` was EMPTY. That
// assertion is false for the ordinary aggregator shape: a
// SorobanAuthorizationEntry's RootInvocation is the root of the subtree
// that needs authorization, so when the top-level call needs no auth but
// something it calls does, the auth root is a NESTED call — and it was
// exported carrying CallPath == [], i.e. labelled as the call-tree root,
// while the real root was never emitted at all.
//
// So when no walked call matches the top-level invocation, the top-level
// call is prepended as the root and each auth entry j is re-rooted
// beneath it at CallPath [j]. That index is the AUTH-ENTRY ordinal, not
// the host's sub-call ordinal — the auth tree does not carry the latter —
// but it is stable across re-derives and, unlike [], it does not claim
// to be the root. The dedup is by (contract, function, args) so a
// top-level call that IS its own auth root is never emitted twice; that
// matters because a duplicate would become a duplicate trade row.
//
// Multiple auth entries (rare; co-signed multi-user txs) are walked
// independently. Duplicate calls across entries are accepted at this
// layer — dispatch-side dedup is the consumer's concern via the
// CallPath identifier.
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
			// by the protocol). The top-level call alone, matching the
			// pre-#48 baseline.
			calls = append(calls, top)
		}

		if len(calls) > 0 {
			out[i] = calls
		}
	}
	return out
}

// extractInvokeContractCalls returns, per operation, the full
// invokeCall snapshot when the op is an InvokeHostFunction invoking
// a contract; nil otherwise. Result is indexed parallel to ops —
// ops[i] → result[i]. Non-InvokeContract ops (wasm upload, create
// contract, classic ops) yield a nil slot.
//
// Called once per tx by ProcessLedger. Fuels both the event-path
// OpArgs enrichment and the ContractCallDecoder routing, so doing
// the XDR walk here saves duplicate work.
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
