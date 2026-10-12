package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// UpshiftVaultEventKind discriminates the four decoded Upshift vault events.
type UpshiftVaultEventKind string

const (
	UpshiftDeposit               UpshiftVaultEventKind = "deposit"
	UpshiftWithdraw              UpshiftVaultEventKind = "withdraw"
	UpshiftTransfer              UpshiftVaultEventKind = "transfer"
	UpshiftDeployedAssetsChanged UpshiftVaultEventKind = "deployed_assets_changed"
)

// IsValid reports whether k is one of the four known kinds.
func (k UpshiftVaultEventKind) IsValid() bool {
	switch k {
	case UpshiftDeposit, UpshiftWithdraw, UpshiftTransfer, UpshiftDeployedAssetsChanged:
		return true
	}
	return false
}

// isFlow reports whether k is a deposit or withdraw, the kinds carrying the full (caller, receiver, owner) triple and both amounts.
func (k UpshiftVaultEventKind) isFlow() bool {
	return k == UpshiftDeposit || k == UpshiftWithdraw
}

// UpshiftVaultEvent is one upshift_vault_events row; fields not meaningful for Kind are written as SQL NULL, not zero, because a real
// zero amount and an absent one are different facts.
type UpshiftVaultEvent struct {
	ContractID      string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	// EventIndex is load-bearing in the PK: one operation emits the
	// underlying SAC transfer AND the vault event, and a redemption
	// emits several vault events on a single op.
	EventIndex uint32

	Kind     UpshiftVaultEventKind
	Caller   string
	Receiver string // deposit / withdraw / transfer
	Owner    string // deposit / withdraw

	Assets canonical.Amount // deposit / withdraw
	Shares canonical.Amount // deposit / withdraw / transfer

	OldAmount canonical.Amount // deployed_assets_changed
	NewAmount canonical.Amount // deployed_assets_changed
}

// InsertUpshiftVaultEvent appends one Upshift vault event row, idempotent on the PK via an upsert guarded by derive_generation <=
// EXCLUDED.derive_generation, so an equal-or-higher replay corrects it.
func (s *Store) InsertUpshiftVaultEvent(ctx context.Context, e UpshiftVaultEvent) error {
	if e.ContractID == "" {
		return errors.New("timescale: InsertUpshiftVaultEvent: ContractID is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertUpshiftVaultEvent: TxHash is empty")
	}
	if e.Caller == "" {
		return errors.New("timescale: InsertUpshiftVaultEvent: Caller is empty")
	}
	if !e.Kind.IsValid() {
		return fmt.Errorf("timescale: InsertUpshiftVaultEvent: invalid Kind %q", e.Kind)
	}

	var (
		receiver, owner                      sql.NullString
		assets, shares, oldAmount, newAmount sql.NullString
	)
	switch {
	case e.Kind.isFlow():
		if e.Receiver == "" || e.Owner == "" {
			return fmt.Errorf("timescale: InsertUpshiftVaultEvent: %s needs Receiver and Owner", e.Kind)
		}
		if e.Assets.Sign() < 0 || e.Shares.Sign() < 0 {
			return fmt.Errorf("timescale: InsertUpshiftVaultEvent: %s amounts must be >= 0 (assets %s, shares %s)",
				e.Kind, e.Assets, e.Shares)
		}
		receiver = sql.NullString{String: e.Receiver, Valid: true}
		owner = sql.NullString{String: e.Owner, Valid: true}
		assets = sql.NullString{String: e.Assets.String(), Valid: true}
		shares = sql.NullString{String: e.Shares.String(), Valid: true}
	case e.Kind == UpshiftTransfer:
		if e.Receiver == "" {
			return errors.New("timescale: InsertUpshiftVaultEvent: transfer needs Receiver")
		}
		if e.Shares.Sign() < 0 {
			return fmt.Errorf("timescale: InsertUpshiftVaultEvent: transfer Shares must be >= 0 (got %s)", e.Shares)
		}
		receiver = sql.NullString{String: e.Receiver, Valid: true}
		shares = sql.NullString{String: e.Shares.String(), Valid: true}
	default: // deployed_assets_changed
		oldAmount = sql.NullString{String: e.OldAmount.String(), Valid: true}
		newAmount = sql.NullString{String: e.NewAmount.String(), Valid: true}
	}

	const q = `
        INSERT INTO upshift_vault_events (
            contract_id, ledger, ledger_close_time, tx_hash, op_index, event_index,
            event_kind, caller, receiver, owner,
            assets, shares, old_amount, new_amount,
            derive_generation
        ) VALUES (
            $1, $2, $3, $4, $5, $6,
            $7, $8, $9, $10,
            $11, $12, $13, $14,
            $15
        )
        ON CONFLICT (ledger_close_time, contract_id, ledger, tx_hash,
                     op_index, event_index) DO UPDATE SET
            event_kind        = EXCLUDED.event_kind,
            caller            = EXCLUDED.caller,
            receiver          = EXCLUDED.receiver,
            owner             = EXCLUDED.owner,
            assets            = EXCLUDED.assets,
            shares            = EXCLUDED.shares,
            old_amount        = EXCLUDED.old_amount,
            new_amount        = EXCLUDED.new_amount,
            derive_generation = EXCLUDED.derive_generation
          WHERE upshift_vault_events.derive_generation <= EXCLUDED.derive_generation
    `
	_, err := s.db.ExecContext(ctx, q,
		e.ContractID, int(e.Ledger), e.LedgerCloseTime.UTC(),
		e.TxHash, int(e.OpIndex), int(e.EventIndex),
		string(e.Kind), e.Caller, receiver, owner,
		assets, shares, oldAmount, newAmount,
		s.deriveGeneration,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertUpshiftVaultEvent %s@%d: %w", e.ContractID, e.Ledger, err)
	}
	return nil
}

// UpshiftVaultShareSupply is one vault's derived share supply and cumulative underlying flow over the whole recorded history; supply is
// exact since no mint or burn occurs outside deposit/withdraw.
type UpshiftVaultShareSupply struct {
	ContractID string
	// Shares outstanding: minted − burned, in the vault's share units
	// (NOT the underlying's scale — see migration 0157's header note on
	// the ERC-4626 decimals offset).
	SharesOutstanding canonical.Amount
	AssetsIn          canonical.Amount
	AssetsOut         canonical.Amount
	// DeployedAssets is the most recent deployed_assets_changed
	// new_amount for the vault, or zero when none has been recorded.
	// The DEPLOYED leg only; never the vault's total assets.
	DeployedAssets   canonical.Amount
	HasDeployedTotal bool
	Deposits         int64
	Withdrawals      int64
	LatestAt         time.Time
}

// UpshiftVaultShareSupplies returns the derived share supply for every vault with a recorded flow, ordered by contract id, or (nil,
// nil) when nothing is indexed; unbounded-latest-ok: a vault whose last deployed_assets_changed predates any floor still has that
// deployment.
func (s *Store) UpshiftVaultShareSupplies(ctx context.Context) ([]UpshiftVaultShareSupply, error) {
	const q = `
		WITH flows AS (
		    SELECT contract_id,
		           COALESCE(sum(shares) FILTER (WHERE event_kind = 'deposit'), 0)
		             - COALESCE(sum(shares) FILTER (WHERE event_kind = 'withdraw'), 0) AS shares_outstanding,
		           COALESCE(sum(assets) FILTER (WHERE event_kind = 'deposit'), 0)  AS assets_in,
		           COALESCE(sum(assets) FILTER (WHERE event_kind = 'withdraw'), 0) AS assets_out,
		           count(*) FILTER (WHERE event_kind = 'deposit')  AS deposits,
		           count(*) FILTER (WHERE event_kind = 'withdraw') AS withdrawals,
		           max(ledger_close_time) AS latest_at
		      FROM upshift_vault_events
		     WHERE event_kind IN ('deposit', 'withdraw')
		     GROUP BY contract_id
		), deployed AS (
		    SELECT DISTINCT ON (contract_id) contract_id, new_amount
		      FROM upshift_vault_events
		     WHERE event_kind = 'deployed_assets_changed'
		     ORDER BY contract_id, ledger_close_time DESC, ledger DESC, op_index DESC, event_index DESC
		)
		SELECT f.contract_id,
		       f.shares_outstanding::text,
		       f.assets_in::text,
		       f.assets_out::text,
		       d.new_amount::text,
		       f.deposits, f.withdrawals, f.latest_at
		  FROM flows f
		  LEFT JOIN deployed d USING (contract_id)
		 ORDER BY f.contract_id`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("timescale: UpshiftVaultShareSupplies: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []UpshiftVaultShareSupply
	for rows.Next() {
		var (
			v        UpshiftVaultShareSupply
			deployed sql.NullString
			latest   time.Time
		)
		if err := rows.Scan(&v.ContractID, &v.SharesOutstanding, &v.AssetsIn, &v.AssetsOut,
			&deployed, &v.Deposits, &v.Withdrawals, &latest); err != nil {
			return nil, fmt.Errorf("timescale: UpshiftVaultShareSupplies scan: %w", err)
		}
		if deployed.Valid {
			amt, perr := canonical.FromString(deployed.String)
			if perr != nil {
				return nil, fmt.Errorf("timescale: UpshiftVaultShareSupplies deployed %q: %w", deployed.String, perr)
			}
			v.DeployedAssets = amt
			v.HasDeployedTotal = true
		}
		v.LatestAt = latest.UTC()
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: UpshiftVaultShareSupplies rows: %w", err)
	}
	return out, nil
}
