package classicmovements

import (
	"errors"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// SourceName is the canonical identifier for classic-movement rows, stamped as
// MovementEvent.Source(). stellar.account_movements has no `source` column because it has exactly
// one writer, so nothing needs to discriminate rows by writer.
const SourceName = "classic-movements"

// P23StartLedger is the first ledger of Protocol 23, the upper bound of this package's
// reconstruction: from there every classic movement emits a CAP-67 event (sep41_transfers).
// timescale.SEP41MovementsFloorLedger duplicates it because internal/storage cannot import
// internal/sources; TestP23BoundaryConstantsAgree pins the two together.
const P23StartLedger uint32 = 58_762_517

// Kind discriminates a movement's semantic type. The ten values match migration 0103's
// movement_kind CHECK constraint. recognition_test.go forces each new decode arm to be added
// deliberately.
type Kind string

// The ten movement kinds, in the same order as the migration 0103 CHECK constraint.
const (
	KindPayment                  Kind = "payment"
	KindCreateAccount            Kind = "create_account"
	KindPathPayment              Kind = "path_payment"
	KindAccountMerge             Kind = "account_merge"
	KindClawback                 Kind = "clawback"
	KindClaimableBalanceCreate   Kind = "claimable_balance_create"
	KindClaimableBalanceClaim    Kind = "claimable_balance_claim"
	KindClaimableBalanceClawback Kind = "claimable_balance_clawback"
	KindLiquidityPoolDeposit     Kind = "liquidity_pool_deposit"
	KindLiquidityPoolWithdraw    Kind = "liquidity_pool_withdraw"
)

// IsValid reports whether k is one of the ten known movement kinds.
// Mirrors the CHECK constraint in migration 0103.
func (k Kind) IsValid() bool {
	switch k {
	case KindPayment, KindCreateAccount, KindPathPayment, KindAccountMerge,
		KindClawback, KindClaimableBalanceCreate, KindClaimableBalanceClaim,
		KindClaimableBalanceClawback, KindLiquidityPoolDeposit, KindLiquidityPoolWithdraw:
		return true
	}
	return false
}

// Provenance discriminates how a movement row was derived.
type Provenance string

const (
	// ProvenanceClassicDerived is every row this package has ever
	// written — reconstructed from the ClickHouse lake per ADR-0047.
	ProvenanceClassicDerived Provenance = "classic_derived"

	// ProvenanceCAP67Event is reserved for a possible normalization of post-P23 sep41_transfers
	// rows into the movement store. No writer emits it today.
	ProvenanceCAP67Event Provenance = "cap67_event"
)

// IsValid reports whether p is one of the two known provenance
// values. Mirrors the CHECK constraint in migration 0103.
func (p Provenance) IsValid() bool {
	switch p {
	case ProvenanceClassicDerived, ProvenanceCAP67Event:
		return true
	}
	return false
}

// Movement is one reconstructed two-party classic-asset movement, the decode-time shape of a
// stellar.account_movements row. LegIndex disambiguates multiple rows from the SAME op (e.g. a
// liquidity-pool deposit's two asset legs); single-leg kinds use 0.
type Movement struct {
	Kind            Kind
	Provenance      Provenance
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	LegIndex        uint32
	Asset           string // canonical asset_id: "native" or "CODE-ISSUER"
	Amount          canonical.Amount
	FromAddress     string
	ToAddress       string

	// Attributes is the kind-specific remainder (both path_payment legs carry the whole op, since
	// each leg's Asset/Amount holds only its own side). Values are strings or slices of them,
	// never a raw i128 or JSON number, to keep full precision.
	Attributes map[string]any
}

// MovementEvent is the consumer.Event this package emits, a thin wrapper like sdex.TradeEvent.
// It has NO persist arm in pipeline's HandleEvent on purpose: movements are historical-only and
// written to ClickHouse by the classic-movements-backfill batch writer, never the live dispatcher
// (enforced by the notSunkEvents entry in lockstep_ast_test.go).
type MovementEvent struct {
	Movement Movement
}

// EventKind implements consumer.Event.
func (MovementEvent) EventKind() string { return "classicmovements.movement" }

// Source implements consumer.Event.
func (MovementEvent) Source() string { return SourceName }

// PendingClaimableBalanceRef is a claim/clawback op whose asset/amount could not be resolved from
// Decoder's in-run BalanceId index (see Decoder.TakePendingClaimableBalances). FromAddress and
// ToAddress are already resolved; ResolvePendingClaimableBalance fills in the rest once the create
// is found.
type PendingClaimableBalanceRef struct {
	Kind            Kind // KindClaimableBalanceClaim or KindClaimableBalanceClawback
	BalanceIDHex    string
	Ledger          uint32
	LedgerCloseTime time.Time
	TxHash          string
	OpIndex         uint32
	FromAddress     string
	ToAddress       string
}

// ResolvePendingClaimableBalance builds the Movement for a pending claim/clawback once the caller
// has found the create row (a ClickHouse second pass); this package stays storage-agnostic.
func ResolvePendingClaimableBalance(ref PendingClaimableBalanceRef, asset string, amount canonical.Amount, createdBy string) Movement {
	return Movement{
		Kind:            ref.Kind,
		Provenance:      ProvenanceClassicDerived,
		Ledger:          ref.Ledger,
		LedgerCloseTime: ref.LedgerCloseTime,
		TxHash:          ref.TxHash,
		OpIndex:         ref.OpIndex,
		LegIndex:        0,
		Asset:           asset,
		Amount:          amount,
		FromAddress:     ref.FromAddress,
		ToAddress:       ref.ToAddress,
		Attributes: map[string]any{
			"balance_id":   ref.BalanceIDHex,
			"created_by":   createdBy,
			"resolved_via": "ch_second_pass",
		},
	}
}

// Errors returned by the decode path.
var (
	// ErrUnsupportedOpType is returned by Decode for an op type outside scope. It exists so a
	// future author must extend Matches AND the Decode switch together, not silently produce zero
	// rows (recognition_test.go).
	ErrUnsupportedOpType = errors.New("classicmovements: unsupported op type for this phase")

	// ErrMalformedMovement means a decoded field failed validation (non-positive amount,
	// unresolvable address, unrecognized asset shape): an unaudited protocol assumption, not a
	// routine failed op, which the result's success code filters out earlier.
	ErrMalformedMovement = errors.New("classicmovements: malformed movement")
)
