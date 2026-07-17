package timescale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
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

	// INV-3 generation-guarded corrective upsert (migration 0110): a
	// corrected re-derive of the decoded governance columns (admin / target /
	// attributes) lands in place when its generation is >= the stored one; a
	// live gen-0 replay can never revert it. Replaces the old DO NOTHING.
	const q = `
        INSERT INTO aquarius_admin (
            contract_id, ledger, ledger_close_time, tx_hash,
            op_index, event_index, event_kind, admin, target,
            attributes, derive_generation
        ) VALUES (
            $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
        )
        ON CONFLICT (ledger_close_time, contract_id, ledger, tx_hash,
                     op_index, event_kind, event_index) DO UPDATE SET
            admin             = EXCLUDED.admin,
            target            = EXCLUDED.target,
            attributes        = EXCLUDED.attributes,
            derive_generation = EXCLUDED.derive_generation
          WHERE aquarius_admin.derive_generation <= EXCLUDED.derive_generation
    `
	_, err = s.db.ExecContext(ctx, q,
		e.ContractID, int(e.Ledger), e.LedgerCloseTime.UTC(), e.TxHash,
		int(e.OpIndex), int(e.EventIndex), string(e.Kind),
		nullString(e.Admin), nullString(e.Target),
		attrsJSON, s.deriveGeneration,
	)
	if err != nil {
		return fmt.Errorf("timescale: InsertAquariusAdminEvent %s@%d: %w", e.ContractID, e.Ledger, err)
	}
	return nil
}

// ─── Read side: governance analytics for the Aquarius bespoke block ────
//
// See aquarius_rewards.go's matching section header — same v0.12
// "backfilled but served nowhere" gap, same consumer
// (protocol_bespoke.go's aquariusRewardsBlocks).

// aquariusAdminAllKinds is the ordered set of the eight governance/upgrade
// kinds — the migration-0100 census order (busiest kind first).
var aquariusAdminAllKinds = []AquariusAdminKind{
	AquariusAdminApplyUpgrade, AquariusAdminCommitUpgrade, AquariusAdminSetPrivilegedAddrs,
	AquariusAdminApplyTransferOwnership, AquariusAdminCommitTransferOwnership,
	AquariusAdminEnableEmergencyMode, AquariusAdminDisableEmergencyMode,
	AquariusAdminPoolGaugeSwitchToken,
}

// AquariusAdminLifetimeTotal reads the LIFETIME (all-time) governance/
// upgrade event count across all eight kinds.
//
// Uses the same per-kind LATERAL pattern as
// AquariusRewardsLifetimeByKind (bounded `event_kind = <literal>` index
// scans against aquarius_admin_kind_ts_idx, migration 0100, one per kind,
// single round trip) rather than a plain `count(*)` — aquarius_admin is a
// low-volume table (governance actions are inherently rare — the
// migration-0100 census counted ~1.8K lifetime rows across all 8 kinds),
// so an unqualified count(*) would likely be cheap too, but staying
// consistent with the indexed-per-kind access path costs nothing and
// avoids relying on "this table happens to be small today."
func (s *Store) AquariusAdminLifetimeTotal(ctx context.Context) (int64, error) {
	kinds := make([]string, len(aquariusAdminAllKinds))
	for i, k := range aquariusAdminAllKinds {
		kinds[i] = string(k)
	}
	const q = `
		SELECT COALESCE(sum(agg.events), 0)
		  FROM unnest($1::text[]) AS k(kind)
		  LEFT JOIN LATERAL (
		         SELECT count(*) AS events
		           FROM aquarius_admin
		          WHERE event_kind = k.kind
		       ) agg ON true`
	var total int64
	if err := s.db.QueryRowContext(ctx, q, pq.Array(kinds)).Scan(&total); err != nil {
		return 0, fmt.Errorf("timescale: AquariusAdminLifetimeTotal: %w", err)
	}
	return total, nil
}

// AquariusAdminEventView is one governance/upgrade event row for the
// "Recent governance events" table — kind/contract/admin/target/ledger,
// newest first.
type AquariusAdminEventView struct {
	Kind            AquariusAdminKind
	ContractID      string
	Admin           string // "" when the kind carries none
	Target          string // "" when the kind carries none
	Ledger          uint32
	LedgerCloseTime time.Time
}

// LatestAquariusAdminEvents reads the most recent `limit` governance/
// upgrade events across the router + pools, newest first. limit <= 0 is
// treated as 25.
//
// Deliberately UNWINDOWED, unlike the sibling bespoke augments' `since`-
// bound "recent N" queries (e.g. lendingAuctionBlocks' "Recent auctions"):
// aquarius_admin is a low-frequency governance surface (~1.8K lifetime
// rows across all 8 kinds), so a trailing-window bound could render an
// empty table between rare upgrade/ownership events even though the
// protocol has governance history worth surfacing. `ORDER BY
// ledger_close_time DESC LIMIT $1` stays indexed + bounded regardless:
// ledger_close_time is the leading column of aquarius_admin's primary-key
// index, so this is a backward index scan capped by LIMIT — TimescaleDB's
// standard "top-N over a hypertable's time column" access path, not a
// full-table scan.
func (s *Store) LatestAquariusAdminEvents(ctx context.Context, limit int) ([]AquariusAdminEventView, error) {
	if limit <= 0 {
		limit = 25
	}
	const q = `
		SELECT event_kind, contract_id, COALESCE(admin, ''), COALESCE(target, ''),
		       ledger, ledger_close_time
		  FROM aquarius_admin
		 ORDER BY ledger_close_time DESC
		 LIMIT $1`
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("timescale: LatestAquariusAdminEvents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []AquariusAdminEventView
	for rows.Next() {
		var (
			v      AquariusAdminEventView
			kind   string
			ledger int64
			closed time.Time
		)
		if err := rows.Scan(&kind, &v.ContractID, &v.Admin, &v.Target, &ledger, &closed); err != nil {
			return nil, fmt.Errorf("timescale: LatestAquariusAdminEvents scan: %w", err)
		}
		v.Kind = AquariusAdminKind(kind)
		v.Ledger = uint32(ledger)
		v.LedgerCloseTime = closed.UTC()
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: LatestAquariusAdminEvents rows: %w", err)
	}
	return out, nil
}
