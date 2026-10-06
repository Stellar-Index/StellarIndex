package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// SpectraEventKind is a `spectra_events.event_kind` value (migration 0210).
type SpectraEventKind string

const (
	SpectraPTDeployed      SpectraEventKind = "pt_deployed"
	SpectraYTDeployed      SpectraEventKind = "yt_deployed"
	SpectraPTAdded         SpectraEventKind = "pt_added"
	SpectraPTMinted        SpectraEventKind = "pt_minted"
	SpectraRedeem          SpectraEventKind = "redeem"
	SpectraYieldUpdated    SpectraEventKind = "yield_updated"
	SpectraTransfer        SpectraEventKind = "transfer"
	SpectraWrap            SpectraEventKind = "wrap"
	SpectraUnwrap          SpectraEventKind = "unwrap"
	SpectraDeposit         SpectraEventKind = "deposit"
	SpectraWithdraw        SpectraEventKind = "withdraw"
	SpectraOrderRegistered SpectraEventKind = "order_registered"
	SpectraOrderFilled     SpectraEventKind = "order_filled"
	SpectraOrderCancelled  SpectraEventKind = "order_cancelled"
)

// SpectraRole is the gated role of the contract that emitted a row.
type SpectraRole string

const (
	SpectraRoleFactory     SpectraRole = "factory"
	SpectraRoleRegistry    SpectraRole = "registry"
	SpectraRolePT          SpectraRole = "pt"
	SpectraRoleYT          SpectraRole = "yt"
	SpectraRoleIBT         SpectraRole = "ibt"
	SpectraRoleOrderEngine SpectraRole = "order_engine"
)

// spectraField is one optional spectra_events column.
type spectraField uint16

const (
	sfMarketPT spectraField = 1 << iota
	sfCaller
	sfReceiver
	sfOwner
	sfMaker
	sfOrderID
	sfIBT
	sfYT
	sfDuration
	sfShares
	sfVaultShares
	sfAssets
	sfAmount
	sfYieldInIBT
)

type spectraKindSpec struct {
	roles  []SpectraRole
	fields spectraField
}

// spectraKindSpecs mirrors the spectra_events_kind_columns and
// spectra_events_kind_column_count CHECKs: a kind carries exactly these
// fields and every other optional column is NULL.
var spectraKindSpecs = map[SpectraEventKind]spectraKindSpec{
	SpectraPTDeployed:      {[]SpectraRole{SpectraRoleFactory}, sfMarketPT | sfCaller | sfIBT | sfDuration},
	SpectraYTDeployed:      {[]SpectraRole{SpectraRolePT}, sfMarketPT | sfYT},
	SpectraPTAdded:         {[]SpectraRole{SpectraRoleRegistry}, sfMarketPT},
	SpectraPTMinted:        {[]SpectraRole{SpectraRolePT}, sfMarketPT | sfCaller | sfReceiver | sfShares},
	SpectraRedeem:          {[]SpectraRole{SpectraRolePT}, sfMarketPT | sfOwner | sfReceiver | sfShares},
	SpectraYieldUpdated:    {[]SpectraRole{SpectraRolePT}, sfMarketPT | sfOwner | sfYieldInIBT},
	SpectraTransfer:        {[]SpectraRole{SpectraRolePT, SpectraRoleYT}, sfMarketPT | sfCaller | sfReceiver | sfAmount},
	SpectraWrap:            {[]SpectraRole{SpectraRoleIBT}, sfCaller | sfReceiver | sfShares | sfVaultShares},
	SpectraUnwrap:          {[]SpectraRole{SpectraRoleIBT}, sfCaller | sfReceiver | sfOwner | sfShares | sfVaultShares},
	SpectraDeposit:         {[]SpectraRole{SpectraRoleIBT}, sfCaller | sfReceiver | sfOwner | sfAssets | sfShares},
	SpectraWithdraw:        {[]SpectraRole{SpectraRoleIBT}, sfCaller | sfReceiver | sfOwner | sfAssets | sfShares},
	SpectraOrderRegistered: {[]SpectraRole{SpectraRoleOrderEngine}, sfMaker | sfOrderID | sfAmount},
	SpectraOrderFilled:     {[]SpectraRole{SpectraRoleOrderEngine}, sfOrderID | sfAmount},
	SpectraOrderCancelled:  {[]SpectraRole{SpectraRoleOrderEngine}, sfMaker | sfOrderID},
}

var spectraOrderIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// SpectraEvent is one `spectra_events` row. Which optional fields a kind
// carries is fixed by migration 0210; the writer sends the rest as NULL
// and refuses a non-zero value in a field the kind does not carry.
type SpectraEvent struct {
	ContractID      string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	EventIndex      uint32

	Kind SpectraEventKind
	Role SpectraRole

	// MarketPT is the PT naming the market; empty on IBT and order kinds.
	MarketPT string

	Caller   string
	Receiver string
	Owner    string
	Maker    string
	// OrderID is the order's bytes32 id as 64 lowercase hex characters.
	OrderID         string
	IBT             string
	YT              string
	DurationSeconds uint64

	// Amounts are pointers: nil is "not set", which the writer refuses
	// for a column the kind carries instead of storing it as 0.
	Shares      *canonical.Amount
	VaultShares *canonical.Amount
	Assets      *canonical.Amount
	Amount      *canonical.Amount
	YieldInIBT  *canonical.Amount
}

// InsertSpectraEvent writes one spectra_events row, idempotent on the
// (ledger_close_time, contract_id, ledger, tx_hash, op_index,
// event_index) key under the derive_generation guard: an equal-or-higher
// generation overwrites the stored row, a lower one is refused. When the
// row written is, or replaces, a pt_deployed / yt_deployed / pt_added row,
// the markets it names are re-derived from spectra_events in the same
// transaction, so a rebuild that corrects a discovery row's PT leaves no
// stale market behind, and a refused replay touches neither table.
func (s *Store) InsertSpectraEvent(ctx context.Context, e SpectraEvent) error {
	args, err := spectraEventArgs(e)
	if err != nil {
		return fmt.Errorf("timescale: InsertSpectraEvent: %w", err)
	}

	const q = `
        INSERT INTO spectra_events (
            contract_id, ledger, ledger_close_time, tx_hash, op_index, event_index,
            event_kind, role, market_pt,
            caller, receiver, owner, maker, order_id, ibt, yt, duration_s,
            shares, vault_shares, assets, amount, yield_in_ibt,
            derive_generation
        ) VALUES (
            $1, $2, $3, $4, $5, $6,
            $7, $8, $9,
            $10, $11, $12, $13, $14, $15, $16, $17,
            $18, $19, $20, $21, $22,
            $23
        )
        ON CONFLICT (ledger_close_time, contract_id, ledger, tx_hash,
                     op_index, event_index) DO UPDATE SET
            event_kind        = EXCLUDED.event_kind,
            role              = EXCLUDED.role,
            market_pt         = EXCLUDED.market_pt,
            caller            = EXCLUDED.caller,
            receiver          = EXCLUDED.receiver,
            owner             = EXCLUDED.owner,
            maker             = EXCLUDED.maker,
            order_id          = EXCLUDED.order_id,
            ibt               = EXCLUDED.ibt,
            yt                = EXCLUDED.yt,
            duration_s        = EXCLUDED.duration_s,
            shares            = EXCLUDED.shares,
            vault_shares      = EXCLUDED.vault_shares,
            assets            = EXCLUDED.assets,
            amount            = EXCLUDED.amount,
            yield_in_ibt      = EXCLUDED.yield_in_ibt,
            derive_generation = EXCLUDED.derive_generation
          WHERE spectra_events.derive_generation <= EXCLUDED.derive_generation
    `
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("timescale: InsertSpectraEvent begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	prior, err := priorSpectraMarket(ctx, tx, e)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, q, append(args, s.deriveGeneration)...)
	if err != nil {
		return fmt.Errorf("timescale: InsertSpectraEvent %s %s@%d: %w", e.Kind, e.ContractID, e.Ledger, err)
	}
	written, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("timescale: InsertSpectraEvent rows affected: %w", err)
	}
	if written > 0 {
		pts := []string{prior}
		if isSpectraDiscovery(string(e.Kind)) {
			pts = append(pts, e.MarketPT)
		}
		for _, pt := range pts {
			if err := refreshSpectraMarket(ctx, tx, pt); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("timescale: InsertSpectraEvent commit: %w", err)
	}
	return nil
}

// spectraEventSpec checks e's identity, role and scalar formats and
// returns its kind's field set.
func spectraEventSpec(e SpectraEvent) (spectraKindSpec, error) {
	if e.ContractID == "" || e.TxHash == "" {
		return spectraKindSpec{}, errors.New("ContractID and TxHash are required")
	}
	spec, ok := spectraKindSpecs[e.Kind]
	if !ok {
		return spec, fmt.Errorf("invalid Kind %q", e.Kind)
	}
	if !slices.Contains(spec.roles, e.Role) {
		return spec, fmt.Errorf("%s cannot be emitted by role %q", e.Kind, e.Role)
	}
	if e.Role == SpectraRolePT && e.MarketPT != e.ContractID {
		return spec, fmt.Errorf("%s from PT %s names market %q", e.Kind, e.ContractID, e.MarketPT)
	}
	if e.OrderID != "" && !spectraOrderIDPattern.MatchString(e.OrderID) {
		return spec, fmt.Errorf("OrderID %q is not 64 lowercase hex characters", e.OrderID)
	}
	if e.DurationSeconds > math.MaxInt64 {
		return spec, fmt.Errorf("DurationSeconds %d overflows bigint", e.DurationSeconds)
	}
	return spec, nil
}

// spectraColumns turns optional fields into NULL-or-value arguments for
// one kind, collecting every field that contradicts the kind.
type spectraColumns struct {
	kind    SpectraEventKind
	fields  spectraField
	problem error
}

func (c *spectraColumns) text(f spectraField, name, v string) sql.NullString {
	carried := c.fields&f != 0
	switch {
	case !carried && v != "":
		c.problem = errors.Join(c.problem, fmt.Errorf("%s does not carry %s (got %q)", c.kind, name, v))
	case carried && v == "":
		c.problem = errors.Join(c.problem, fmt.Errorf("%s needs %s", c.kind, name))
	}
	return sql.NullString{String: v, Valid: carried}
}

// amount keys presence off the pointer, never IsZero: a zero amount is a
// value, an unset one is a decoder bug.
func (c *spectraColumns) amount(f spectraField, name string, v *canonical.Amount, signed bool) sql.NullString {
	carried := c.fields&f != 0
	switch {
	case !carried && v != nil:
		c.problem = errors.Join(c.problem, fmt.Errorf("%s does not carry %s (got %s)", c.kind, name, v))
	case carried && v == nil:
		c.problem = errors.Join(c.problem, fmt.Errorf("%s needs %s", c.kind, name))
	case carried && !signed && v.Sign() < 0:
		c.problem = errors.Join(c.problem, fmt.Errorf("%s %s must be >= 0 (got %s)", c.kind, name, v))
	}
	if v == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: v.String(), Valid: carried}
}

// spectraEventArgs validates e against its kind and returns the first 22
// insert arguments, with every field the kind does not carry as NULL.
func spectraEventArgs(e SpectraEvent) ([]any, error) {
	spec, err := spectraEventSpec(e)
	if err != nil {
		return nil, err
	}
	c := &spectraColumns{kind: e.Kind, fields: spec.fields}
	text, amount := c.text, c.amount
	duration := sql.NullInt64{Int64: int64(e.DurationSeconds), Valid: spec.fields&sfDuration != 0}
	if !duration.Valid && e.DurationSeconds != 0 {
		c.problem = errors.Join(c.problem, fmt.Errorf("%s does not carry DurationSeconds (got %d)", e.Kind, e.DurationSeconds))
	}

	args := []any{
		e.ContractID, int(e.Ledger), e.LedgerCloseTime.UTC(), e.TxHash, int(e.OpIndex), int(e.EventIndex),
		string(e.Kind), string(e.Role), text(sfMarketPT, "MarketPT", e.MarketPT),
		text(sfCaller, "Caller", e.Caller),
		text(sfReceiver, "Receiver", e.Receiver),
		text(sfOwner, "Owner", e.Owner),
		text(sfMaker, "Maker", e.Maker),
		text(sfOrderID, "OrderID", e.OrderID),
		text(sfIBT, "IBT", e.IBT),
		text(sfYT, "YT", e.YT),
		duration,
		amount(sfShares, "Shares", e.Shares, false),
		amount(sfVaultShares, "VaultShares", e.VaultShares, false),
		amount(sfAssets, "Assets", e.Assets, false),
		amount(sfAmount, "Amount", e.Amount, false),
		amount(sfYieldInIBT, "YieldInIBT", e.YieldInIBT, true),
	}
	if c.problem != nil {
		return nil, c.problem
	}
	return args, nil
}

func isSpectraDiscovery(kind string) bool {
	switch SpectraEventKind(kind) {
	case SpectraPTDeployed, SpectraYTDeployed, SpectraPTAdded:
		return true
	case SpectraPTMinted, SpectraRedeem, SpectraYieldUpdated, SpectraTransfer,
		SpectraWrap, SpectraUnwrap, SpectraDeposit, SpectraWithdraw,
		SpectraOrderRegistered, SpectraOrderFilled, SpectraOrderCancelled:
		return false
	}
	return false
}

// priorSpectraMarket returns the market a stored discovery row at e's key
// names, or "" when there is none, locking the row for the overwrite.
func priorSpectraMarket(ctx context.Context, tx *sql.Tx, e SpectraEvent) (string, error) {
	const q = `
        SELECT event_kind, market_pt
          FROM spectra_events
         WHERE ledger_close_time = $1 AND contract_id = $2 AND ledger = $3
           AND tx_hash = $4 AND op_index = $5 AND event_index = $6
           FOR UPDATE`
	var kind string
	var pt sql.NullString
	err := tx.QueryRowContext(ctx, q, e.LedgerCloseTime.UTC(), e.ContractID, int(e.Ledger),
		e.TxHash, int(e.OpIndex), int(e.EventIndex)).Scan(&kind, &pt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("timescale: InsertSpectraEvent prior row: %w", err)
	case isSpectraDiscovery(kind):
		return pt.String, nil
	}
	return "", nil
}

// refreshSpectraMarket re-derives one spectra_markets row from the
// earliest discovery row of each kind naming pt, and deletes it when none
// is left. The six pt_deployed columns come from one row, so the
// all-or-none CHECK holds.
//
// unbounded-latest-ok: a market's creation row is arbitrarily old, and the read is keyed to one market_pt.
func refreshSpectraMarket(ctx context.Context, tx *sql.Tx, pt string) error {
	if pt == "" {
		return nil
	}
	const upsert = `
        WITH d AS (
            SELECT DISTINCT ON (event_kind)
                   event_kind, contract_id, ledger, ledger_close_time, caller, ibt, yt, duration_s
              FROM spectra_events
             WHERE market_pt = $1
               AND event_kind IN ('pt_deployed', 'yt_deployed', 'pt_added')
             ORDER BY event_kind, ledger, op_index, event_index
        )
        INSERT INTO spectra_markets (
            pt, yt, ibt, factory_id, deployer, duration_s,
            creation_ledger, deployed_at, listed_ledger
        )
        SELECT $1,
               max(yt)                FILTER (WHERE event_kind = 'yt_deployed'),
               max(ibt)               FILTER (WHERE event_kind = 'pt_deployed'),
               max(contract_id)       FILTER (WHERE event_kind = 'pt_deployed'),
               max(caller)            FILTER (WHERE event_kind = 'pt_deployed'),
               max(duration_s)        FILTER (WHERE event_kind = 'pt_deployed'),
               max(ledger)            FILTER (WHERE event_kind = 'pt_deployed'),
               max(ledger_close_time) FILTER (WHERE event_kind = 'pt_deployed'),
               max(ledger)            FILTER (WHERE event_kind = 'pt_added')
          FROM d
        HAVING count(*) > 0
        ON CONFLICT (pt) DO UPDATE SET
            yt              = EXCLUDED.yt,
            ibt             = EXCLUDED.ibt,
            factory_id      = EXCLUDED.factory_id,
            deployer        = EXCLUDED.deployer,
            duration_s      = EXCLUDED.duration_s,
            creation_ledger = EXCLUDED.creation_ledger,
            deployed_at     = EXCLUDED.deployed_at,
            listed_ledger   = EXCLUDED.listed_ledger,
            updated_at      = now()
    `
	if _, err := tx.ExecContext(ctx, upsert, pt); err != nil {
		return fmt.Errorf("timescale: InsertSpectraEvent market %s: %w", pt, err)
	}
	const prune = `
        DELETE FROM spectra_markets m
         WHERE m.pt = $1
           AND NOT EXISTS (
               SELECT 1 FROM spectra_events
                WHERE market_pt = $1
                  AND event_kind IN ('pt_deployed', 'yt_deployed', 'pt_added'))`
	if _, err := tx.ExecContext(ctx, prune, pt); err != nil {
		return fmt.Errorf("timescale: InsertSpectraEvent prune market %s: %w", pt, err)
	}
	return nil
}

// SpectraMarket is one `spectra_markets` row. An empty string or zero
// value means that column's discovery event has not been recorded yet.
type SpectraMarket struct {
	PT              string
	YT              string
	IBT             string
	FactoryID       string
	Deployer        string
	DurationSeconds uint64
	CreationLedger  uint32
	DeployedAt      time.Time
	// ListedLedger is the registry's pt_added ledger; zero = not listed.
	ListedLedger uint32
}

// SpectraMarkets returns every recorded market ordered by PT.
// Empty-safe: (nil, nil) before any market is recorded.
func (s *Store) SpectraMarkets(ctx context.Context) ([]SpectraMarket, error) {
	const q = `
        SELECT pt, yt, ibt, factory_id, deployer, duration_s,
               creation_ledger, deployed_at, listed_ledger
          FROM spectra_markets
         ORDER BY pt`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("timescale: SpectraMarkets: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SpectraMarket
	for rows.Next() {
		var (
			m                          SpectraMarket
			yt, ibt, factory, deployer sql.NullString
			duration, created, listed  sql.NullInt64
			deployedAt                 sql.NullTime
		)
		if err := rows.Scan(&m.PT, &yt, &ibt, &factory, &deployer, &duration,
			&created, &deployedAt, &listed); err != nil {
			return nil, fmt.Errorf("timescale: SpectraMarkets scan: %w", err)
		}
		m.YT, m.IBT, m.FactoryID, m.Deployer = yt.String, ibt.String, factory.String, deployer.String
		m.DurationSeconds = uint64(duration.Int64)
		m.CreationLedger = uint32(created.Int64)
		m.ListedLedger = uint32(listed.Int64)
		if deployedAt.Valid {
			m.DeployedAt = deployedAt.Time.UTC()
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: SpectraMarkets rows: %w", err)
	}
	return out, nil
}
