package sdex

import (
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// ClaimDrop records one claim atom the SDEX decoder rejected: a trade in stellar-core's op
// result (and in external counts like Hubble's) that we do NOT emit. Reason is the decoder's
// own error string.
type ClaimDrop struct {
	AtomType int32
	Reason   string
}

// AuditOp runs every claim atom of one operation through the SAME path the live decoder uses
// (extractClaimAtoms → decodeClaimAtom) and reports the claim count plus the dropped claims
// with reasons. Trades emitted = claims − len(drops); non-trade ops give claims == 0.
//
// It explains SDEX trade-count gaps against external anchors (Hubble): drops are exactly the
// off-by-N. Sharing the production path keeps the audit from disagreeing with the decoder.
func AuditOp(op xdr.Operation, result xdr.OperationResult) (claims int, drops []ClaimDrop) {
	atoms := extractClaimAtoms(op, result)
	for i := range atoms {
		if _, err := decodeClaimAtom(atoms[i], 0, time.Time{}, "", 0, i, ""); err != nil {
			drops = append(drops, ClaimDrop{AtomType: int32(atoms[i].Type), Reason: err.Error()})
		}
	}
	return len(atoms), drops
}
