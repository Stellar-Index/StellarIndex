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

// Event is one decoded SEP-41 audit-trail event; Kind selects which of FromAddr, ToAddr, Amount, LiveUntilLedger and Authorized are set.
// A transfer's ToAddr is its topic[2] Address; the CAP-67 to_muxed_id is not captured, so muxed and plain payments to one account share one ?to= history.
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
