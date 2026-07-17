package timescale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
)

// InsertBlendEmissionEvent appends one Blend emission / credit-
// risk event (gulp / claim / reserve_emission_update /
// gulp_emissions / bad_debt / defaulted_debt) to the
// blend_emissions hypertable. Idempotent on the PK
// (pool, ledger, tx_hash, op_index, event_kind, ledger_close_time).
//
// The amount column is i128 → NUMERIC via decimal string per
// ADR-0003. Asset / User are NULL when the event kind doesn't
// carry them (see per-kind doc in migration 0042 + the
// blend.EmissionEvent godoc).
//
// reserve_emission_update extras (res_token_id, eps, expiration)
// and claim's reserve_token_ids are serialised into the
// attributes jsonb column rather than promoted to typed columns —
// they're per-kind specific and don't warrant a dedicated column
// each (same shape as cctp_events / migration 0038).
func (s *Store) InsertBlendEmissionEvent(ctx context.Context, e domain.BlendEmissionEvent) error {
	if e.Pool == "" {
		return errors.New("timescale: InsertBlendEmissionEvent: Pool is empty")
	}
	if e.TxHash == "" {
		return errors.New("timescale: InsertBlendEmissionEvent: TxHash is empty")
	}
	if !isBlendEmissionKind(e.Kind) {
		return fmt.Errorf("timescale: InsertBlendEmissionEvent: invalid Kind %q", e.Kind)
	}

	attrs := buildEmissionAttributes(e)
	attrsJSON, err := json.Marshal(attrs)
	if err != nil {
		return fmt.Errorf("timescale: InsertBlendEmissionEvent: marshal attributes: %w", err)
	}

	// INV-3 generation-guarded corrective upsert (migration 0110): a
	// corrected re-derive of the emission `amount` (or asset / user /
	// attributes) lands in place when its generation is >= the stored one; a
	// live gen-0 replay can never revert it. Replaces the old DO NOTHING.
	const q = `
        INSERT INTO blend_emissions (
            pool, ledger, tx_hash, op_index, event_index, ledger_close_time,
            event_kind, amount, asset, user_address,
            attributes, derive_generation
        ) VALUES (
            $1, $2, $3, $4, $5, $6,
            $7, $8, $9, $10,
            $11, $12
        )
        ON CONFLICT (pool, ledger, tx_hash, op_index, event_kind, event_index, ledger_close_time) DO UPDATE SET
            amount            = EXCLUDED.amount,
            asset             = EXCLUDED.asset,
            user_address      = EXCLUDED.user_address,
            attributes        = EXCLUDED.attributes,
            derive_generation = EXCLUDED.derive_generation
          WHERE blend_emissions.derive_generation <= EXCLUDED.derive_generation
    `
	_, err = s.db.ExecContext(ctx, q,
		e.Pool, int(e.Ledger), e.TxHash, int(e.OpIndex), int(e.EventIndex), e.Timestamp.UTC(),
		e.Kind,
		nullNumeric(bigIntOrEmpty(e.Amount)),
		nullString(e.Asset), nullString(e.User),
		attrsJSON, s.deriveGeneration,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertBlendEmissionEvent %s@%d: %w", e.Pool, e.Ledger, err)
	}
	return nil
}

// BlendEmissionSummary is the windowed Blend emission / credit-risk
// activity summary — the READ side of blend_emissions (migration 0045).
// ClaimVolume is the summed claimed-emission amount in token base units
// (i128/NUMERIC, never int64 — ADR-0003). CreditRisk counts bad_debt +
// defaulted_debt events (a genuine risk signal — surface honestly).
type BlendEmissionSummary struct {
	Claims      int64
	ClaimVolume canonical.Amount
	Gulps       int64 // gulp + gulp_emissions (emission accounting)
	CreditRisk  int64 // bad_debt + defaulted_debt
	TotalEvents int64
}

// BlendEmissionWindowStats reads the windowed Blend emission / credit-risk
// summary (claims + claim volume + gulps + credit-risk events) from
// blend_emissions. Claim amounts are token base units (per-asset decimals),
// preserved as canonical.Amount (ADR-0003).
//
// Empty-safe: returns (nil, nil) when no emission event exists in the
// window, so the bespoke KPI is omitted cleanly. windowDays <= 0 is treated
// as 90.
func (s *Store) BlendEmissionWindowStats(ctx context.Context, windowDays int) (*BlendEmissionSummary, error) {
	if windowDays <= 0 {
		windowDays = 90
	}
	since := fmt.Sprintf("%d days", windowDays)

	var out BlendEmissionSummary
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE event_kind = 'claim'),
		       COALESCE(sum(amount) FILTER (WHERE event_kind = 'claim'),0)::text,
		       count(*) FILTER (WHERE event_kind IN ('gulp','gulp_emissions')),
		       count(*) FILTER (WHERE event_kind IN ('bad_debt','defaulted_debt')),
		       count(*)
		  FROM blend_emissions WHERE ledger_close_time > now() - $1::interval`, since).
		Scan(&out.Claims, &out.ClaimVolume, &out.Gulps, &out.CreditRisk, &out.TotalEvents); err != nil {
		return nil, fmt.Errorf("timescale: BlendEmissionWindowStats: %w", err)
	}
	if out.TotalEvents == 0 {
		return nil, nil
	}
	return &out, nil
}

// buildEmissionAttributes builds the jsonb payload for the
// attributes column on a per-kind basis. Empty (i.e. {}) for
// kinds whose fields are all promoted to typed columns.
func buildEmissionAttributes(e domain.BlendEmissionEvent) map[string]any {
	attrs := map[string]any{}
	switch e.Kind {
	case domain.BlendEventReserveEmissions:
		attrs["res_token_id"] = e.ResTokenID
		attrs["eps"] = e.EmissionsPerSec
		attrs["expiration"] = e.Expiration
	case domain.BlendEventClaim:
		if len(e.ReserveTokenIDs) > 0 {
			attrs["reserve_token_ids"] = e.ReserveTokenIDs
		}
	}
	return attrs
}

// isBlendEmissionKind reports whether kind is one of the emission /
// credit-risk event kinds. Mirrors the CHECK constraint in migration
// 0042, widened by migration 0097 to admit the V1 pool-factory's
// update_emissions.
func isBlendEmissionKind(kind string) bool {
	switch kind {
	case domain.BlendEventGulp,
		domain.BlendEventClaim,
		domain.BlendEventReserveEmissions,
		domain.BlendEventGulpEmissions,
		domain.BlendEventBadDebt,
		domain.BlendEventDefaultedDebt,
		domain.BlendEventUpdateEmissions:
		return true
	}
	return false
}

// bigIntOrEmpty returns the decimal string of n, or empty string
// if n is nil. Empty string is what nullNumeric treats as NULL.
func bigIntOrEmpty(n interface{ String() string }) string {
	// Concrete *big.Int may be a typed-nil interface; check the
	// concrete pointer rather than the interface value.
	if n == nil {
		return ""
	}
	// big.Int's String() is defined on the value receiver, and
	// (*big.Int)(nil).String() does NOT panic in modern Go (it
	// returns "<nil>"). Catch that explicit sentinel + the empty
	// case as "no amount".
	s := n.String()
	if s == "" || s == "<nil>" {
		return ""
	}
	return s
}
