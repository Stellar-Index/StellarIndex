package upshift

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// Event is one decoded Upshift vault event — the single
// [consumer.Event] this package emits, discriminated by Kind (the
// internal/sources/sep41_transfers shape). The pipeline sink writes it
// to the `upshift_vault_events` hypertable (migration 0157).
//
// Which fields are populated is a function of Kind, and the migration's
// CHECK constraints enforce the same table:
//
//	deposit / withdraw        Caller, Receiver, Owner, Assets, Shares
//	transfer                  Caller (from), Receiver (to), Shares (amount)
//	deployed_assets_changed   Caller (operator), OldAmount, NewAmount
//
// Amounts are RAW i128 (canonical.Amount → NUMERIC per ADR-0003).
// Nothing here divides shares by assets or applies a decimals
// assumption: the two are on different scales (the vault mints with an
// ERC-4626 decimals offset of 6, see the package doc) and the offset is
// evidence, not a constant this package is entitled to bake in.
type Event struct {
	// ContractID is the emitting vault — always a member of the gated
	// set, since Matches has already run.
	ContractID string

	// Soroban event identity. EventIndex is load-bearing: a single
	// operation emits the SAC transfer AND the vault event, and a
	// redemption emits several vault events, so without it the rows
	// collide on the primary key.
	Ledger     uint32
	ObservedAt time.Time
	TxHash     string
	OpIndex    uint32
	EventIndex uint32

	// Kind is one of the four decoded [EventDeposit], [EventWithdraw],
	// [EventTransfer], [EventDeployedAssetsChanged] values.
	Kind string

	// Caller is topic[1]: the account that invoked the vault on
	// deposit/withdraw, the `from` on a share transfer, or the operator
	// on deployed_assets_changed.
	Caller string

	// Receiver is topic[2]: who receives the underlying on a withdraw
	// (or the shares on a deposit), and the `to` on a share transfer.
	// Empty on deployed_assets_changed, which has only two topics.
	Receiver string

	// Owner is topic[3]: whose shares are minted or burned. Empty on
	// transfer and deployed_assets_changed. See the package doc for
	// what is proven about this ordering and what is inferred.
	Owner string

	// Assets is the underlying moved in or out. Zero on transfer and
	// deployed_assets_changed.
	Assets canonical.Amount

	// Shares is the share-token delta: minted on deposit, burned on
	// withdraw, moved on transfer. Zero on deployed_assets_changed.
	Shares canonical.Amount

	// OldAmount / NewAmount are the deployed-capital totals a
	// deployed_assets_changed event reports. Zero on every other kind.
	// These are the DEPLOYED leg only — the vault's idle balance is not
	// observable in any event, so they are NOT the vault's total assets
	// and must never be served as TVL on their own.
	OldAmount canonical.Amount
	NewAmount canonical.Amount
}

// EventKind implements [consumer.Event].
func (Event) EventKind() string { return EventKind }

// Source implements [consumer.Event] — matches [SourceName].
func (Event) Source() string { return SourceName }

// Compile-time check that Event satisfies consumer.Event.
var _ consumer.Event = Event{}
