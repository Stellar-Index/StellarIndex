package timescale

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// CCTPEventType discriminates the 26 Circle CCTP v2 event variants.
// String values match the cctp_events.event_type CHECK constraint
// (migration 0038, extended by 0070, 0092 and 0094) and
// internal/sources/cctp's event-name constants. LESSON (board #31,
// v0.7.0→v0.7.1): the type is gated in THREE layers — the decoder's
// Classify, this enum's IsValid, and the SQL CHECK. Adding an event
// means all three, or rows are rejected at whichever layer was
// missed. Re-confirmed 2026-07-08 (ROADMAP #89b) when the first 5
// governance/admin events were added, and again 2026-07-09 (#89c)
// closing the full topic census: this file is easy to miss because
// InsertCCTPEvent's defensive IsValid rejection fires INSIDE the Go
// binary, before the SQL CHECK ever sees the row — a mismatch here
// silently drops every governance event at insert time even though
// decode + dispatch work.
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

	// Lower-signal admin/governance events — ROADMAP #89c, 2026-07-09.
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

// CCTPEvent is one cctp_events row — a single observed Circle CCTP v2
// contract event on Stellar. Mirrors the migration-0038 columns.
//
// Amount / Fee / Token are decimal-or-strkey strings; the empty
// string means "this event type carries no such field" and writes
// SQL NULL. CounterpartyDomain is a *uint32 for the same reason
// (message_sent / mint_and_withdraw carry no domain). Attributes
// holds the event-type-specific remainder as a jsonb blob.
type CCTPEvent struct {
	ContractID         string
	Ledger             uint32
	TxHash             string
	OpIndex            uint32
	ObservedAt         time.Time
	EventType          CCTPEventType
	Amount             string // decimal i128; "" → NULL
	Fee                string // decimal i128; "" → NULL
	Token              string // Stellar Address strkey; "" → NULL
	CounterpartyDomain *uint32
	Attributes         map[string]any
}

// InsertCCTPEvent appends one CCTP event row, idempotent on the
// (contract_id, ledger, tx_hash, op_index, event_type, ts) PK.
// Re-running the indexer or a backfill over the same range writes
// the same rows; ON CONFLICT DO NOTHING makes the replay a no-op.
//
// Defensive: rejects empty ContractID / TxHash and an invalid
// EventType before touching the DB. Amount / Fee are passed straight
// to the NUMERIC columns as decimal strings — a malformed value (the
// decoder should never produce one) surfaces as a DB error here
// rather than being silently coerced.
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

	// INV-3 generation-guarded corrective upsert (migration 0110): a
	// corrected re-derive of the i128 amounts (amount / fee) or the other
	// decoded columns lands in place when its generation is >= the stored
	// one; a live gen-0 replay can never revert it. Replaces the old DO NOTHING.
	const q = `
        INSERT INTO cctp_events (
            contract_id, ledger, tx_hash, op_index, ts,
            event_type, amount, fee, token, counterparty_domain,
            attributes, derive_generation
        ) VALUES (
            $1, $2, $3, $4, $5,
            $6, $7, $8, $9, $10,
            $11, $12
        )
        ON CONFLICT (contract_id, ledger, tx_hash, op_index, event_type, ts) DO UPDATE SET
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
		attrs, s.deriveGeneration,
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
