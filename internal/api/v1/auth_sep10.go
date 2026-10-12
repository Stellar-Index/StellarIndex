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

// sep10UnavailableDetail is the shared 404 body and must stand alone: WHY (no working validator) and WHAT TO DO (use an API key). It names
// no single cause because resolveSEP10Validator installs the Noop for ANY construction failure (real error in the startup log). Enabling
// SEP-10 SWAPS the credential type: middleware.authenticate is a mutually exclusive switch over auth_mode.
const sep10UnavailableDetail = "no SEP-10 validator is wired on this deployment, " +
	"so it cannot issue or verify SEP-10 challenges; this is a deployment posture, not an outage. " +
	"Authenticate with an API key instead (Authorization: Bearer sip_…) — see https://stellarindex.io/sdk. " +
	"Enabling SEP-10 is an operator action. It needs a server signing seed (S… secret seed) in " +
	"STELLARINDEX_SEP10_SEED, a JWT secret of at least 32 bytes in STELLARINDEX_SEP10_JWT_SECRET, " +
	"web_auth_domain and home_domain set, and Redis for the replay guard; the API's startup log line " +
	"\"sep10 validator not wired\" names the one that failed. Then switch auth_mode to sep10 — which " +
	"turns sip_* API keys OFF, since a deployment verifies one credential type or the other, never both."

// writeSEP10Unavailable answers the 404 that says SEP-10 is not
// offered here.
func writeSEP10Unavailable(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r,
		sep10UnavailableType,
		"SEP-10 not configured", http.StatusNotFound,
		sep10UnavailableDetail)
}

// handleSEP10Challenge serves GET /v1/auth/sep10/challenge?account=G…, returning the challenge transaction to sign (SEP-10
// §3.2) plus issued_at/valid_until. Deliberately unauthenticated: SEP-10 bootstraps auth from a public G-strkey.
func (s *Server) handleSEP10Challenge(w http.ResponseWriter, r *http.Request) {
	if s.SEP10 == nil {
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

	ch, err := s.SEP10.Challenge(r.Context(), account)
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

// sep10TokenResponse is the POST /v1/auth/sep10/token body: SEP-10 §3.4 `token` plus expires_at and the G-strkey, which do not
// break the contract.
type sep10TokenResponse struct {
	Token     string   `json:"token"`
	ExpiresAt WireTime `json:"expires_at"`
	Account   string   `json:"account"`
}

// handleSEP10Token serves POST /v1/auth/sep10/token (JSON `{"transaction": "<base64-XDR>"}`, SEP-10 §3.3). Errors: 400 missing
// body/field or malformed XDR (auth.ErrTokenMalformed); 401 bad signature/signers/account (auth.ErrUnauthorized); 410 expired
// time-bounds (auth.ErrTokenExpired); 404 validator not wired (sep10-unavailable); 500 otherwise.
func (s *Server) handleSEP10Token(w http.ResponseWriter, r *http.Request) {
	if s.SEP10 == nil {
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

	tok, err := s.SEP10.Verify(r.Context(), req.Transaction)
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
// SEP10Validator.Verify into RFC 9457 problem+json responses, in one place.
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
		// The no-op validator stub (signing seed not configured) returns ErrNotImplemented.
		// That is a deployment state, not a crash, so it is a non-retryable 404 with a detail
		// instead of falling through to a misleading 500.
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
