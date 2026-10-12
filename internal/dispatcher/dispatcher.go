// Package dispatcher routes ledger-meta values from internal/ledgerstream to the decoders registered
// by internal/sources/<venue>. It is the single production ingest codepath
// (docs/architecture/ingest-pipeline.md): every trade and oracle update goes through ProcessLedger.
//
// Decoders carry all protocol logic; the per-ledger walk hits three seams:
//
//   - [Decoder]: Soroban contract events; the first decoder whose Matches() is true owns each event.
//   - [OpDecoder]: classic operations; EVERY matching OpDecoder runs, since one op can be facts in
//     several domains (a path payment is a trade and a movement).
//   - [ContractCallDecoder]: event-less Soroban contracts, matched by (contract_id, function_name)
//     and decoded from the InvokeContract args (Band relay / force_relay).
//
// Decode errors are logged and counted, never fatal. The event and contract-call seams also sniff
// for discovery (sighting only, never attribution; docs/architecture/oracle-manipulation-defense.md).
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

// Decoder is what a source package implements to join event dispatch; Matches is a cheap topic check (no SCVal parsing), and Decode may emit nothing for intermediate events of a correlated sequence; a Decode error is counted and dispatch continues.
type Decoder interface {
	Name() string
	Matches(ev events.Event) bool
	Decode(ev events.Event) ([]consumer.Event, error)
}

// Drainer is an optional [Decoder] interface for stateful decoders to flush correlation groups still buffered when a bounded stream ends; without it those groups are lost.
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

// StateWriteKeyConsumer is an optional [Decoder] extension declaring contracts whose events need events.Event.StateWriteKeys; other events carry nil, which consumers MUST read as "unknown", not "no writes".
type StateWriteKeyConsumer interface {
	StateWriteContracts() []string
}

// stateWriteContracts unions every registered decoder's [StateWriteKeyConsumer] contract set. It is
// recomputed per ledger because the decoder list is tiny and fixed after startup.
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

// OpDecoder decodes classic Stellar operations rather than Soroban events; errors are skip-and-count, and unlike [Decoder] every matching OpDecoder runs, so each MUST emit only its own domain's facts.
type OpDecoder interface {
	Name() string
	// Matches is a cheap predicate on the op (typically checks
	// op.Body.Type). Called before Decode.
	Matches(op xdr.Operation) bool
	// Decode emits zero or more canonical outputs from one op +
	// its result. See OpContext for the per-op fields available.
	Decode(ctx OpContext) ([]consumer.Event, error)
}

// OpContext carries what an OpDecoder needs to decode one classic operation: the op, its result and tx-level metadata.
type OpContext struct {
	Ledger   uint32
	ClosedAt time.Time
	TxHash   string
	// TxSource is the G-address of the transaction source account (the fee payer). Ops can
	// override it with their own SourceAccount (see OpSource).
	TxSource string
	// OpSource is the per-op source account, empty when the op has none (the tx-level source
	// applies). Classic trades do not need it, as the claim atoms identify the counterparty.
	OpSource string
	OpIndex  int
	Op       xdr.Operation
	OpResult xdr.OperationResult
}

// ContractCallDecoder observes Soroban InvokeContract calls whether or not the contract emits an event, matching by (contract_id, function_name); errors are skip-and-count, as for [Decoder].
type ContractCallDecoder interface {
	Name() string
	// Matches reports whether this decoder owns the call: contractID is the invoked C-strkey and
	// functionName the targeted Symbol (e.g. "relay").
	Matches(contractID, functionName string) bool
	// Decode emits zero or more canonical outputs from one call; see ContractCallContext.
	Decode(ctx ContractCallContext) ([]consumer.Event, error)
}

// ExecutionCorroborationRequirer is an optional [ContractCallDecoder] extension: the dispatcher drops matched calls whose ExecutionCorroborated is false, because a submitter-supplied auth entry can carry forged price args that never ran (price oracles implement it).
type ExecutionCorroborationRequirer interface {
	// RequiresExecutionCorroboration reports whether this decoder's calls must be
	// execution-corroborated; false is equivalent to not implementing the interface.
	RequiresExecutionCorroboration() bool
}

// RefusesUncorroborated reports whether dec must skip a matched call whose ExecutionCorroborated is false; the live dispatcher and the lake re-derive share it so both gate identically.
func RefusesUncorroborated(dec ContractCallDecoder, corroborated bool) bool {
	r, ok := dec.(ExecutionCorroborationRequirer)
	return ok && r.RequiresExecutionCorroboration() && !corroborated
}

// executionCorroborated reports whether call is the op's top-level executed invocation (top; nil
// for a non-InvokeContract op) rather than an auth-tree declaration that may never have run.
func executionCorroborated(top, call *invokeCall) bool {
	return top != nil && sameInvocation(top, call)
}

// ContractCallContext is what a ContractCallDecoder needs to decode one InvokeContract call; Args are base64 SCVal blobs.
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
	// CallPath is this call's position in the op's auth tree (ADR-0052): empty for the top-level
	// call, else pre-order per-step indices (e.g. [0,1] = second sub-call of the first sub-call
	// of root). (TxHash, OpIndex, CallPath) is a stable identifier for dedup within a tx.
	CallPath []int
	// CallPathContracts is the ordered contract C-strkeys from the top-level invocation down to this call,
	// index-aligned with CallPath's depth ([ContractID] for a top-level call). [0] is the outermost (e.g. an
	// aggregator) and the last always equals ContractID, so a decoder can record WHO wrapped the call.
	CallPathContracts []string
	// AuthOccurrence is how many byte-identical calls (same contract, function, args) precede this
	// one in the SAME auth entry (0 for the first). Each node of one entry authorizes its own
	// execution, so a repeat there is a second call; the same call in a DIFFERENT entry
	// (co-signing) is a duplicate. A decoder keying rows on call content adds this to tell them apart.
	AuthOccurrence int
	// ExecutionCorroborated is true iff this call equals the op's top-level executed InvokeContract
	// call (same contract, function and args). Anything else was only DECLARED in the
	// submitter-supplied auth tree and may never have run; a decoder whose output is a
	// manipulation surface ([ExecutionCorroborationRequirer]) must refuse such calls.
	ExecutionCorroborated bool
}

// LedgerEntryChangeDecoder observes raw [xdr.LedgerEntryChange] rows from each LCM regardless of producing transaction (ADR-0021); Matches pre-filters on the entry's Data discriminant, and errors are skip-and-count.
type LedgerEntryChangeDecoder interface {
	Name() string
	Matches(change xdr.LedgerEntryChange) bool
	Decode(ctx LedgerEntryChangeContext) ([]consumer.Event, error)
}

// LedgerEntryChangeContext carries everything a [LedgerEntryChangeDecoder] needs to decode one
// entry-change delta: the change plus the tx-level metadata that produced it. FeeMeta-block changes
// carry an empty TxHash and OpIndex == -1 to distinguish them from per-op changes.

// EntryWalkVersion identifies the entry-change walk ORDER behind [LedgerEntryChangeContext.IntraLedgerSeq]; bump it only when a change RENUMBERS existing positions, since persisted positions are compared across binaries and an unbumped renumbering silently drops corrections (docs/operations/runbooks/entry-walk-renumbering.md).
const EntryWalkVersion = 3

type LedgerEntryChangeContext struct {
	Ledger   uint32
	ClosedAt time.Time
	TxHash   string
	OpIndex  int

	// IntraLedgerSeq is this change's position in the ledger-wide phased walk, as core commits a
	// ledger. The HIGHEST value for an entry is its final state. Balance writers guard their upsert
	// on it so an out-of-order worker never overwrites a later change. Comparable only within one
	// [EntryWalkVersion].
	IntraLedgerSeq uint32

	Change xdr.LedgerEntryChange
}

// Dispatcher owns the registered decoders: register once at startup, then call ProcessLedger per LedgerCloseMeta; ProcessLedger is not concurrency-safe (the ledgerstream callback serialises it), stats counters are.
type Dispatcher struct {
	decoders             []Decoder
	opDecoders           []OpDecoder
	contractCallDecoders []ContractCallDecoder
	entryDecoders        []LedgerEntryChangeDecoder

	// discoverySink, when non-nil, receives [discovery.Hit] records for every SEP-41-shaped event
	// [dispatchOne] observes; nil disables discovery. See [Dispatcher.SetDiscoverySink].
	discoverySink DiscoverySink

	// rawEventSink, when non-nil, receives EVERY Soroban contract event [dispatchOne] sees, claimed or not,
	// feeding the soroban_events landing zone (ADR-0029) so decoder backfills become SQL, not MinIO re-walks.
	// See [Dispatcher.SetRawEventSink].
	rawEventSink RawEventSink

	// logger serves the decoder-panic guard (recordDecoderPanic) and unreadable evicted keys
	// (walkEvictedKeys), the two signals that must carry a ledger coordinate; everything else is a
	// counter. Nil falls back to slog.Default(); see [Dispatcher.SetLogger].
	logger *slog.Logger

	// statsMu guards the counter fields below (eventsSeen through uncorroboratedCalls): ProcessLedger
	// writes them while statsflush reads Stats(), and unguarded that is a fatal concurrent map
	// access. Critical sections are one `++` or one snapshot copy.
	statsMu sync.Mutex

	// Per-source events_seen, bumped when a decoder's Matches() claims an input. It is the denominator
	// of the decoder error rate written to decoder_stats_5m, bumped pre-Decode so a decoder that matches
	// and then errors still counts.
	eventsSeen map[string]int

	// Error counters, read via Stats(). Production increments obs.SourceDecodeErrorsTotal per source;
	// these are for test assertions.
	decodeErrors  map[string]int
	unmatchedHits int
	// txReadErrors counts malformed transactions skipped during ProcessLedger (a non-EOF Read error).
	// One bad tx must not abort the ledger, but a silent skip would hide slow corruption until a
	// downstream price gap surfaced it.
	txReadErrors int

	// txEventReadErrors counts txs whose GetTransactionEvents() errored (e.g. an unsupported
	// TransactionMeta version). soroban_events and the census would drop in lock-step and the
	// ADR-0033 reconcile still read "complete", so a sustained climb means Soroban ingestion is
	// broken whatever the completeness verdict says.
	txEventReadErrors int

	// entryMetaUnsupported counts txs whose apply-phase entry-change walk was skipped for an
	// unhandled meta version. Non-zero means an archive from an old core or meta past V4, and every
	// classic balance / trustline / offer / LP change in those txs is invisible.
	entryMetaUnsupported int

	// evictedKeysUnreadable counts ledgers whose evicted-key list could not be read, so none of
	// their evictions reached the entry decoders and each evicted balance stays served as live.
	evictedKeysUnreadable int

	// ledgerUpgradeEntries counts upgrade entries seen in closed ledgers. No decoder reads them;
	// the count makes a protocol change visible.
	ledgerUpgradeEntries int

	// uncorroboratedCalls is the per-source count of calls an [ExecutionCorroborationRequirer]
	// decoder matched but the dispatcher dropped as only DECLARED in the auth tree. Non-zero on an
	// oracle is a price-forgery attempt or a routing-shape change (a relayer nesting relay() under
	// a wrapper); either needs review.
	uncorroboratedCalls map[string]int
}

// New constructs a Dispatcher with the given Soroban-event decoders; earlier registration wins first-match precedence.
func New(decoders ...Decoder) *Dispatcher {
	return &Dispatcher{
		decoders:            decoders,
		eventsSeen:          map[string]int{},
		decodeErrors:        map[string]int{},
		uncorroboratedCalls: map[string]int{},
	}
}

// AddOpDecoder registers a classic-operation decoder once at startup, not concurrently with ProcessLedger; every decoder whose Matches claims an op decodes it, in registration order.
func (d *Dispatcher) AddOpDecoder(od OpDecoder) {
	d.opDecoders = append(d.opDecoders, od)
}

// AddContractCallDecoder registers a decoder that observes InvokeContract calls directly, for sources that emit no on-chain events; registration order is first-match precedence.
func (d *Dispatcher) AddContractCallDecoder(ccd ContractCallDecoder) {
	d.contractCallDecoders = append(d.contractCallDecoders, ccd)
}

// AddEntryDecoder registers a LedgerEntryChange decoder once at startup, not concurrently with ProcessLedger; registration order is first-match precedence and unmatched changes are not counted in unmatchedHits.
func (d *Dispatcher) AddEntryDecoder(ld LedgerEntryChangeDecoder) {
	d.entryDecoders = append(d.entryDecoders, ld)
}

// AddDecoder registers a Soroban event Decoder after construction with [New]'s first-match precedence, once at startup and not concurrently with ProcessLedger.
func (d *Dispatcher) AddDecoder(dec Decoder) {
	d.decoders = append(d.decoders, dec)
}

// DiscoverySink receives one Push per discovery hit (SEP-41 topic, oracle-suggestive topic or function name); Push MUST NOT block because it runs on the ingest hot path.
type DiscoverySink interface {
	Push(hit discovery.Hit)
}

// SetDiscoverySink installs the sink called on every watched-shape sighting; nil disables all hooks. Call once at startup, not concurrently with ProcessLedger.
func (d *Dispatcher) SetDiscoverySink(sink DiscoverySink) {
	d.discoverySink = sink
}

// RawEventSink receives one PushEvent per Soroban contract event BEFORE the decoder chain (ADR-0029); PushEvent MAY block so the ledger cursor cannot outrun durable writes, and dropping on full is unsafe because nothing recovers lost rows.
type RawEventSink interface {
	PushEvent(ev events.Event)
}

// SetRawEventSink installs the sink called on every Soroban contract event; nil disables it. Call once at startup, not concurrently with ProcessLedger.
func (d *Dispatcher) SetRawEventSink(sink RawEventSink) {
	d.rawEventSink = sink
}

// SetLogger installs the logger for the decoder-panic guard and eviction-read failures; nil keeps slog.Default(). Call once at startup, not concurrently with ProcessLedger.
func (d *Dispatcher) SetLogger(logger *slog.Logger) {
	d.logger = logger
}

// Stats is a snapshot of the dispatcher's counters keyed by source name, with an "unmatched" total; callers MUST treat it as immutable.
type Stats struct {
	// EventsSeen is the per-source count of inputs a decoder's Matches() claimed: the denominator of
	// the decoder error rate in decoder_stats_5m, without which errors are a count, not a rate.
	EventsSeen   map[string]int
	DecodeErrors map[string]int
	OrphanEvents map[string]int
	// UnknownContractDrops is the per-source count of fully decoded events dropped because the
	// contract (pair/pool) had no token mapping: a lost trade, not a decode error. Collected via a
	// duck-typed interface like OrphanEvents, since the drop returns a nil error inside Decode and
	// DecodeErrors never sees it.
	UnknownContractDrops map[string]int
	// NonDirectionalSwaps is the per-source count of swap+sync pairs recognized as non-directional
	// (a single-side reserve move, not a trade): an expected non-error class (ADR-0033), not lost
	// data. Collected the same duck-typed way.
	NonDirectionalSwaps map[string]int
	UnmatchedHits       int
	// TxReadErrors counts malformed transactions skipped during ProcessLedger; without it the only
	// signal of a sustained climb would be a downstream price gap days later.
	TxReadErrors int
	// TxEventReadErrors counts transactions whose GetTransactionEvents() failed (e.g. an unsupported
	// TransactionMeta version), dropping their Soroban events. Non-zero means Soroban ingestion is
	// broken even if the completeness reconcile still reads "complete".
	TxEventReadErrors int
	// EntryMetaUnsupported counts transactions whose apply-phase entry-change walk was skipped for an
	// unhandled TransactionMeta version; a sustained climb means the supply observers are blind.
	EntryMetaUnsupported int
	// EvictedKeysUnreadable counts ledgers whose evicted-key list failed to read; every eviction in
	// them is missing from the served state.
	EvictedKeysUnreadable int
	// LedgerUpgradeEntries counts ledger-upgrade entries seen. They are observed, not decoded; a
	// non-zero delta marks a network-wide parameter change (protocol, base reserve, Soroban config).
	LedgerUpgradeEntries int
	// UncorroboratedCalls is the per-source count of oracle-class ContractCalls dropped before Decode
	// because they were only declared in the auth tree, never executed. Non-zero on an oracle source
	// ("band") means a price-forgery attempt was rejected or the legitimate routing shape changed;
	// both warrant review.
	UncorroboratedCalls map[string]int
}

func (d *Dispatcher) Stats() Stats {
	// Snapshot the counters under statsMu so this read can't race the dispatch goroutine's `++`.
	// The orphan walk below calls decoder code and stays OUTSIDE the lock (the decoders slice is set
	// once at startup) so the critical section the dispatch path contends on stays tight.
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

// ProcessLedger walks lcm, routes each artefact to its decoder and returns all outputs, blocking until done; passphrase must match the ledger's network, a bad LCM errors, and per-tx read or per-event decode errors are skipped and counted.
func (d *Dispatcher) ProcessLedger(lcm xdr.LedgerCloseMeta, passphrase string) ([]consumer.Event, error) { //nolint:gocognit,gocyclo,funlen // dispatch-heavy; splitting would reduce linearity
	reader, err := ingest.NewLedgerTransactionReaderFromLedgerCloseMeta(passphrase, lcm)
	if err != nil {
		return nil, fmt.Errorf("dispatcher: build reader for ledger %d: %w",
			lcm.LedgerSequence(), err)
	}
	defer func() { _ = reader.Close() }()

	ledgerSeq := lcm.LedgerSequence()
	// RFC 3339 so events.Event matches stellar-rpc's getEvents shape and decoders parsing
	// EventClosedAt need no transport awareness.
	closedAt := lcm.ClosedAt().UTC().Format(time.RFC3339)

	var outputs []consumer.Event
	parsedClosedAt := mustParseRFC3339(closedAt)

	// Read every tx up front: on chain the fee phase for ALL txs precedes the apply phase for ALL
	// txs, so the entry-change walk needs them all (see [Dispatcher.walkLedgerEntryChanges]).
	// LedgerTransaction only holds slices into lcm, so retaining them costs no extra decode.
	txs := make([]ingest.LedgerTransaction, 0, 64)
	for {
		tx, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Skip and continue: one malformed tx must not abort the ledger. The counter keeps
			// the drop visible, since a silent skip would hide a slow corruption rate.
			d.statsMu.Lock()
			d.txReadErrors++
			d.statsMu.Unlock()
			continue
		}
		txs = append(txs, tx)
	}

	// ─── LedgerEntryChange walk (ADR-0021) ───────────────────────
	// Runs over the WHOLE ledger, including failed txs (their fee debits are committed on chain),
	// in the chain's three-phase order, then state-archival evictions. Skipped when no entry
	// decoders are registered.
	if len(d.entryDecoders) > 0 {
		outputs = append(outputs,
			d.walkLedgerEntryChanges(lcm, txs, ledgerSeq, parsedClosedAt)...)
	}
	// Outside the guard: upgrades are observed even with no entry decoders.
	d.noteLedgerUpgrades(lcm.UpgradesProcessing(), ledgerSeq)

	for i := range txs {
		tx := txs[i]
		if !tx.Result.Successful() {
			// Failed transactions carry no price signal; their entry changes are walked above.
			continue
		}

		txHash := hex.EncodeToString(tx.Result.TransactionHash[:])

		// One walk per tx, indexed by opIdx, feeds events.Event.OpArgs (Redstone, scoped by the
		// provenance gate below) and ContractCallDecoder routing (Band). Other ops get a nil slot.
		invokeCalls := extractInvokeContractCalls(tx.Envelope.Operations())
		txSource, _ := accountIDToStrkey(tx.Envelope.SourceAccount().ToAccountId())
		ops := tx.Envelope.Operations()

		// ─── Soroban contract events ─────────────────────────
		// Per-OPERATION events only. Tx-level CAP-67 fee/diagnostic events carry no price/supply
		// signal and are out of scope; the census (census.go) makes the identical choice so the
		// ADR-0033 reconcile stays consistent. Change BOTH sites together.
		txEvents, terr := tx.GetTransactionEvents()
		switch {
		case terr != nil:
			// An unsupported TransactionMeta version fails this for every tx, dropping all Soroban
			// events; count it so the break is not mistaken for a clean empty ledger.
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
				// State-write enrichment (state_write_keys.go): contract-data entries whose VALUE this op
				// changed, filtered to the event's contract (Redstone's subset attribution). Resolved lazily,
				// once per op, only for [StateWriteKeyConsumer] contracts; the meta walk is overhead
				// otherwise.
				var opWrites []contractDataWrite
				opWritesResolved := false
				for evIdx, ce := range opEvents {
					ev := contractEventToEventsEvent(ce, ledgerSeq, txHash, opIdx, evIdx, closedAt, nil)
					if ev == nil {
						continue
					}
					// OpArgs provenance gate: top-level args belong to the invoked contract only. For a
					// sub-invoked contract they are attacker-chosen, so an args-requiring decoder
					// refuses (ErrMissingOpArgs, counted). The lake extractor applies the same rule.
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
		// Walks each op's FULL auth tree: most Soroswap traffic reaches the router as a sub-invocation of
		// an aggregator, so a top-level-only walk misses nearly all router calls (ADR-0052). Decoders match
		// on (contract_id, function_name, args); CallPath records the node's position.
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
				// The op's top-level EXECUTED call (nil for other ops or an unrenderable address). A routed
				// call is execution-corroborated iff it IS this call; anything else came from the
				// submitter-supplied auth tree and may never have run.
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

// walkLedgerEntryChanges dispatches EVERY LedgerEntryChange in the ledger to the entry-decoder
// chain, in the order stellar-core committed them. Two requirements:
//
//  1. FAILED TXS ARE INCLUDED: core commits their fee debit. Skipping them over-reports the balance
//     and diverges from the lake's clickhouse.extractEntryChanges.
//  2. THE WALK IS LEDGER-WIDE AND PHASED: a per-tx walk would rank tx2's fee after tx1's ops on a
//     shared account and publish a fee-phase balance as final; omitting phase 3 does the same.
//
// Phases: 1 every tx's FeeChanges; 2 every tx's apply-phase meta; 3 every tx's
// PostTxApplyFeeChanges (P23 refunds; LCM V2 only); 4 the ledger's EVICTED keys as synthetic
// Removed changes ([walkEvictedKeys]). Ledger upgrades are not walked; [ProcessLedger] counts them.
//
// Only V3/V4 meta is handled; anything else is COUNTED in the default arm, else entry observation
// would stop silently.
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
			// Unreachable on production input, and deliberately not an error: the fee phase already
			// walked and phase 3 still will, so failing would discard real observations. It must not be
			// silent, though: an unwalked apply phase looks like a ledger where nothing happened.
			d.statsMu.Lock()
			d.entryMetaUnsupported++
			d.statsMu.Unlock()
		}
	}
	// ── Phase 3: the post-apply fee phase (P23 Soroban fee refunds) for every tx, same order. Empty
	// pre-P23 (the SDK populates PostTxApplyFeeChanges only from LCM V2). op_index -1, tx-level like
	// the fee phase.
	for i := range txs {
		dispatch := dispatchFor(entryChangeTxHash(&txs[i]))
		outputs = append(outputs, walkChangeSet(txs[i].PostTxApplyFeeChanges, -1, dispatch)...)
	}
	// ── Phase 4: the ledger's state-archival evictions. Not transaction-scoped (empty TxHash, OpIndex
	// -1) and last, because core evicts at ledger close after every transaction has applied.
	// See [walkEvictedKeys].
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

// walkEvictedKeys dispatches one synthetic Removed change per ledger key core EVICTED at close
// (CAP-62). An expired entry appears in no tx meta; unwalked, an evicted SAC balance would stand
// forever and served supply would drift ABOVE the truth. Removed is absorbing and a later Restored
// reverses it.
//
// The lake walker gives evicted keys the same positions but writes `removed` only for deleted
// entries (entry_walk_parity_test.go pins them together). Unreadable keys are counted and logged
// with the ledger number, since each dropped eviction overstates a balance and needs a replay.
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

// entryChangeTxHash is the hex tx hash stamped on entry-change contexts, computed identically in
// every phase.
func entryChangeTxHash(tx *ingest.LedgerTransaction) string {
	return hex.EncodeToString(tx.Result.TransactionHash[:])
}

// walkChangeSet dispatches each change at opIndex (-1 for tx-level / fee-meta blocks) in
// entrywalk.Canonical order, so IntraLedgerSeq does not depend on which export the ledger came from.
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

// bumpDecodeError increments the per-source decode-error counter under statsMu, when a matched
// decoder's Decode errors or panics; see bumpEventsSeen for the lazy map init.
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

// bumpUncorroborated increments, under statsMu, the per-source count of oracle-class calls dropped
// for lack of execution corroboration. It lazily initialises the map so a Dispatcher built
// without New() still counts rather than panicking.
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
	// Decoder-panic guard, installed AFTER the discovery hook so it covers Matches+Decode only;
	// see recordDecoderPanic.
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
		// Execution-corroboration gate: refuse a call the auth tree merely DECLARED, before Decode
		// reads its args as a price, and count it so forgery or a routing-shape change is visible.
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

// RouteContractCall is the test-harness entry point for contract-call decoders.
func (d *Dispatcher) RouteContractCall(ctx ContractCallContext) ([]consumer.Event, error) {
	return d.dispatchContractCall(ctx)
}

// dispatchEntryChange runs one [xdr.LedgerEntryChange] through the entry-decoder chain; the first
// matching decoder owns it. Mismatches are dropped uncounted: every tx produces several, so an
// unmatched counter would dominate the metric.
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

// RouteEntryChange is the test-harness entry point for LedgerEntryChange decoders (ADR-0021).
func (d *Dispatcher) RouteEntryChange(ctx LedgerEntryChangeContext) ([]consumer.Event, error) {
	return d.dispatchEntryChange(ctx)
}

// dispatchOp offers one operation to EVERY registered op decoder. Op decoders are per-domain
// observers (sdex trades, classic movements) whose op-type sets overlap on path payments, so
// first-match routing would drop one domain's rows. A decoder's error or panic costs only its own
// output for this op: it is counted per decoder and returned joined beside the others' outputs.
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

// RouteOp is the test-harness entry point for classic-op decoders.
func (d *Dispatcher) RouteOp(ctx OpContext) ([]consumer.Event, error) {
	return d.dispatchOp(ctx)
}

// Route feeds one event through the Matches/Decode chain for test harnesses and fixture replay; it errors only when the matching decoder fails.
func (d *Dispatcher) Route(ev events.Event) ([]consumer.Event, error) {
	return d.dispatchOne(ev)
}

// dispatchOne runs one event through the Matches/Decode chain; it errors only when the matching
// decoder fails, and unclaimed events are counted and dropped. Before decoding, the event goes to
// [discovery.Sniff], [discovery.SniffOracleEvent] and the [RawEventSink], so events a decoder
// rejects still reach discovered_assets and soroban_events.
func (d *Dispatcher) dispatchOne(ev events.Event) (outs []consumer.Event, err error) {
	if d.discoverySink != nil {
		if hit, ok := discovery.Sniff(ev); ok {
			d.discoverySink.Push(hit)
		}
		if hit, ok := discovery.SniffOracleEvent(ev); ok {
			d.discoverySink.Push(hit)
		}
	}
	// Raw-event capture (ADR-0029) runs BEFORE the decoders so events a decoder later rejects still
	// land in soroban_events. PushEvent back-pressures on a full buffer, slowing dispatch so the
	// producer's cursor never outruns durable writes.
	if d.rawEventSink != nil {
		d.rawEventSink.PushEvent(ev)
	}
	// Decoder-panic guard, installed AFTER the discovery and raw-event hooks: a fault in the lake
	// sink is NOT a decoder bug and keeps its ledger-level handling. See recordDecoderPanic.
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
	// Topic and Value are base64 ScVals matching stellar-rpc getEvents byte-for-byte: decoders
	// byte-equality-match on them, so any deviation would silently break topic routing.
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
	// CallPathContracts is the ordered ancestor+self contract chain (see
	// ContractCallContext.CallPathContracts); length >= 1 when non-nil, own ContractID last.
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

// authRootCall builds the invokeCall for an auth entry's root node, or nil for a create-contract
// root. Path and ancestors are nil on purpose: it is only compared against the top-level call.
func authRootCall(node *xdr.SorobanAuthorizedInvocation) *invokeCall {
	if node.Function.Type != xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeContractFn {
		return nil
	}
	ic := node.Function.MustContractFn()
	return buildInvokeCallFromArgs(&ic, nil, nil)
}

// walkAuthEntries flattens an op's auth entries, classifying EACH independently as the op's
// top-level call or a nested call to re-root under it. An all-or-nothing re-root breaks a co-signed
// tx whose entry 0 is the top-level call and entry 1 a deeper call: entries 1..n would export as
// the ENTRY POINT, under-reporting the wrapper on /v1/aggregators, and two calls could share the
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
				// No top-level call to root under (create-contract or unrenderable address): walk
				// the entries as their own roots.
				walkAuthEntry(&auth[j].RootInvocation, nil, nil, &calls)
				continue
			}
			// The auth roots are NESTED calls (the top-level needed no auth): re-root them under it so
			// only the real root carries CallPath [].
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

	// Every other entry is a separately-authorized subtree whose real sub-invocation index the auth
	// tree does not carry. Re-root them under the top-level call at indices PAST its own sub-invocation
	// space: no collision with a genuine sibling path, and they read as depth>=1, not the entry point.
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

// containsCall reports whether calls already holds the same invocation as c (same contract,
// function, argument encoding). Identity is deliberately NOT CallPath: the question is whether the
// auth walk already emitted the top-level call under some path, since emitting it twice would double
// every downstream decode.
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

// extractInvokeContractCallTrees returns, parallel to ops, every call reachable from each op: the
// top-level invocation plus every nested call in its auth tree (nil for other ops). It is the
// ContractCallDecoder routing source; [extractInvokeContractCalls] is top-level only, for OpArgs.
//
// The top-level call is always emitted with CallPath == []. An auth entry's root is the subtree
// NEEDING authorization, often a nested call, so when no walked call matches the top level it is
// prepended and entry j is re-rooted at [j]: the auth-entry ordinal, stable across re-derives and
// not claiming to be the root. Dedup on (contract, function, args) keeps a top-level call that is
// its own auth root from emitting twice (a duplicate trade row). Cross-entry duplicates are left to
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
			// No auth array: only the top-level call is recorded.
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
		// ContractAddress may be account-typed in rare composed calls; Band's real target is always a
		// ScAddressTypeContract, so the address-kind switch below handles both defensively.
		contractStrkey := ""
		switch ic.ContractAddress.Type {
		case xdr.ScAddressTypeScAddressTypeContract:
			cid := ic.ContractAddress.MustContractId()
			if s, err := contractIDToStrkey(cid); err == nil {
				contractStrkey = s
			}
		case xdr.ScAddressTypeScAddressTypeAccount:
			// An account address is invalid at the protocol level; skip it, not emit a bad strkey.
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
				// Marshal failure on a local ScVal means a broken envelope or SDK drift. Surface as
				// "no args"; decoders that require them (Redstone, Band) report their own error.
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

// accountIDToStrkey encodes an xdr.AccountId as a G-strkey, or "" if it is not Ed25519.
func accountIDToStrkey(aid xdr.AccountId) (string, error) {
	if aid.Type != xdr.PublicKeyTypePublicKeyTypeEd25519 {
		return "", fmt.Errorf("accountIDToStrkey: unsupported account type %d", aid.Type)
	}
	pub := aid.Ed25519
	return strkey.Encode(strkey.VersionByteAccountID, pub[:])
}

// mustParseRFC3339 parses a closedAt string the dispatcher itself formatted, so a failure is a
// programming bug and panics.
func mustParseRFC3339(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic("dispatcher: malformed closedAt (self-generated): " + err.Error())
	}
	return t
}
