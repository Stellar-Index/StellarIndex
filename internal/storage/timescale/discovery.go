package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical/discovery"
)

// RecordDiscovered persists a [discovery.Hit] to the
// `discovered_assets` table. Idempotent on contract_id via
// ON CONFLICT — the first observation per contract preserves
// first_seen_*, subsequent observations update last_seen_* and
// increment event_count.
//
// discovery_kind (migration 0103) distinguishes which sniffer
// produced the hit ([discovery.KindSEP41] / [discovery.KindOracleEvent]
// / [discovery.KindOracleCall]) — a Hit with an empty Kind (any
// hand-built Hit predating that field, e.g. from an older in-flight
// binary or a test that only sets EventType) is treated as
// KindSEP41, matching the sniffer's own default. first_seen_event
// stores hit.Symbol when set (all three sniffers populate it) and
// falls back to hit.EventType for the same legacy-Hit case — the
// column's CHECK constraint was widened in the same migration to
// admit the oracle-suggestive symbol set alongside the original four
// SEP-41 values.
//
// Implements [discovery.Recorder].
func (s *Store) RecordDiscovered(ctx context.Context, hit discovery.Hit) error {
	if hit.ContractID == "" {
		return errors.New("timescale: RecordDiscovered: empty ContractID")
	}
	observedAt, err := time.Parse(time.RFC3339, hit.ObservedAtRFC3339)
	if err != nil {
		return fmt.Errorf("timescale: RecordDiscovered %s: parse ObservedAtRFC3339 %q: %w",
			hit.ContractID, hit.ObservedAtRFC3339, err)
	}

	kind := hit.Kind
	if kind == "" {
		kind = discovery.KindSEP41
	}
	symbol := hit.Symbol
	if symbol == "" {
		symbol = string(hit.EventType)
	}

	const q = `
		INSERT INTO discovered_assets
		    (contract_id, first_seen_at, first_seen_ledger, first_seen_event,
		     last_seen_at, last_seen_ledger, event_count, discovery_kind)
		VALUES ($1, $2, $3, $4, $2, $3, 1, $5)
		ON CONFLICT (contract_id) DO UPDATE SET
		    last_seen_at     = EXCLUDED.last_seen_at,
		    last_seen_ledger = EXCLUDED.last_seen_ledger,
		    event_count      = discovered_assets.event_count + 1
	`
	_, err = s.db.ExecContext(ctx, q,
		hit.ContractID,
		observedAt.UTC(),
		int64(hit.Ledger),
		symbol,
		string(kind),
	)
	if err != nil {
		return fmt.Errorf("timescale: RecordDiscovered %s: %w", hit.ContractID, err)
	}
	return nil
}

// IsKnownDiscovered reports whether contractID has been recorded.
// Implements [discovery.Recorder].
//
// Returns (false, nil) for never-seen contracts (NOT an error —
// that's the steady-state for the first event from a new contract).
// Storage errors propagate; caller can choose to log + continue
// (the same contract will reappear on the next event) or surface.
func (s *Store) IsKnownDiscovered(ctx context.Context, contractID string) (bool, error) {
	if contractID == "" {
		return false, nil
	}
	const q = `SELECT 1 FROM discovered_assets WHERE contract_id = $1`
	var one int
	err := s.db.QueryRowContext(ctx, q, contractID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("timescale: IsKnownDiscovered %s: %w", contractID, err)
	}
	return true, nil
}

// DiscoveredAsset is the read shape returned by [ListDiscovered].
// Mirrors the table schema 1:1; the Hit shape from the discovery
// package is write-only-side and lacks the running counters.
type DiscoveredAsset struct {
	ContractID      string
	FirstSeenAt     time.Time
	FirstSeenLedger uint32
	// FirstSeenEvent holds the raw first-seen symbol string cast to
	// [discovery.SEP41EventType] for backward compatibility. For
	// DiscoveryKind == KindSEP41 rows this is exactly one of the four
	// named SEP-41 constants; for KindOracleEvent/KindOracleCall rows
	// it holds the matched oracle-suggestive symbol/function name
	// (e.g. "price_update", "relay") — still a valid string value,
	// just not one of the four legacy constants.
	FirstSeenEvent discovery.SEP41EventType
	// DiscoveryKind distinguishes which sniffer produced the first
	// sighting (added migration 0103). Rows written before that
	// migration read back as [discovery.KindSEP41] (the column's
	// DEFAULT).
	DiscoveryKind  discovery.Kind
	LastSeenAt     time.Time
	LastSeenLedger uint32
	EventCount     int64
}

// ListDiscovered returns rows from `discovered_assets` ordered by
// first_seen_at DESC. Used by the operator-facing "what's new"
// query (`stellarindex-ops discovery list`) and integration tests.
//
// limit caps the result count; pass 0 for no limit. Returns an
// empty slice (not nil + nil error) when the table is empty.
func (s *Store) ListDiscovered(ctx context.Context, limit int) ([]DiscoveredAsset, error) {
	q := `
		SELECT contract_id, first_seen_at, first_seen_ledger, first_seen_event,
		       last_seen_at, last_seen_ledger, event_count, discovery_kind
		  FROM discovered_assets
		 ORDER BY first_seen_at DESC
	`
	args := []any{}
	if limit > 0 {
		q += " LIMIT $1"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("timescale: ListDiscovered: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]DiscoveredAsset, 0, 64)
	for rows.Next() {
		var a DiscoveredAsset
		var fl, ll int64
		var fe, dk string
		if err := rows.Scan(&a.ContractID, &a.FirstSeenAt, &fl, &fe,
			&a.LastSeenAt, &ll, &a.EventCount, &dk); err != nil {
			return nil, fmt.Errorf("timescale: ListDiscovered scan: %w", err)
		}
		a.FirstSeenLedger = uint32(fl) //nolint:gosec // CHECK constraint enforces > 0
		a.LastSeenLedger = uint32(ll)  //nolint:gosec // same
		a.FirstSeenEvent = discovery.SEP41EventType(fe)
		a.DiscoveryKind = discovery.Kind(dk)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ListDiscovered rows: %w", err)
	}
	return out, nil
}
