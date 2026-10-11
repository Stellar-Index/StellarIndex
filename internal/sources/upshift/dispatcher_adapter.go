package upshift

import (
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Decoder is the dispatcher-facing view of the Upshift vaults. One
// instance per indexer; no goroutines, no polling, no correlation
// buffer — every decoded body is self-contained, so there is no orphan
// class here.
//
// It owns one piece of state: the contract-identity gate (ADR-0035).
// Unlike the factory-anchored sources there is nothing to fan out from
// — neither vault has a creation event in the lake, each one's first
// event being its own `admin_set` — so this is the ADR-0040 CURATED-SET
// mechanism, the shape internal/sources/comet uses. The trust root is
// the in-code [MainnetGatedSet]; caller options layer the
// `protocol_contracts` DB warm on top (contractid.WithSeed), which is
// the operator seam for admitting a new vault without a redeploy.
// There are no factories, so [contractid.Registry.Seed] never fires
// from a decode.
type Decoder struct {
	reg *contractid.Registry
}

// NewDecoder constructs an Upshift Decoder. The curated vault set is
// always installed first; opts layer the DB warm on top.
func NewDecoder(opts ...contractid.Option) *Decoder {
	base := []contractid.Option{contractid.WithSeed(MainnetGatedSet())}
	return &Decoder{reg: contractid.New(append(base, opts...)...)}
}

// Name implements [dispatcher.Decoder].
func (*Decoder) Name() string { return SourceName }

// GatedContractSet returns every contract whose events Matches can
// accept — the tightest sound contract-id prefilter for a lake read
// over this decoder. The projector reads it every cycle as Source.ContractIDsFunc.
//
// It is not an optimisation here, it is what makes a catch-up window
// affordable: this source's own symbols are `deposit`, `withdraw` and
// `transfer`, three of the most common on pubnet, so the topic-exclusion
// prefilter the other Soroban sources use would have to exclude this
// source's own events to be worth anything. Scoping to two contracts is
// both cheaper and lossless.
func (d *Decoder) GatedContractSet() []string { return d.reg.GatedSet() }

// Matches implements [dispatcher.Decoder]. Gates on CONTRACT IDENTITY,
// never on topic bytes alone (ADR-0035).
//
// `deposit`, `withdraw` and `transfer` are generic Soroban symbols with no
// protocol namespace: a bounded 20,000-ledger pubnet census
// (63,000,000-63,020,000) found four distinct contracts emitting `deposit`,
// the vaults a minority of them. A topic-only decoder would record strangers'
// vault flows under this source, and corrupt the ADR-0033 re-derive (served
// table and re-derived expectation would agree over polluted data).
//
// Coverage trade (ADR-0035): an un-admitted real vault's events are DROPPED, so
// [MainnetVaults] completeness is load-bearing, held by the bespoke-symbol
// sweep in the package doc plus the `protocol_contracts` warm. A missed vault
// fails CLOSED into a visible recognition gap (ADR-0033 Claim 2a), never a
// silent mis-attribution.
func (d *Decoder) Matches(ev events.Event) bool {
	if classify(&ev) == "" {
		return false
	}
	return d.reg.Has(ev.ContractID)
}

// Decode implements [dispatcher.Decoder]. Returns exactly one
// consumer.Event for the four decoded kinds and ZERO events for the
// ten recognized-but-undecoded ones.
//
// Zero rows with NO error is the deliberate ADR-0033 signal: those
// ten are real, gated, fully classified events with no serveable
// state of their own (see the package doc for why each is skipped), so
// the re-derive must count their ledgers as expected-zero. Returning an
// error instead would mark the ledger blind and hold this source's
// completeness verdict open forever — the do-nothing re-derive
// trap that kept `comet` permanently incomplete. The error path stays
// reserved for INDETERMINATE parse failures, which must remain blind.
//
// Decode itself is shape-only: the identity gate lives in Matches,
// which the dispatcher consults first. Direct Decode callers (tests,
// fixture tooling) bypass the gate by design.
func (d *Decoder) Decode(ev events.Event) ([]consumer.Event, error) {
	kind := classify(&ev)
	switch kind {
	case "":
		return nil, ErrNotUpshiftEvent
	case EventDeposit, EventWithdraw:
		out, err := decodeFlow(&ev, kind)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil
	case EventTransfer:
		out, err := decodeTransfer(&ev)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil
	case EventDeployedAssetsChanged:
		out, err := decodeDeployedAssets(&ev)
		if err != nil {
			return nil, err
		}
		return []consumer.Event{out}, nil
	default:
		// Recognized custody / governance / allowance event. Gated,
		// classified, projected as zero rows.
		return nil, nil
	}
}
