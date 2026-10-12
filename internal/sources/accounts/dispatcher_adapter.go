package accounts

import (
	"errors"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// Observer is the dispatcher-facing AccountEntry observer (ADR-0021), a [dispatcher.LedgerEntryChangeDecoder] firing only on G-strkeys in the operator-configured watched set (see [NewObserver]); a watch-every-account mode would be a table-size decision needing its own ADR.
type Observer struct {
	// watched maps G-strkey → true. Map lookup is O(1) per change;
	// the watched set is typically small (single-digit accounts at
	// v1).
	watched map[string]struct{}
}

// ErrEmptyWatchSet is returned by [NewObserver] for an empty list: an observer with nothing to watch is a configuration error; operators who don't want it should not register it.
var ErrEmptyWatchSet = errors.New("accounts: cannot construct Observer with empty watched-account list")

// NewObserver constructs an [Observer] watching the G-strkey list; entries are deduplicated and empty strings rejected to surface mistyped TOML before live ingest.
func NewObserver(watched []string) (*Observer, error) {
	if len(watched) == 0 {
		return nil, ErrEmptyWatchSet
	}
	set := make(map[string]struct{}, len(watched))
	for _, acc := range watched {
		if acc == "" {
			return nil, errors.New("accounts: empty G-strkey in watched-account list")
		}
		set[acc] = struct{}{}
	}
	return &Observer{watched: set}, nil
}

// Name implements [dispatcher.LedgerEntryChangeDecoder].
func (*Observer) Name() string { return SourceName }

// Matches implements [dispatcher.LedgerEntryChangeDecoder]: true only for an AccountEntry change whose AccountId is watched; both checks are cheap, extraction is left to Decode.
func (o *Observer) Matches(change xdr.LedgerEntryChange) bool {
	id, err := accountIDFromChange(change)
	if err != nil {
		return false
	}
	_, watched := o.watched[id]
	return watched
}

// Decode implements [dispatcher.LedgerEntryChangeDecoder], emitting exactly one [Observation] per matched change.
func (o *Observer) Decode(ctx dispatcher.LedgerEntryChangeContext) ([]consumer.Event, error) {
	obs, err := extractObservation(ctx.Change)
	if err != nil {
		return nil, err
	}
	obs.Ledger = ctx.Ledger
	obs.ObservedAt = ctx.ClosedAt
	obs.IntraLedgerSeq = ctx.IntraLedgerSeq
	return []consumer.Event{obs}, nil
}

// Compile-time check.
var _ dispatcher.LedgerEntryChangeDecoder = (*Observer)(nil)
