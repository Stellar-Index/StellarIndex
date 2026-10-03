package v1

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/notify"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// SignupResendThrottle is the v1 boundary for the per-address and
// per-IP budget on resent verification mail. Allow returns nil while
// both have room and [auth.ErrSignupRateLimited] once either is spent;
// any other error means the budget could not be consulted.
type SignupResendThrottle interface {
	Allow(ctx context.Context, ip, emailHash string) error
}

// signupResendDeliverTimeout bounds the detached token-issue + send.
const signupResendDeliverTimeout = 10 * time.Second

// SignupResendResult is the wire shape of POST /v1/signup/resend-verification.
// It is the same for every well-formed, in-budget request.
type SignupResendResult struct {
	Detail string `json:"detail"`
}

const signupResendDetail = "if that address has a signup awaiting verification, a new link is on its way and earlier links no longer work"

// handleSignupResend serves POST /v1/signup/resend-verification.
//
// The answer never depends on whether the address has an account: both
// budgets are spent first, the one inline store read is the same for
// every address, and the token issue + send run detached so the known
// path is not slower than the unknown one. Issuing a token retires the
// key's previous one ([auth.SignupVerifier.Reserve]).
func (s *Server) handleSignupResend(w http.ResponseWriter, r *http.Request) {
	if !s.requireSignupVerifier(w, r) {
		return
	}
	if s.signups == nil || s.signupVerifyEmailer == nil || s.signupResendThrottle == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/signup-verify-unavailable",
			"Signup verification not configured", http.StatusServiceUnavailable,
			"this deployment cannot send verification email")
		return
	}
	if !requireJSONContentType(w, r, "/v1/signup/resend-verification") {
		return
	}
	email, ok := parseResendEmail(w, r)
	if !ok {
		return
	}
	sum := sha256.Sum256([]byte(email))
	emailHash := hex.EncodeToString(sum[:])

	if !s.signupResendBudgetOK(w, r, emailHash) {
		return
	}
	keyID, err := s.signups.LookupByEmailHash(r.Context(), emailHash)
	if err != nil {
		s.logger.Error("signup resend: tracker lookup failed", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError,
			"resend failed; try again in a moment")
		return
	}
	// "pending" is the 5-minute placeholder of a signup still minting.
	if keyID != "" && keyID != "pending" {
		go s.deliverSignupResend(context.WithoutCancel(r.Context()), keyID, email)
	}
	writeJSON(w, SignupResendResult{Detail: signupResendDetail}, Flags{})
}

// deliverSignupResend issues the token and sends the mail; ctx is
// detached from the request, which has already been answered.
func (s *Server) deliverSignupResend(parent context.Context, keyID, email string) {
	defer worker.Recover(s.logger, "api-signup-resend")
	ctx, cancel := context.WithTimeout(parent, signupResendDeliverTimeout)
	defer cancel()
	s.issueSignupVerification(ctx, keyID, email)
}

// signupResendBudgetOK spends the resend budgets and reports whether to
// continue; on false the response is written. A budget that cannot be
// consulted fails closed because an accepted resend sends mail.
func (s *Server) signupResendBudgetOK(w http.ResponseWriter, r *http.Request, emailHash string) bool {
	err := s.signupResendThrottle.Allow(r.Context(), middleware.RemoteIP(r), emailHash)
	if err == nil {
		return true
	}
	if errors.Is(err, auth.ErrSignupRateLimited) {
		w.Header().Set("Retry-After", "3600")
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/signup-rate-limited",
			"Resend rate limit exceeded", http.StatusTooManyRequests,
			"too many verification resends for this address or IP; wait an hour and try again")
		return false
	}
	s.logger.Warn("signup resend throttle unavailable; failing closed", "err", err)
	w.Header().Set("Retry-After", "30")
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/throttle-unavailable",
		"Throttle layer unavailable", http.StatusServiceUnavailable,
		"the abuse-prevention layer is unreachable; retry in a moment")
	return false
}

// parseResendEmail reads {"email": …} and returns the canonical address.
func parseResendEmail(w http.ResponseWriter, r *http.Request) (string, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, signupBodyMaxBytes))
	var req signupRequest
	if err == nil {
		err = json.Unmarshal(body, &req)
	}
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-body",
			"Malformed JSON body", http.StatusBadRequest,
			"the body must be a JSON object under 4 KiB with an 'email' field")
		return "", false
	}
	canon, err := notify.CanonicalRecipient(strings.ToLower(strings.TrimSpace(req.Email)))
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-email",
			"Invalid email", http.StatusBadRequest,
			"the email field could not be parsed as a valid address")
		return "", false
	}
	return canon, true
}
