// Package sep10 implements the server side of Stellar Ecosystem
// Proposal 10 (Web Authentication). Conforms to the SEP-10 spec at
// https://github.com/stellar/stellar-protocol/blob/master/ecosystem/sep-0010.md.
//
// The Validator wraps three protocol functions:
//
//   - Challenge — issue a SEP-10-conformant Stellar transaction the
//     client must sign. The transaction is unsigned and never
//     submitted to the network; it's a structured nonce.
//
//   - Verify — accept a signed challenge transaction. Validates
//     structure (server signer, time bounds, manage-data ops, web
//     auth domain), cryptographic signatures, and freshness. Issues
//     a JWT bearing the authenticated G-strkey on success.
//
//   - VerifyJWT — validate a JWT issued by Verify on subsequent
//     requests. Returns the [auth.Subject] for the request context.
//
// Dependencies:
//
//   - txnbuild from go-stellar-sdk for the Stellar XDR + signature
//     plumbing. We don't reimplement BuildChallengeTx /
//     VerifyChallengeTxSigners — the SDK is the canonical
//     implementation.
//   - crypto/hmac + crypto/sha256 for the JWT — hand-rolled
//     HMAC-SHA256 over a JSON body. No third-party JWT library is
//     pulled in for the v1 launch; once we have a clear need for
//     RS256 / public-key JWT (e.g. cross-service issuance) the
//     dependency lands then.
//
// # Signer and threshold check
//
// Verify loads the client account's current signers and medium threshold
// through an [AccountLoader] (in the API binary: the ClickHouse lake's
// current ledger-entry state, never Horizon). An account that exists on
// chain must be signed by its own non-zero-weight signers with combined
// weight meeting its medium threshold, so a master key rotated to weight 0
// no longer authenticates and a multisig account's cosigners can. An
// account not yet on chain can only be proven by its master key. A failed
// lookup fails the verification closed. The check is only as fresh as the
// lake: a signer change the lake has not yet ingested is not seen.
//
// HTTP handlers live in internal/api/v1 and call the Validator via
// the [auth.SEP10Validator] interface in the parent package.
package sep10
