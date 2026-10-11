package upshift

import (
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
)

// Event is the single [consumer.Event] this package emits, discriminated by Kind and written
// to the `upshift_vault_events` hypertable. Populated fields depend on Kind, and the
// migration's CHECK constraints enforce the same table:
//
//	deposit / withdraw        Caller, Receiver, Owner, Assets, Shares
//	transfer                  Caller (from), Receiver (to), Shares (amount)
//	deployed_assets_changed   Caller (operator), OldAmount, NewAmount
//
// Amounts are RAW i128 (canonical.Amount → NUMERIC per ADR-0003). Shares and assets are on
// different scales (ERC-4626 decimals offset of 6, see the package doc), so nothing here
// divides one by the other or bakes in a decimals assumption.
type Event struct {
	// ContractID is the emitting vault — always a member of the gated
	// set, since Matches has already run.
	ContractID string

	// Soroban event identity. EventIndex is load-bearing: one operation can emit several events,
	// so without it the rows collide on the primary key.
	Ledger     uint32
	ObservedAt time.Time
	TxHash     string
	OpIndex    uint32
	EventIndex uint32

	// Kind is one of the four decoded [EventDeposit], [EventWithdraw],
	// [EventTransfer], [EventDeployedAssetsChanged] values.
	Kind string

	// Caller is topic[1]: the invoking account, the `from` on a share transfer, or the operator.
	Caller string

	// Receiver is topic[2]: the recipient of the underlying (withdraw), the shares (deposit) or the
	// `to` of a transfer. Empty on deployed_assets_changed, which has only two topics.
	Receiver string

	// Owner is topic[3]: whose shares are minted or burned. Empty on transfer and
	// deployed_assets_changed. The package doc says which of this ordering is proven.
	Owner string

	// Assets is the underlying moved in or out. Zero on transfer and
	// deployed_assets_changed.
	Assets canonical.Amount

	// Shares is the share-token delta: minted on deposit, burned on
	// withdraw, moved on transfer. Zero on deployed_assets_changed.
	Shares canonical.Amount

	// OldAmount / NewAmount are the deployed-capital totals of a deployed_assets_changed event,
	// zero on every other kind. They are the DEPLOYED leg only (idle balance is in no event),
	// so they are NOT total assets and must never be served as TVL on their own.
	OldAmount canonical.Amount
	NewAmount canonical.Amount
}

// EventKind implements [consumer.Event].
func (Event) EventKind() string { return EventKind }

// Source implements [consumer.Event] — matches [SourceName].
func (Event) Source() string { return SourceName }

// Compile-time check that Event satisfies consumer.Event.
var _ consumer.Event = Event{}
