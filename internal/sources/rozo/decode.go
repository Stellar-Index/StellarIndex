package rozo

import (
	"errors"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// ErrUnknownEvent flags an event whose topic[0] matches neither v1 Payment topic, a double-check since symbol_short! has no protocol namespace and another protocol can collide on the same symbol bytes.
var ErrUnknownEvent = errors.New("rozo: unknown event topic")

// ErrMalformedBody flags a Payment / Flush event whose ScMap body lacks the contract's field names ({ from, destination, amount, memo } / { token, destination, amount }), so a mismatch surfaces instead of a half-populated event.
var ErrMalformedBody = errors.New("rozo: malformed event body")

// Classify reports which v1 event e is, or "" if the topic does not match; contract-id filtering happens DOWNSTREAM (a topic symbol can collide with any contract), so the consumer must drop events whose ContractID is not a known Rozo contract before decoding.
func Classify(e *events.Event) string {
	if len(e.Topic) < 1 {
		return ""
	}
	switch e.Topic[0] {
	case TopicSymbolPayment, TopicSymbolPaymentEvent:
		// The deployed contract emits the long form "payment_event"; a short-form-only match
		// would never fire live and would leave rozo_events empty.
		return EventPayment
	case TopicSymbolFlush, TopicSymbolFlushEvent:
		return EventFlush
	}
	return ""
}

// DecodePayment turns one PaymentEvent-shaped Event into a canonical Payment from the ScMap { amount: I128, destination, from, memo: String }, looked up by name with scval.MustMapField; Memo is in scval.ToText form (literal when valid NUL-free UTF-8, else `\x` + hex) and is the only free-form, contract-chosen value.
func DecodePayment(e *events.Event) (Payment, error) {
	body, err := scval.Parse(e.Value)
	if err != nil {
		return Payment{}, fmt.Errorf("rozo: payment body parse: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return Payment{}, fmt.Errorf("rozo: payment body not a map: %w", err)
	}

	fromSV, err := scval.MustMapField(entries, "from")
	if err != nil {
		return Payment{}, fmt.Errorf("%w: missing 'from': %w", ErrMalformedBody, err)
	}
	from, err := scval.AsAddressStrkey(fromSV)
	if err != nil {
		return Payment{}, fmt.Errorf("rozo: payment 'from' address: %w", err)
	}

	destSV, err := scval.MustMapField(entries, "destination")
	if err != nil {
		return Payment{}, fmt.Errorf("%w: missing 'destination': %w", ErrMalformedBody, err)
	}
	destination, err := scval.AsAddressStrkey(destSV)
	if err != nil {
		return Payment{}, fmt.Errorf("rozo: payment 'destination' address: %w", err)
	}

	amountSV, err := scval.MustMapField(entries, "amount")
	if err != nil {
		return Payment{}, fmt.Errorf("%w: missing 'amount': %w", ErrMalformedBody, err)
	}
	amount, err := scval.AsAmountFromI128(amountSV)
	if err != nil {
		return Payment{}, fmt.Errorf("rozo: payment 'amount' i128: %w", err)
	}

	memoSV, err := scval.MustMapField(entries, "memo")
	if err != nil {
		return Payment{}, fmt.Errorf("%w: missing 'memo': %w", ErrMalformedBody, err)
	}
	// AsText, not AsString: the memo is the payer's bytes bound to a `text` column, and Postgres
	// refuses a NUL or invalid UTF-8 (SQLSTATE 22021) on every attempt, so a one-stroop pay()
	// would lose the row for good. AsText keeps every byte (see scval.ToText).
	memo, err := scval.AsText(memoSV)
	if err != nil {
		return Payment{}, fmt.Errorf("rozo: payment 'memo' string: %w", err)
	}

	return Payment{
		Ledger:      e.Ledger,
		TxHash:      e.TxHash,
		OpIndex:     e.OperationIndex,
		ClosedAt:    e.LedgerClosedAt,
		ContractID:  e.ContractID,
		From:        from,
		Destination: destination,
		Amount:      amount.String(),
		Memo:        memo,
	}, nil
}

// DecodeFlush turns one FlushEvent-shaped Event into a canonical Flush from the ScMap { amount: I128, destination: Address, token: Address }.
func DecodeFlush(e *events.Event) (Flush, error) {
	body, err := scval.Parse(e.Value)
	if err != nil {
		return Flush{}, fmt.Errorf("rozo: flush body parse: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return Flush{}, fmt.Errorf("rozo: flush body not a map: %w", err)
	}

	tokenSV, err := scval.MustMapField(entries, "token")
	if err != nil {
		return Flush{}, fmt.Errorf("%w: missing 'token': %w", ErrMalformedBody, err)
	}
	token, err := scval.AsAddressStrkey(tokenSV)
	if err != nil {
		return Flush{}, fmt.Errorf("rozo: flush 'token' address: %w", err)
	}

	destSV, err := scval.MustMapField(entries, "destination")
	if err != nil {
		return Flush{}, fmt.Errorf("%w: missing 'destination': %w", ErrMalformedBody, err)
	}
	destination, err := scval.AsAddressStrkey(destSV)
	if err != nil {
		return Flush{}, fmt.Errorf("rozo: flush 'destination' address: %w", err)
	}

	amountSV, err := scval.MustMapField(entries, "amount")
	if err != nil {
		return Flush{}, fmt.Errorf("%w: missing 'amount': %w", ErrMalformedBody, err)
	}
	amount, err := scval.AsAmountFromI128(amountSV)
	if err != nil {
		return Flush{}, fmt.Errorf("rozo: flush 'amount' i128: %w", err)
	}

	return Flush{
		Ledger:      e.Ledger,
		TxHash:      e.TxHash,
		OpIndex:     e.OperationIndex,
		ClosedAt:    e.LedgerClosedAt,
		ContractID:  e.ContractID,
		Token:       token,
		Destination: destination,
		Amount:      amount.String(),
	}, nil
}
