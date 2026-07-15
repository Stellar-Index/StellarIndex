package classicmovements

import (
	"errors"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// SourceName is the canonical identifier for classic-movement rows.
// Stamped as MovementEvent.Source() and as the ClassicMovementRow's
// implicit writer identity — the classic_movements table itself has
// no `source` column (unlike `trades`) because it has exactly one
// writer (ADR-0031 "one writer per domain"; ADR-0047 D2), so nothing
// needs to discriminate rows by writer.
const SourceName = "classic-movements"

// P23StartLedger is ADR-0047 D2's hard upper bound: the first ledger
// of Protocol 23 (Whisk, mainnet 2025-09-03), from which every
// classic-asset movement already emits a unified CAP-67 event
// (internal/sources/sep41_transfers) — this package's pre-P23
// reconstruction has nothing to do at or beyond this ledger.
// docs/architecture/pre-p23-classic-movements-research.md §1's
// ledger-boundary table confirms this exact value against
// stellar.ledgers on r1 — NOT an approximation.
//
// The canonical, exported home for this value: internal/ops/chops's
// classic-movements-backfill clamp and ADR-0048 D5's
// /v1/accounts/{g}/movements merge (internal/api/v1/explorer/movements.go,
// timescale.SEP41MovementsFloorLedger) both key off it — the latter
// via a same-VALUE constant rather than an import, since
// internal/storage sits below internal/sources in the repo's import
// direction (scripts/ci/lint-imports.sh's L/storage-below-compute
// rule); TestP23BoundaryConstantsAgree (internal/api/v1/explorer/movements_test.go)
// pins the two together so they can't silently drift.
const P23StartLedger uint32 = 58_762_517

// Kind discriminates a movement's semantic type. The ten values
// match migration 0103's movement_kind CHECK constraint — ALL TEN
// are admitted by the schema from Phase 1 on (ADR-0047 D1), even
// though this package's Decode only ever emits KindPayment /
// KindCreateAccount today. Phases 2-4 add the remaining decode
// arms; see recognition_test.go for the guard that forces each
// phase's author to extend this deliberately.
type Kind string

// The ten ADR-0047 D1 movement kinds, in the same order as the
// migration 0103 CHECK constraint.
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

	// ProvenanceCAP67Event is RESERVED (ADR-0047 D1) for a possible
	// future normalization of post-P23 sep41_transfers 'transfer'
	// rows into classic_movements. No writer emits it today —
	// present here only so callers building attributes maps have
	// the exact wire value on hand if that normalization ever
	// lands.
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

// Movement is one reconstructed two-party classic-asset movement —
// the decode-time shape of a classic_movements row (ADR-0047 D1).
// LegIndex disambiguates multiple rows produced by the SAME op
// (e.g. a liquidity-pool deposit's two asset legs, Phase 4); Phase
// 1's two kinds are always single-leg, so it is always 0 there.
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

	// Attributes is the kind-specific remainder, written straight to
	// migration 0105's `attributes jsonb` column (empty/nil marshals
	// to '{}', matching the column DEFAULT). Phase 1's two kinds
	// never populate it. From Phase 2 on: path_payment carries the
	// source leg (send_asset/send_amount) here since Asset/Amount
	// above hold the DESTINATION leg (ADR-0047 Phase 2); claimable
	// balance kinds carry balance_id (+ a claimants summary on
	// create); the CAP-0038 liquidity_pool_withdraw revocation edge
	// case (Phase 4) marks its provenance here. Values are strings
	// (decimal amounts, hex ids, asset ids) or simple slices thereof
	// — never a raw i128, per ADR-0003's "decimal string, not a JSON
	// number" rule applied uniformly.
	Attributes map[string]any
}

// MovementEvent is the consumer.Event this package emits — the
// same "wrap the canonical row in a thin Source()/EventKind()
// shell" pattern as sdex.TradeEvent.
//
// This type deliberately has NO persist arm in internal/pipeline/
// sink.go's HandleEvent: classic_movements is historical-only
// (ADR-0047 D2) and is written by its own dedicated
// `stellarindex-ops classic-movements-backfill` batch writer, never
// through the live dispatcher / pipeline.HandleEvent path. See
// internal/pipeline/lockstep_ast_test.go's notSunkEvents entry for
// "classicmovements.MovementEvent" — that registration is this
// design decision made mechanically enforceable.
type MovementEvent struct {
	Movement Movement
}

// EventKind implements consumer.Event.
func (MovementEvent) EventKind() string { return "classicmovements.movement" }

// Source implements consumer.Event.
func (MovementEvent) Source() string { return SourceName }

// PendingClaimableBalanceRef is a ClaimClaimableBalance /
// ClawbackClaimableBalance op whose asset/amount couldn't be
// resolved from Decoder's in-run BalanceId index — see
// dispatcher_adapter.go's Decoder doc and
// Decoder.TakePendingClaimableBalances. FromAddress/ToAddress are
// already resolved (they don't depend on the correlated create row);
// only Asset/Amount/Provenance/Attributes remain to be filled in by
// ResolvePendingClaimableBalance once the create is found.
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

// ResolvePendingClaimableBalance builds the Movement for a
// previously-pending claim/clawback now that its create row's
// asset/amount/creator have been found — typically via a ClickHouse
// second-pass lookup (clickhouse.FindClaimableBalanceCreates —
// ADR-0048 D2; previously Postgres)
// run by the caller after Decoder.TakePendingClaimableBalances,
// since this package stays storage-agnostic (mirrors
// internal/sources/sdex never importing a storage package).
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
	// ErrUnsupportedOpType is returned by Decode when handed an
	// operation type outside the current phase's scope. Matches()
	// gates the op type BEFORE Decode is ever called in the normal
	// backfill loop, so this should never fire there — its only job
	// is the ADR-0047 D4.2 recognition guard: it forces a future
	// phase's author to extend Matches AND the Decode switch
	// together, rather than let an unhandled op type silently
	// produce zero rows. See recognition_test.go.
	ErrUnsupportedOpType = errors.New("classicmovements: unsupported op type for this phase")

	// ErrMalformedMovement — a decoded field failed validation
	// (non-positive amount, unresolvable address, unrecognized
	// asset shape). Indicates a protocol assumption this package
	// hasn't audited, not routine "op failed" — a failed op is
	// filtered out earlier via the result's success code and never
	// reaches this error path.
	ErrMalformedMovement = errors.New("classicmovements: malformed movement")
)
