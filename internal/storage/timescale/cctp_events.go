package timescale

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// CCTPEventType discriminates the 26 Circle CCTP v2 event variants. Values match the
// cctp_events.event_type CHECK constraint and internal/sources/cctp's event-name constants.
// A new event must be added in all three gates (the decoder's Classify, this enum's IsValid, the
// SQL CHECK). This one is easy to miss: InsertCCTPEvent rejects on IsValid before the SQL CHECK
// runs, so a mismatch silently drops every event of the new type even though decode works.
type CCTPEventType string

const (
	CCTPDepositForBurn             CCTPEventType = "deposit_for_burn"
	CCTPMintAndWithdraw            CCTPEventType = "mint_and_withdraw"
	CCTPMessageSent                CCTPEventType = "message_sent"
	CCTPMessageReceived            CCTPEventType = "message_received"
	CCTPMintAndForward             CCTPEventType = "mint_and_forward"
	CCTPOwnershipTransfer          CCTPEventType = "ownership_transfer"
	CCTPOwnershipTransferCompleted CCTPEventType = "ownership_transfer_completed"
	CCTPAdminChanged               CCTPEventType = "admin_changed"
	CCTPRemoteTokenMessengerAdded  CCTPEventType = "remote_token_messenger_added"
	CCTPTokenPairLinked            CCTPEventType = "token_pair_linked"

	// Lower-signal admin/governance events.
	CCTPAdminChangeStarted        CCTPEventType = "admin_change_started"
	CCTPAttesterEnabled           CCTPEventType = "attester_enabled"
	CCTPAttesterManagerUpdated    CCTPEventType = "attester_manager_updated"
	CCTPDenylisted                CCTPEventType = "denylisted"
	CCTPDenylisterChanged         CCTPEventType = "denylister_changed"
	CCTPFeeRecipientSet           CCTPEventType = "fee_recipient_set"
	CCTPMaxMessageBodySizeUpdated CCTPEventType = "max_message_body_size_updated"
	CCTPMinFeeControllerSet       CCTPEventType = "min_fee_controller_set"
	CCTPPauserChanged             CCTPEventType = "pauser_changed"
	CCTPRescuerChanged            CCTPEventType = "rescuer_changed"
	CCTPSetBurnLimitPerMessage    CCTPEventType = "set_burn_limit_per_message"
	CCTPSetTokenController        CCTPEventType = "set_token_controller"
	CCTPSignatureThresholdUpdated CCTPEventType = "signature_threshold_updated"
	CCTPSwapMinterConfigSet       CCTPEventType = "swap_minter_config_set"
	CCTPTokenDecimalConfigAdded   CCTPEventType = "token_decimal_config_added"
	CCTPUnDenylisted              CCTPEventType = "un_denylisted"
)

// IsValid reports whether t is one of the 26 known CCTP events.
func (t CCTPEventType) IsValid() bool {
	switch t {
	case CCTPDepositForBurn, CCTPMintAndWithdraw, CCTPMessageSent, CCTPMessageReceived, CCTPMintAndForward,
		CCTPOwnershipTransfer, CCTPOwnershipTransferCompleted, CCTPAdminChanged,
		CCTPRemoteTokenMessengerAdded, CCTPTokenPairLinked,
		CCTPAdminChangeStarted, CCTPAttesterEnabled, CCTPAttesterManagerUpdated,
		CCTPDenylisted, CCTPDenylisterChanged, CCTPFeeRecipientSet,
		CCTPMaxMessageBodySizeUpdated, CCTPMinFeeControllerSet, CCTPPauserChanged,
		CCTPRescuerChanged, CCTPSetBurnLimitPerMessage, CCTPSetTokenController,
		CCTPSignatureThresholdUpdated, CCTPSwapMinterConfigSet, CCTPTokenDecimalConfigAdded,
		CCTPUnDenylisted:
		return true
	}
	return false
}

// CCTPEvent is one cctp_events row: a single observed Circle CCTP v2 contract event on Stellar.
//
// Amount / Fee / Token are decimal-or-strkey strings; the empty string means the event type
// carries no such field and writes SQL NULL. CounterpartyDomain is a *uint32 for the same reason.
// Attributes holds the event-type-specific remainder as jsonb.
type CCTPEvent struct {
	ContractID string
	Ledger     uint32
	TxHash     string
	OpIndex    uint32
	// EventIndex is the position of this event within its operation's
	// contract-event list — the migration-0112 PK discriminator that keeps
	// two same-type events emitted by one op from collapsing.
	EventIndex         uint32
	ObservedAt         time.Time
	EventType          CCTPEventType
	Amount             string // decimal i128; "" → NULL
	Fee                string // decimal i128; "" → NULL
	Token              string // Stellar Address strkey; "" → NULL
	CounterpartyDomain *uint32
	Attributes         map[string]any
}

// InsertCCTPEvent appends one CCTP event row. Idempotent in row count on the table PK, but not
// inert in value: the upsert is guarded by `derive_generation <= EXCLUDED.derive_generation`, so a
// replay at an equal-or-higher generation overwrites stored values (letting a re-derive repair a
// wrong row) and a lower one is refused.
//
// Rejects empty ContractID/TxHash and an invalid EventType up front. Amount / Fee go to the NUMERIC
// columns as decimal strings, so a malformed value surfaces as a DB error instead of being coerced.
func (s *Store) InsertCCTPEvent(ctx context.Context, e CCTPEvent) error {
	if e.ContractID == "" {
		return errors.New("timescale: InsertCCTPEvent: ContractID is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertCCTPEvent: TxHash is empty")
	}
	if !e.EventType.IsValid() {
		return fmt.Errorf("timescale: InsertCCTPEvent: invalid EventType %q", e.EventType)
	}

	attrs := []byte("{}")
	if len(e.Attributes) > 0 {
		marshaled, err := json.Marshal(e.Attributes)
		if err != nil {
			return fmt.Errorf("timescale: InsertCCTPEvent: marshal attributes: %w", err)
		}
		attrs = marshaled
	}

	// Generation-guarded upsert (migration 0110): a corrected re-derive of the i128 amounts or
	// other decoded columns lands when its generation is >= the stored one; a live gen-0 replay
	// never reverts it. event_index ($13) is in the ON CONFLICT target (migration 0112) because two
	// same-type events from one op differ only by it, and the second row would otherwise collapse
	// onto the first.
	const q = `
        INSERT INTO cctp_events (
            contract_id, ledger, tx_hash, op_index, ts,
            event_type, amount, fee, token, counterparty_domain,
            attributes, derive_generation, event_index
        ) VALUES (
            $1, $2, $3, $4, $5,
            $6, $7, $8, $9, $10,
            $11, $12, $13
        )
        ON CONFLICT (contract_id, ledger, tx_hash, op_index, event_type, event_index, ts) DO UPDATE SET
            amount              = EXCLUDED.amount,
            fee                 = EXCLUDED.fee,
            token               = EXCLUDED.token,
            counterparty_domain = EXCLUDED.counterparty_domain,
            attributes          = EXCLUDED.attributes,
            derive_generation   = EXCLUDED.derive_generation
          WHERE cctp_events.derive_generation <= EXCLUDED.derive_generation
    `
	var (
		domain sql.NullInt64
		token  sql.NullString
	)
	if e.CounterpartyDomain != nil {
		domain = sql.NullInt64{Int64: int64(*e.CounterpartyDomain), Valid: true}
	}
	if e.Token != "" {
		token = sql.NullString{String: e.Token, Valid: true}
	}
	_, err := s.db.ExecContext(ctx, q,
		e.ContractID, int(e.Ledger), e.TxHash, int(e.OpIndex), e.ObservedAt.UTC(),
		string(e.EventType), nullNumeric(e.Amount), nullNumeric(e.Fee), token, domain,
		attrs, s.deriveGeneration, int(e.EventIndex),
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertCCTPEvent %s@%d: %w", e.ContractID, e.Ledger, err)
	}
	return nil
}

// nullNumeric maps an empty string to SQL NULL and any other value to
// a string the postgres driver hands to a NUMERIC column verbatim.
func nullNumeric(v string) sql.NullString {
	if v == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}
