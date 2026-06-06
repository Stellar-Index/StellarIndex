package sdex

import (
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// ClaimDrop records one claim atom that the SDEX decoder rejected — i.e. a
// trade present in stellar-core's op result (and counted by external sources
// like Hubble's history_trades) that we do NOT emit. Reason is the decoder's
// own error string, which for the non-positive-amount guard already embeds the
// sold/bought amounts.
type ClaimDrop struct {
	AtomType int32
	Reason   string
}

// AuditOp runs every claim atom of one operation through the SAME path the live
// decoder uses (extractClaimAtoms → decodeClaimAtom) and reports the total claim
// count plus the claims that were dropped, with reasons. Trades we emit =
// claims − len(drops). For non-trade ops, claims == 0.
//
// This is the diagnostic seam for explaining SDEX trade-count gaps against
// external anchors (Hubble): drops here are exactly the off-by-N. Reuses the
// production decode logic so the audit can't disagree with the real decoder for
// the wrong reason.
func AuditOp(op xdr.Operation, result xdr.OperationResult) (claims int, drops []ClaimDrop) {
	atoms := extractClaimAtoms(op, result)
	for i := range atoms {
		if _, err := decodeClaimAtom(atoms[i], 0, time.Time{}, "", 0, i, ""); err != nil {
			drops = append(drops, ClaimDrop{AtomType: int32(atoms[i].Type), Reason: err.Error()})
		}
	}
	return len(atoms), drops
}
