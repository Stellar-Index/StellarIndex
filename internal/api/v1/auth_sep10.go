package v1

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// sep10UnavailableType is the problem type every "SEP-10 is not on
// here" answer carries.
const sep10UnavailableType = "https://api.stellarindex.io/errors/sep10-unavailable"

// sep10UnavailableDetail is the body those answers carry.
//
// A caller who reaches this has read a document that told them the
// flow exists — /sdk, the getting-started guide, the SDK's godoc, the
// spec — and every one of those now says it is off here. This is the
// one surface that answers a caller who did not read them, so it has
// to stand on its own: WHY the route refuses (no signing seed, so
// there is nothing to sign a challenge with — not an outage, not a
// bad request), and WHAT TO DO INSTEAD (an API key, which is the
// credential this deployment actually verifies).
//
// The four call sites used to carry two different strings, the
// terser of which said only "no SEP-10 validator wired" — a
// restatement of the error code, which tells a caller nothing they
// could act on. One constant means those branches cannot drift apart
// again.
//
// The last sentence is for the operator reading their own logs, and
// is deliberate about the cost: enabling SEP-10 does not ADD a
// credential type, it SWAPS the deployment's one credential type,
// because middleware.authenticate is a mutually exclusive switch over
// auth_mode. A deployment verifies sip_* keys or SEP-10 JWTs, never
// both, and an operator who reads this as "turn it on as well" would
// break every existing key holder.
const sep10UnavailableDetail = "no SEP-10 validator is wired on this deployment: " +
	"no server signing seed is configured, so there is nothing to sign a challenge with. " +
	"Authenticate with an API key instead (Authorization: Bearer sip_…) — see https://stellarindex.io/sdk. " +
	"Enabling SEP-10 is an operator action: set STELLARINDEX_SEP10_SEED and STELLARINDEX_SEP10_JWT_SECRET " +
	"on a deployment with Redis, and switch auth_mode to sep10 — which turns sip_* API keys OFF, " +
	"since a deployment verifies one credential type or the other, never both."

// writeSEP10Unavailable answers the 503 that says SEP-10 is not
// offered here.
func writeSEP10Unavailable(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r,
		sep10UnavailableType,
		"SEP-10 not configured", http.StatusServiceUnavailable,
		sep10UnavailableDetail)
}

// handleSEP10Challenge serves GET /v1/auth/sep10/challenge?account=G…
//
// Returns the SEP-10 challenge transaction the client must sign with
// its account secret key. Per SEP-10 §3.2, the response shape is the
// stellar.toml-spec'd { transaction, network_passphrase } pair plus
// our own valid_until + issued_at convenience fields.
//
// This endpoint is deliberately unauthenticated — the whole point of
// SEP-10 is to bootstrap auth from a public Stellar G-strkey.
func (s *Server) handleSEP10Challenge(w http.ResponseWriter, r *http.Request) {
	if s.sep10 == nil {
		writeSEP10Unavailable(w, r)
		return
	}

	account := r.URL.Query().Get("account")
	if account == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-account",
			"Missing account parameter", http.StatusBadRequest,
			"account query parameter is required (G-strkey)")
		return
	}

	ch, err := s.sep10.Challenge(r.Context(), account)
	if err != nil {
		if errors.Is(err, auth.ErrUnauthorized) {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-account",
				"Invalid account", http.StatusBadRequest,
				"account must be a valid Stellar G-strkey")
			return
		}
		if errors.Is(err, auth.ErrNotImplemented) {
			writeSEP10Unavailable(w, r)
			return
		}
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("SEP-10 challenge failed", "err", err, "account", account)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}

	// SEP-10-spec field names on the wire: `transaction`,
	// `network_passphrase`. Our envelope's data field carries them.
	writeJSON(w, sep10ChallengeResponse{
		Transaction:       ch.TransactionXDR,
		NetworkPassphrase: ch.NetworkPassphrase,
		IssuedAt:          WireTime(ch.IssuedAt),
		ValidUntil:        WireTime(ch.ValidUntil),
	}, Flags{})
}

// sep10ChallengeResponse is the wire shape for /v1/auth/sep10/challenge.
type sep10ChallengeResponse struct {
	// Transaction is the base64-encoded XDR of the unsigned challenge.
	// Field name `transaction` per SEP-10 §3.2.
	Transaction string `json:"transaction"`

	// NetworkPassphrase echoes the network the challenge was crafted
	// for. Field name `network_passphrase` per SEP-10 §3.2.
	NetworkPassphrase string `json:"network_passphrase"`

	// IssuedAt + ValidUntil — convenience fields not in SEP-10 itself
	// but useful for client UIs that need to display "challenge
	// expires in N minutes".
	IssuedAt   WireTime `json:"issued_at"`
	ValidUntil WireTime `json:"valid_until"`
}

// sep10TokenRequest is the wire shape for POST /v1/auth/sep10/token.
// Field name `transaction` per SEP-10 §3.3.
type sep10TokenRequest struct {
	Transaction string `json:"transaction"`
}

// sep10TokenResponse is the wire shape for POST /v1/auth/sep10/token.
// SEP-10 §3.4 specifies a `token` field. We additionally return
// expires_at + the authenticated G-strkey for client convenience —
// neither breaks the SEP-10 contract.
type sep10TokenResponse struct {
	Token     string   `json:"token"`
	ExpiresAt WireTime `json:"expires_at"`
	Account   string   `json:"account"`
}

// handleSEP10Token serves POST /v1/auth/sep10/token.
//
// Accepts a signed challenge transaction in the body, validates it
// via the Validator, and returns a JWT. Per SEP-10 §3.3 the body is
// JSON `{"transaction": "<base64-XDR>"}`. The current handler only
// implements the JSON form reflected in OpenAPI.
//
// Errors:
//   - 400 — body missing or transaction field absent
//   - 400 — XDR malformed (auth.ErrTokenMalformed)
//   - 401 — signature wrong / no signers / wrong account
//     (auth.ErrUnauthorized)
//   - 410 — challenge time-bounds expired (auth.ErrTokenExpired)
//   - 503 — validator not wired
//   - 500 — anything else
func (s *Server) handleSEP10Token(w http.ResponseWriter, r *http.Request) {
	if s.sep10 == nil {
		writeSEP10Unavailable(w, r)
		return
	}

	const maxBody = 64 * 1024 // signed XDRs are typically <2 KiB; 64 KiB is generous
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/body-too-large",
			"Request body too large", http.StatusBadRequest,
			"/v1/auth/sep10/token body must be under 64 KiB")
		return
	}
	var req sep10TokenRequest
	if len(body) == 0 {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-body",
			"Request body required", http.StatusBadRequest,
			"body must be JSON: {\"transaction\":\"<base64-XDR>\"}")
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-body",
			"Malformed JSON body", http.StatusBadRequest,
			"could not parse request body as JSON")
		return
	}
	if req.Transaction == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/missing-transaction",
			"Missing transaction", http.StatusBadRequest,
			"transaction field is required (base64-encoded signed XDR)")
		return
	}

	tok, err := s.sep10.Verify(r.Context(), req.Transaction)
	if err != nil {
		s.writeSEP10VerifyError(w, r, err)
		return
	}

	writeJSON(w, sep10TokenResponse{
		Token:     tok.JWT,
		ExpiresAt: WireTime(tok.ExpiresAt),
		Account:   tok.Subject.Identifier,
	}, Flags{})
}

// writeSEP10VerifyError translates the typed errors from
// SEP10Validator.Verify into RFC 9457 problem+json responses. Kept
// separate from the handler to keep cyclomatic complexity reasonable
// and let the error-mapping table evolve in one place.
func (s *Server) writeSEP10VerifyError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrTokenExpired):
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/sep10-challenge-expired",
			"Challenge expired", http.StatusGone,
			"the SEP-10 challenge's time-bound window has elapsed; request a fresh challenge")
	case errors.Is(err, auth.ErrTokenMalformed):
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/sep10-malformed-transaction",
			"Malformed challenge transaction", http.StatusBadRequest,
			"the supplied transaction XDR could not be parsed as a SEP-10 challenge")
	case errors.Is(err, auth.ErrUnauthorized):
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/sep10-verification-failed",
			"Challenge verification failed", http.StatusUnauthorized,
			"the supplied signed transaction did not pass SEP-10 verification (signature missing or wrong account)")
	case errors.Is(err, auth.ErrNotImplemented):
		// Mirrors the challenge handler's ErrNotImplemented branch
		// — when the server-side validator is the no-op stub
		// (sep10 wired but signing seed not configured), Verify
		// returns ErrNotImplemented. Pre-fix this fell through to
		// the default branch and surfaced as 500 + "Internal
		// error" — misleading because it's a deployment config
		// state, not a server crash. Surfacing it as 503 with a
		// detail makes the operator-side fix obvious.
		writeSEP10Unavailable(w, r)
	default:
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("SEP-10 verify failed", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
	}
}
