package timescale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// AquariusAdminKind discriminates the eight governance/upgrade admin
// event kinds (migration 0100). String values match the
// aquarius_admin.event_kind CHECK constraint and the AdminAction
// constants in internal/sources/aquarius/consumer.go.
type AquariusAdminKind string

// Governance / upgrade admin event kinds — see
// internal/sources/aquarius/README.md (ROADMAP #89) for the per-kind
// lifetime counts + wire-shape citations.
const (
	AquariusAdminApplyUpgrade            AquariusAdminKind = "apply_upgrade"
	AquariusAdminCommitUpgrade           AquariusAdminKind = "commit_upgrade"
	AquariusAdminSetPrivilegedAddrs      AquariusAdminKind = "set_privileged_addrs"
	AquariusAdminApplyTransferOwnership  AquariusAdminKind = "apply_transfer_ownership"
	AquariusAdminCommitTransferOwnership AquariusAdminKind = "commit_transfer_ownership"
	AquariusAdminEnableEmergencyMode     AquariusAdminKind = "enable_emergency_mode"
	AquariusAdminDisableEmergencyMode    AquariusAdminKind = "disable_emergency_mode"
	AquariusAdminPoolGaugeSwitchToken    AquariusAdminKind = "pool_gauge_switch_token"
)

// IsValid reports whether k is one of the eight known governance/
// upgrade kinds. Mirrors the CHECK constraint in migration 0100.
func (k AquariusAdminKind) IsValid() bool {
	switch k {
	case AquariusAdminApplyUpgrade, AquariusAdminCommitUpgrade, AquariusAdminSetPrivilegedAddrs,
		AquariusAdminApplyTransferOwnership, AquariusAdminCommitTransferOwnership,
		AquariusAdminEnableEmergencyMode, AquariusAdminDisableEmergencyMode,
		AquariusAdminPoolGaugeSwitchToken:
		return true
	}
	return false
}

// AquariusAdminEvent is one observed router/pool governance event
// (any of the eight kinds). Admin / Target are universal promoted
// columns, NULL when the kind doesn't carry them; Attributes holds
// the kind-specific remainder.
type AquariusAdminEvent struct {
	ContractID      string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	EventIndex      uint32
	Kind            AquariusAdminKind
	Admin           string // "" when the kind carries none
	Target          string // "" when the kind carries none
	Attributes      map[string]any
}

// InsertAquariusAdminEvent appends one governance/upgrade admin event
// to aquarius_admin. Idempotent on the (ledger_close_time,
// contract_id, ledger, tx_hash, op_index, event_kind, event_index) PK.
func (s *Store) InsertAquariusAdminEvent(ctx context.Context, e AquariusAdminEvent) error {
	if e.ContractID == "" {
		return errors.New("timescale: InsertAquariusAdminEvent: ContractID is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertAquariusAdminEvent: TxHash is empty")
	}
	if !e.Kind.IsValid() {
		return fmt.Errorf("timescale: InsertAquariusAdminEvent: invalid Kind %q", e.Kind)
	}

	attrs := e.Attributes
	if attrs == nil {
		attrs = map[string]any{}
	}
	attrsJSON, err := json.Marshal(attrs)
	if err != nil {
		return fmt.Errorf("timescale: InsertAquariusAdminEvent: marshal attributes: %w", err)
	}

	const q = `
        INSERT INTO aquarius_admin (
            contract_id, ledger, ledger_close_time, tx_hash,
            op_index, event_index, event_kind, admin, target,
            attributes
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
        )
        ON CONFLICT (ledger_close_time, contract_id, ledger, tx_hash,
                     op_index, event_kind, event_index) DO NOTHING
    `
	_, err = s.db.ExecContext(ctx, q,
		e.ContractID, int(e.Ledger), e.LedgerCloseTime.UTC(), e.TxHash,
		int(e.OpIndex), int(e.EventIndex), string(e.Kind),
		nullString(e.Admin), nullString(e.Target),
		attrsJSON,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertAquariusAdminEvent %s@%d: %w", e.ContractID, e.Ledger, err)
	}
	return nil
}
