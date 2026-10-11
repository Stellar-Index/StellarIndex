// Package sep10 is the server side of SEP-10 Web Authentication: Challenge issues an unsigned nonce transaction, Verify checks structure, signatures and freshness and issues an HMAC-SHA256 JWT, VerifyJWT turns it back into an [auth.Subject].
// Signers and the medium threshold come from an [AccountLoader] (lake ledger-entry state, never Horizon): on-chain accounts need non-zero-weight signers meeting the threshold, new accounts their master key.
// A failed lookup fails closed; a signer change the lake has not ingested is not seen.
package sep10
