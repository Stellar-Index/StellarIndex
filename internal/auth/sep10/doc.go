// Package sep10 is the server side of SEP-10 Web Authentication
// (https://github.com/stellar/stellar-protocol/blob/master/ecosystem/sep-0010.md):
// Challenge issues an unsigned, never-submitted transaction as a nonce;
// Verify checks structure, signatures and freshness and issues an
// HMAC-SHA256 JWT for the G-address; VerifyJWT turns that JWT back into
// an [auth.Subject]. Transaction plumbing is the SDK's txnbuild.
//
// Verify loads the account's signers and medium threshold through an
// [AccountLoader] (in the API: lake ledger-entry state, never Horizon).
// An on-chain account must be signed by non-zero-weight signers meeting
// the medium threshold, so a master key at weight 0 fails and multisig
// cosigners pass; an account not yet on chain needs its master key. A
// failed lookup fails closed, and a signer change the lake has not yet
// ingested is not seen.
package sep10
