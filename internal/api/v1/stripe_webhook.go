package v1

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/RatesEngine/rates-engine/internal/auth"
)

// StripeKeyManager is the v1 boundary for the Stripe webhook
// handler's key-mutation needs. Implementation:
// [auth.RedisAPIKeyStore] (which provides all three methods).
type StripeKeyManager interface {
	ListKeysForIdentifier(ctx context.Context, identifier string) ([]auth.APIKeyRecord, error)
	UpdateRateLimit(ctx context.Context, keyID string, newRateLimitPerMin int) (auth.APIKeyRecord, error)
}

// StripeWebhookConfig wires the handler. SigningSecret is the
// `whsec_…` value from the Stripe dashboard — used to validate the
// Stripe-Signature header per
// https://docs.stripe.com/webhooks#verify-events. Empty makes the
// handler reject every request 503 (no signing secret = no way to
// trust the payload).
//
// Manager handles the actual key-mutation (read keys for an
// identifier + lift their rate-limit). Production wiring is
// [auth.RedisAPIKeyStore] which provides both methods on
// [StripeKeyManager].
type StripeWebhookConfig struct {
	SigningSecret string
	Manager       StripeKeyManager
	// Now is overridable for tests; defaults to time.Now.
	Now func() time.Time
	// MaxAge is the maximum Stripe-Signature timestamp drift accepted
	// (rejects replays). Default 5 min.
	MaxAge time.Duration
}

// stripeTierMap controls which tier a Stripe metadata.tier value
// upgrades to. Keep in lock-step with the /signup page tier table:
//
//	starter   →  1000 req/min  (free; not actually meaningful here)
//	pro       → 10000 req/min
//	business  → 50000 req/min
//	enterprise → caller specifies via metadata.rate_limit_per_min override
var stripeTierMap = map[string]int{
	"starter":  1000,
	"pro":      10000,
	"business": 50000,
}

// stripeEvent is the minimal Stripe event shape we consume.
// Stripe's full event shape is enormous; we only inspect the
// fields the webhook flow needs.
type stripeEvent struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		Object stripeCheckoutSession `json:"object"`
	} `json:"data"`
}

type stripeCheckoutSession struct {
	ID                string            `json:"id"`
	ClientReferenceID string            `json:"client_reference_id"` // we set this to the customer's identifier
	CustomerEmail     string            `json:"customer_email"`
	PaymentStatus     string            `json:"payment_status"`
	Metadata          map[string]string `json:"metadata"`
}

// handleStripeWebhook serves POST /v1/webhooks/stripe.
//
// Validates the Stripe-Signature header per the documented
// HMAC-SHA256 scheme, parses the event, and on
// `checkout.session.completed` upgrades every key belonging to the
// identifier in `client_reference_id` to the per-tier
// rate-limit. Idempotent on Stripe's side via Stripe's at-least-
// once delivery + the webhook handler's read-then-write semantics
// (subsequent identical events re-set the same RateLimitPerMin).
//
// Stripe metadata fields consumed:
//
//	tier                    one of starter / pro / business
//	rate_limit_per_min      optional integer override (Enterprise)
//
// Both are operator-set on the Stripe Checkout session create call.
//
// Returns 200 + body `{"ok": true, "upgraded": N}` on success.
// Stripe replays webhooks until it gets a 2xx; non-2xx triggers
// retries.
func (s *Server) handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	ev, ok := s.parseStripeWebhook(w, r)
	if !ok {
		return
	}

	// Only react to checkout.session.completed today. Other event
	// types acknowledge with 200 so Stripe stops retrying.
	if ev.Type != "checkout.session.completed" {
		s.logger.Info("stripe webhook: ignored event type",
			"type", ev.Type, "event_id", ev.ID)
		writeJSON(w, map[string]any{"ok": true, "ignored": ev.Type}, Flags{})
		return
	}

	session := ev.Data.Object
	if session.PaymentStatus != "paid" {
		s.logger.Info("stripe webhook: checkout.session.completed but payment_status != paid",
			"event_id", ev.ID, "session_id", session.ID, "payment_status", session.PaymentStatus)
		writeJSON(w, map[string]any{"ok": true, "ignored": "unpaid"}, Flags{})
		return
	}

	identifier := strings.TrimSpace(session.ClientReferenceID)
	if identifier == "" {
		s.logger.Error("stripe webhook: client_reference_id missing",
			"event_id", ev.ID, "session_id", session.ID, "email", session.CustomerEmail)
		writeProblem(w, r,
			"https://api.ratesengine.net/errors/stripe-missing-identifier",
			"client_reference_id missing", http.StatusBadRequest,
			"Stripe Checkout sessions must set client_reference_id to the customer's signup identifier (e.g. signup-abc123); the webhook can't route the upgrade without it")
		return
	}

	tierName := strings.ToLower(strings.TrimSpace(session.Metadata["tier"]))
	rateLimit, ok := stripeTierMap[tierName]
	if !ok {
		// Allow an explicit override via metadata.rate_limit_per_min
		// for Enterprise / custom plans.
		if override := strings.TrimSpace(session.Metadata["rate_limit_per_min"]); override != "" {
			n, err := strconv.Atoi(override)
			if err != nil || n < 0 {
				s.logger.Error("stripe webhook: bad rate_limit_per_min override",
					"event_id", ev.ID, "value", override, "err", err)
				writeProblem(w, r,
					"https://api.ratesengine.net/errors/stripe-bad-metadata",
					"Bad rate_limit_per_min metadata", http.StatusBadRequest,
					"metadata.rate_limit_per_min must be a non-negative integer")
				return
			}
			rateLimit = n
		} else {
			s.logger.Error("stripe webhook: unknown tier + no override",
				"event_id", ev.ID, "tier", tierName,
				"valid_tiers", "pro|business")
			writeProblem(w, r,
				"https://api.ratesengine.net/errors/stripe-bad-metadata",
				"Unknown tier", http.StatusBadRequest,
				"metadata.tier must be one of pro/business, OR metadata.rate_limit_per_min must be set")
			return
		}
	}

	// Upgrade every key the customer holds. This is idempotent —
	// Stripe at-least-once delivery means the same event may arrive
	// multiple times; we always set the same target rate-limit.
	keys, err := s.stripe.Manager.ListKeysForIdentifier(r.Context(), identifier)
	if err != nil {
		s.logger.Error("stripe webhook: list keys failed",
			"err", err, "identifier", identifier, "event_id", ev.ID)
		writeProblem(w, r,
			"https://api.ratesengine.net/errors/internal",
			"Internal error", http.StatusInternalServerError,
			"could not look up customer keys; Stripe will retry")
		return
	}
	if len(keys) == 0 {
		s.logger.Warn("stripe webhook: no keys for identifier (customer paid but never signed up?)",
			"identifier", identifier, "event_id", ev.ID,
			"email", session.CustomerEmail)
		// Acknowledge — there's nothing to upgrade. Operator triages
		// out-of-band (refund? ask customer to sign up?). Refusing
		// would just trigger Stripe retries.
		writeJSON(w, map[string]any{"ok": true, "upgraded": 0, "note": "no keys for identifier"}, Flags{})
		return
	}

	upgraded := 0
	for _, k := range keys {
		if _, err := s.stripe.Manager.UpdateRateLimit(r.Context(), k.KeyID, rateLimit); err != nil {
			s.logger.Error("stripe webhook: upgrade failed for one key",
				"err", err, "key_id", k.KeyID, "identifier", identifier, "event_id", ev.ID)
			// Continue with the others — partial success is better
			// than failing the whole webhook (which would trigger
			// Stripe retries that attempt the same upgrades again,
			// some of which already succeeded). Operator sees the
			// per-key error in the log and reconciles out-of-band.
			continue
		}
		upgraded++
	}

	s.logger.Info("stripe webhook: customer upgraded",
		"identifier", identifier, "event_id", ev.ID,
		"tier", tierName, "rate_limit_per_min", rateLimit,
		"keys_total", len(keys), "keys_upgraded", upgraded)

	writeJSON(w, map[string]any{
		"ok":                 true,
		"upgraded":           upgraded,
		"keys_total":         len(keys),
		"rate_limit_per_min": rateLimit,
	}, Flags{})
}

// parseStripeWebhook handles the auth + body-shape validation for
// the Stripe webhook handler. Returns the parsed event + ok=true
// on success; on failure the response has already been written and
// ok=false signals the caller to bail. Extracted so handleStripeWebhook
// stays under the gocognit threshold.
func (s *Server) parseStripeWebhook(w http.ResponseWriter, r *http.Request) (stripeEvent, bool) {
	if s.stripe == nil {
		writeProblem(w, r,
			"https://api.ratesengine.net/errors/stripe-not-configured",
			"Stripe webhook not configured", http.StatusServiceUnavailable,
			"this deployment has no Stripe signing secret wired — set [api.stripe].signing_secret to enable webhooks")
		return stripeEvent{}, false
	}
	if s.stripe.SigningSecret == "" {
		writeProblem(w, r,
			"https://api.ratesengine.net/errors/stripe-not-configured",
			"Stripe webhook signing secret is empty", http.StatusServiceUnavailable,
			"signing secret unset — webhooks rejected to prevent unauthenticated upgrades")
		return stripeEvent{}, false
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20)) // 1 MiB
	if err != nil {
		writeProblem(w, r,
			"https://api.ratesengine.net/errors/body-too-large",
			"Request body too large", http.StatusBadRequest,
			"Stripe webhook body must be under 1 MiB")
		return stripeEvent{}, false
	}

	sigHeader := r.Header.Get("Stripe-Signature")
	if sigHeader == "" {
		writeProblem(w, r,
			"https://api.ratesengine.net/errors/stripe-signature-missing",
			"Stripe-Signature header missing", http.StatusBadRequest,
			"every Stripe webhook delivery must carry a Stripe-Signature header; absence implies the request didn't come from Stripe")
		return stripeEvent{}, false
	}

	if err := verifyStripeSignature(sigHeader, body, s.stripe.SigningSecret, s.stripeNow(), s.stripeMaxAge()); err != nil {
		// Cap the header preview so log lines stay sane.
		preview := sigHeader
		if len(preview) > 40 {
			preview = preview[:40]
		}
		s.logger.Warn("stripe webhook signature verification failed",
			"err", err, "signature_header", preview)
		writeProblem(w, r,
			"https://api.ratesengine.net/errors/stripe-signature-invalid",
			"Stripe-Signature invalid", http.StatusUnauthorized,
			"signature verification failed; ensure the signing secret matches the dashboard's whsec_… value")
		return stripeEvent{}, false
	}

	var ev stripeEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		writeProblem(w, r,
			"https://api.ratesengine.net/errors/invalid-body",
			"Malformed Stripe event", http.StatusBadRequest,
			"could not parse webhook body as a Stripe event")
		return stripeEvent{}, false
	}
	return ev, true
}

// stripeNow returns the configured clock or time.Now.
func (s *Server) stripeNow() time.Time {
	if s.stripe != nil && s.stripe.Now != nil {
		return s.stripe.Now()
	}
	return time.Now()
}

func (s *Server) stripeMaxAge() time.Duration {
	if s.stripe != nil && s.stripe.MaxAge > 0 {
		return s.stripe.MaxAge
	}
	return 5 * time.Minute
}

// verifyStripeSignature implements the documented Stripe webhook
// signature scheme:
//
//	Stripe-Signature: t=<unix-ts>,v1=<hex(hmac-sha256(secret, "<ts>.<body>"))>
//
// Per https://docs.stripe.com/webhooks#verify-events. Multiple
// `v1=` entries can appear (Stripe rolls signing secrets); we
// accept if ANY matches.
func verifyStripeSignature(header string, body []byte, secret string, now time.Time, maxAge time.Duration) error {
	var ts int64
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return fmt.Errorf("malformed timestamp: %w", err)
			}
			ts = n
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if ts == 0 {
		return errors.New("missing t= timestamp")
	}
	if len(sigs) == 0 {
		return errors.New("missing v1= signature")
	}

	// Replay protection: reject anything outside the maxAge window.
	signedAt := time.Unix(ts, 0)
	skew := now.Sub(signedAt)
	if skew < 0 {
		skew = -skew
	}
	if skew > maxAge {
		return fmt.Errorf("timestamp drift %s exceeds %s", skew, maxAge)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))

	for _, got := range sigs {
		if hmac.Equal([]byte(got), []byte(want)) {
			return nil
		}
	}
	return errors.New("no v1= matched expected HMAC")
}
