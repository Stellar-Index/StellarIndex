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
// # Deliberate limitation: master-key-only authentication (SEC-02)
//
// Verify authenticates a signature from the CLIENT ACCOUNT'S MASTER KEY
// and nothing else. It never fetches the account from Horizon, so it
// cannot see the account's on-chain signer set or thresholds. SEP-10
// §4.4 makes the on-chain check the recommended flow for accounts that
// EXIST on the network; skipping it has two consequences an operator
// must accept before enabling `auth_mode = sep10`:
//
//   - A master key the account holder has ROTATED AWAY from (weight set
//     to 0, control moved to other signers) still authenticates here.
//     For an account that has rotated because the old key was
//     compromised, that is an authentication bypass — the network
//     considers the key powerless, this validator does not.
//   - Conversely, a multisig account whose master key cannot alone meet
//     the medium threshold is LOCKED OUT: its legitimate signers'
//     signatures are rejected as "unrecognized".
//
// Both are acceptable only while the SEP-10 user base is
// single-signer/master-key accounts (the launch posture: SEP-10 exists
// to identify a wallet, not to authorise value movement, and every
// privileged surface is behind API keys, not SEP-10 JWTs). Closing this
// properly requires a Horizon account lookup inside Verify —
// VerifyChallengeTxThreshold + the account's signer set, with a
// fail-CLOSED posture when Horizon is unreachable — plus the operator
// wiring and timeout budget that implies. That is a cross-package change
// (validator + binary wiring + config) and is NOT implemented here; do
// not read the absence of a TODO as the absence of the constraint.
//
// HTTP handlers live in internal/api/v1 and call the Validator via
// the [auth.SEP10Validator] interface in the parent package.
package sep10
