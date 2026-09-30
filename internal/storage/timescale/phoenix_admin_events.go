package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// validPhoenixAdminActions is the closed set the admin_action CHECK
// enforces (migrations 0132, 0192).
var validPhoenixAdminActions = map[string]bool{
	"replace_requested": true, "replace_set": true,
	"undo": true, "accepted": true,
	"factory_config_updated": true, "blend_set_delegate": true,
	"blend_set_min_trading_a": true, "blend_set_min_trading_b": true,
}

// PhoenixAdminEvent is one observed Phoenix admin/config event
// (migrations 0132, 0192). Admin is the address the body carries when
// present, empty otherwise (stored NULL).
type PhoenixAdminEvent struct {
	Pool            string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	EventIndex      uint32
	AdminAction     string
	Admin           string // "" → NULL
	Value           string // decimal i128 of blend_set_min_trading_*; "" → NULL
}

// InsertPhoenixAdmin lands one admin-rotation event, idempotent on the
// (ledger_close_time, pool, ledger, tx_hash, op_index, event_index) PK
// with the INV-3 generation-guarded corrective upsert (migration 0110).
//
// Defensive: rejects an empty Pool / TxHash, a zero LedgerCloseTime, and
// an AdminAction outside the closed set.
func (s *Store) InsertPhoenixAdmin(ctx context.Context, e PhoenixAdminEvent) error {
	if e.Pool == "" {
		return errors.New("timescale: InsertPhoenixAdmin: Pool is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertPhoenixAdmin: TxHash is empty")
	}
	if e.LedgerCloseTime.IsZero() {
		return fmt.Errorf("timescale: InsertPhoenixAdmin: zero LedgerCloseTime (pool=%s ledger=%d)", e.Pool, e.Ledger)
	}
	if !validPhoenixAdminActions[e.AdminAction] {
		return fmt.Errorf("timescale: InsertPhoenixAdmin: unknown AdminAction %q", e.AdminAction)
	}

	var admin sql.NullString
	if e.Admin != "" {
		admin = sql.NullString{String: e.Admin, Valid: true}
	}

	const q = `
        INSERT INTO phoenix_admin_events (
            pool, ledger, ledger_close_time, tx_hash,
            op_index, event_index, admin_action, admin, value, derive_generation
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
        )
        ON CONFLICT (ledger_close_time, pool, ledger, tx_hash, op_index, event_index) DO UPDATE SET
            admin_action      = EXCLUDED.admin_action,
            admin             = EXCLUDED.admin,
            value             = EXCLUDED.value,
            derive_generation = EXCLUDED.derive_generation
          WHERE phoenix_admin_events.derive_generation <= EXCLUDED.derive_generation
    `
	if _, err := s.db.ExecContext(ctx, q,
		e.Pool, int(e.Ledger), e.LedgerCloseTime.UTC(), e.TxHash,
		int(e.OpIndex), int(e.EventIndex), e.AdminAction, admin, nullNumeric(e.Value),
		s.deriveGeneration,
	); err != nil {
		return fmt.Errorf("timescale: InsertPhoenixAdmin %s@%d: %w", e.Pool, e.Ledger, err)
	}
	return nil
}
