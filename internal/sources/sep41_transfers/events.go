package sep41_transfers

import (
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

const (
	SourceName = "sep41_transfers"
	EventKind  = "sep41_transfers.event"

	SymbolTransfer      = "transfer"
	SymbolApprove       = "approve"
	SymbolSetAdmin      = "set_admin"
	SymbolSetAuthorized = "set_authorized"
)

// Pre-encoded base64 SCVal blobs for cheap byte-equality matching
// against incoming topic[0] strings.
var (
	TopicSymbolTransfer      = scval.MustEncodeSymbol(SymbolTransfer)
	TopicSymbolApprove       = scval.MustEncodeSymbol(SymbolApprove)
	TopicSymbolSetAdmin      = scval.MustEncodeSymbol(SymbolSetAdmin)
	TopicSymbolSetAuthorized = scval.MustEncodeSymbol(SymbolSetAuthorized)
)

// Event is one decoded SEP-41 audit-trail event. Kind discriminates
// which of the four topics fired; non-applicable fields are
// zero/nil per kind:
//
//	transfer       -> FromAddr, ToAddr, Amount populated
//	approve        -> FromAddr (from), ToAddr (spender), Amount, LiveUntilLedger populated
//	set_admin      -> FromAddr (admin, optional), ToAddr (new_admin) populated
//	set_authorized -> ToAddr (id), Authorized populated
//
// A transfer's ToAddr is always its topic[2] Address. The CAP-67 map body's
// to_muxed_id is not captured: the row keys the holder's account, so a muxed
// payment and a plain one to the same account stay on one ?to= history.
type Event struct {
	ContractID      string
	Ledger          uint32
	TxHash          string
	OpIndex         uint32
	EventIndex      uint32
	ObservedAt      time.Time
	Kind            string
	FromAddr        string
	ToAddr          string
	Amount          *big.Int
	LiveUntilLedger uint32
	Authorized      *bool
}
