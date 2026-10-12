package sdex

import (
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// ClaimDrop records one claim atom the SDEX decoder rejected: a trade in stellar-core's op result (and external counts like Hubble's) that we do NOT emit. Reason is the decoder's error string.
type ClaimDrop struct {
	AtomType int32
	Reason   string
}

// AuditOp runs every claim atom of one op through the SAME path as the live decoder (extractClaimAtoms, decodeClaimAtom) and reports the claim count plus dropped claims with reasons (trades emitted = claims - len(drops)); sharing the production path keeps the audit from disagreeing with the decoder.
func AuditOp(op xdr.Operation, result xdr.OperationResult) (claims int, drops []ClaimDrop) {
	atoms := extractClaimAtoms(op, result)
	for i := range atoms {
		if _, err := decodeClaimAtom(atoms[i], 0, time.Time{}, "", 0, i, ""); err != nil {
			drops = append(drops, ClaimDrop{AtomType: int32(atoms[i].Type), Reason: err.Error()})
		}
	}
	return len(atoms), drops
}
