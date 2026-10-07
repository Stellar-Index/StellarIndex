package dashboardwebhooks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/dashboardauth"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/api/wiretime"
	"github.com/Stellar-Index/StellarIndex/internal/httpx"
	"github.com/Stellar-Index/StellarIndex/internal/nettools"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// The webhook ceiling is tier-aware: platform.Tier.MaxWebhooks is
// the default ladder (free 10 → partner 100), overridable per
// tier via Config.WebhookQuotas. This replaced the flat 10-webhook
// MaxWebhooksPerAccount cap ("tier-aware quotas can replace this
// once billing is wired — Phase 2").

// Config wires the handlers' dependencies.
type Config struct {
	// Webhooks is the platform store powering CRUD + queue. In
	// production: `internal/platform/postgresstore.WebhookStore`.
	Webhooks platform.WebhookStore
	Logger   *slog.Logger
	Now      func() time.Time

	// WebhookQuotas optionally overrides the per-tier ceiling on
	// registered webhook endpoints. Tiers absent from the map (or a
	// nil map, the production default) fall back to
	// [platform.Tier.MaxWebhooks]. Non-positive values are ignored.
	WebhookQuotas map[platform.Tier]int

	// idempotency backs the optional Idempotency-Key header on
	// HandleCreate: a client that retries a create after a timeout gets
	// the original response (and signing secret) replayed instead of
	// registering a second webhook. Lazily initialized by validate().
	idempotency *middleware.IdempotencyStore
}

func (c *Config) validate() error {
	if c.Webhooks == nil {
		return errors.New("dashboardwebhooks: Webhooks store is required")
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = func() time.Time { return time.Now().UTC() }
	}
	if c.idempotency == nil {
		c.idempotency = middleware.NewIdempotencyStore(0)
	}
	return nil
}

// Handlers exposes the routes to be mounted in the v1 mux.
type Handlers struct{ cfg *Config }

// NewHandlers validates the config and returns a mount-ready
// Handlers.
func NewHandlers(cfg Config) (*Handlers, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Handlers{cfg: &cfg}, nil
}

// Mount installs the dashboard webhook-management routes.
//
// Every route is wrapped in [dashboardauth.RequireSession], so an
// anonymous request is refused before any other layer runs; the
// handlers still read the session for its account.
//
// Every mutation is wrapped in [middleware.RequireSameSiteWrite]
// because these routes authenticate with the session
// COOKIE, so a cross-site page could otherwise drive them on a
// logged-in customer's behalf — registering an endpoint that
// exfiltrates the victim's webhook payloads is the concrete attack.
// Reads stay unwrapped — safe methods change nothing.
func (h *Handlers) Mount(mux *http.ServeMux, _ *middleware.PublicRoutes) {
	session := dashboardauth.RequireSession()
	sameSite := middleware.RequireSameSiteWrite(h.cfg.Logger)
	idem := middleware.Idempotency(h.cfg.idempotency, dashboardauth.SessionAccountSubject)
	mux.Handle("GET /v1/dashboard/webhooks", session(http.HandlerFunc(h.HandleList)))
	mux.Handle("POST /v1/dashboard/webhooks", session(sameSite(idem(http.HandlerFunc(h.HandleCreate)))))
	mux.Handle("PATCH /v1/dashboard/webhooks/{id}", session(sameSite(http.HandlerFunc(h.HandleUpdate))))
	mux.Handle("DELETE /v1/dashboard/webhooks/{id}", session(sameSite(http.HandlerFunc(h.HandleDelete))))
	mux.Handle("POST /v1/dashboard/webhooks/{id}/rotate-secret", session(sameSite(idem(http.HandlerFunc(h.HandleRotateSecret)))))
	mux.Handle("GET /v1/dashboard/webhooks/{id}/deliveries", session(http.HandlerFunc(h.HandleListDeliveries)))
}

// webhookDTO is the wire shape the dashboard reads.
//
// The signing key is persisted (the delivery worker needs it to sign),
// but it is returned by `POST /v1/dashboard/webhooks` exactly once and
// stays out of this DTO so no later read re-exposes it; rotation returns
// the new key once from HandleRotateSecret. See [platform.CustomerWebhook]
// for the at-rest model.
type webhookDTO struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	URL       string        `json:"url"`
	Events    []string      `json:"events"`
	Enabled   bool          `json:"enabled"`
	CreatedAt wiretime.Time `json:"created_at"`
	UpdatedAt wiretime.Time `json:"updated_at"`
}

func toDTO(w platform.CustomerWebhook) webhookDTO {
	return webhookDTO{
		ID:        w.ID.String(),
		Name:      w.Name,
		URL:       w.URL,
		Events:    w.Events,
		Enabled:   w.Enabled,
		CreatedAt: wiretime.Time(w.CreatedAt),
		UpdatedAt: wiretime.Time(w.UpdatedAt),
	}
}

type deliveryDTO struct {
	ID           string `json:"id"`
	EventType    string `json:"event_type"`
	AttemptCount int    `json:"attempt_count"`
	// Pointer times so a zero value (no retry scheduled / not yet
	// delivered) is genuinely omitted — omitempty does NOT omit a zero
	// time.Time (it's a non-empty struct).
	NextAttemptAt      *wiretime.Time `json:"next_attempt_at,omitempty"`
	DeliveredAt        *wiretime.Time `json:"delivered_at,omitempty"`
	LastError          string         `json:"last_error,omitempty"`
	LastResponseStatus int            `json:"last_response_status,omitempty"`
	CreatedAt          wiretime.Time  `json:"created_at"`
}

func toDeliveryDTO(d platform.WebhookDelivery) deliveryDTO {
	return deliveryDTO{
		ID:                 d.ID.String(),
		EventType:          d.EventType,
		AttemptCount:       d.AttemptCount,
		NextAttemptAt:      wiretime.NilIfZero(d.NextAttemptAt),
		DeliveredAt:        wiretime.NilIfZero(d.DeliveredAt),
		LastError:          d.LastError,
		LastResponseStatus: d.LastResponseStatus,
		CreatedAt:          wiretime.Time(d.CreatedAt),
	}
}

type listResponse struct {
	Webhooks []webhookDTO `json:"webhooks"`
}

// HandleList returns every webhook for the session's account,
// newest first.
func (h *Handlers) HandleList(w http.ResponseWriter, r *http.Request) {
	sc, ok := dashboardauth.SessionFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication required", r.URL.Path)
		return
	}
	if !canManage(sc.User.Role) {
		writeProblem(w, http.StatusForbidden, "your role can't view webhooks", r.URL.Path)
		return
	}
	hooks, err := h.cfg.Webhooks.ListWebhooksForAccount(r.Context(), sc.Account.ID)
	if err != nil {
		h.cfg.Logger.Error("list webhooks", "err", err, "account_id", sc.Account.ID)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	out := listResponse{Webhooks: make([]webhookDTO, 0, len(hooks))}
	for _, hk := range hooks {
		out.Webhooks = append(out.Webhooks, toDTO(hk))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

type createRequest struct {
	Name    string   `json:"name"`
	URL     string   `json:"url"`
	Events  []string `json:"events"`
	Enabled *bool    `json:"enabled,omitempty"` // pointer so absent → true default
}

type createResponse struct {
	Webhook webhookDTO `json:"webhook"`
	// Secret is the HMAC-SHA-256 signing key plaintext, returned
	// once at create + never again. The customer stores it
	// server-side + verifies each delivery by recomputing
	// HMAC-SHA-256(secret, X-StellarIndex-Timestamp + "." + rawBody)
	// against X-StellarIndex-Signature (sha256=…), and rejecting a
	// timestamp outside a tolerance window to bound replay.
	// X-StellarIndex-Signature-V2 also binds the Delivery-Id and Event
	// headers; the worker's signDeliveryHMACSHA256 documents it.
	Secret string `json:"secret"`
}

// HandleCreate registers a new webhook endpoint.
func (h *Handlers) HandleCreate(w http.ResponseWriter, r *http.Request) {
	sc, ok := dashboardauth.SessionFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication required", r.URL.Path)
		return
	}
	if !canManage(sc.User.Role) {
		writeProblem(w, http.StatusForbidden, "your role can't manage webhooks", r.URL.Path)
		return
	}

	req, status, problem := parseCreateRequest(r.Context(), r)
	if problem != "" {
		writeProblem(w, status, problem, r.URL.Path)
		return
	}

	// Fast-path UX only: the precheck is raceable, so the store
	// enforces `maxPerAccount` atomically inside the INSERT, which is
	// the actual gate. This surfaces the same 409 without a write.
	maxHooks := h.maxWebhooksFor(sc.Account.Tier)
	if status, problem := h.checkQuota(r, sc.Account.ID, maxHooks); problem != "" {
		writeProblem(w, status, problem, r.URL.Path)
		return
	}

	secret, err := generateSecret()
	if err != nil {
		h.cfg.Logger.Error("generate webhook secret", "err", err)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	rec := platform.CustomerWebhook{
		AccountID: sc.Account.ID,
		Name:      req.Name,
		URL:       req.URL,
		// The customer receives the plaintext `secret` exactly once
		// in the response below; HandleRotateSecret replaces it in place.
		SigningKey: []byte(secret),
		Events:     req.Events,
		Enabled:    enabled,
	}
	out, err := h.cfg.Webhooks.CreateWebhook(r.Context(), rec, maxHooks)
	if err != nil {
		// Race-window loser. The atomic gate inside
		// CreateWebhook returns ErrWebhookQuotaExceeded when the
		// account hits the cap between the precheck and the
		// INSERT.
		if errors.Is(err, platform.ErrWebhookQuotaExceeded) {
			writeProblem(w, http.StatusConflict,
				fmt.Sprintf("account already has %d webhooks (max %d for the %s tier)", maxHooks, maxHooks, sc.Account.Tier),
				r.URL.Path)
			return
		}
		if errors.Is(err, platform.ErrConflict) {
			writeProblem(w, http.StatusConflict, errDuplicateURL, r.URL.Path)
			return
		}
		h.cfg.Logger.Error("create webhook in postgres", "err", err, "account_id", sc.Account.ID)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, createResponse{Webhook: toDTO(out), Secret: secret})
}

// errDuplicateURL is the 409 detail for a URL the account has already
// registered (UNIQUE (account_id, url), migration 0180).
const errDuplicateURL = "this account already has a webhook registered for that url"

type updateRequest struct {
	Name    *string  `json:"name,omitempty"`
	URL     *string  `json:"url,omitempty"`
	Events  []string `json:"events,omitempty"`
	Enabled *bool    `json:"enabled,omitempty"`
}

// HandleUpdate patches mutable fields. SigningKey + AccountID are
// immutable here; HandleRotateSecret replaces the key.
func (h *Handlers) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	sc, ok := dashboardauth.SessionFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication required", r.URL.Path)
		return
	}
	if !canManage(sc.User.Role) {
		writeProblem(w, http.StatusForbidden, "your role can't manage webhooks", r.URL.Path)
		return
	}
	id, ok := parseAndAuthorise(w, r, h, sc.Account.ID)
	if !ok {
		return
	}
	current, err := h.getWebhookMeta(r.Context(), id)
	if err != nil {
		// Should never happen — parseAndAuthorise just looked it
		// up — but guard anyway.
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<10))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "request body too large", r.URL.Path)
		return
	}
	var req updateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), r.URL.Path)
		return
	}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if err := validateWebhookName(name); err != nil {
			writeProblem(w, http.StatusBadRequest, err.Error(), r.URL.Path)
			return
		}
		current.Name = name
	}
	if req.URL != nil {
		u := strings.TrimSpace(*req.URL)
		if err := validateWebhookURL(r.Context(), u); err != nil {
			writeProblem(w, http.StatusBadRequest, err.Error(), r.URL.Path)
			return
		}
		current.URL = u
	}
	if len(req.Events) > 0 {
		if err := validateEvents(req.Events); err != nil {
			writeProblem(w, http.StatusBadRequest, err.Error(), r.URL.Path)
			return
		}
		current.Events = req.Events
	}
	if req.Enabled != nil {
		current.Enabled = *req.Enabled
	}

	if err := h.cfg.Webhooks.UpdateWebhook(r.Context(), current); err != nil {
		if errors.Is(err, platform.ErrConflict) {
			writeProblem(w, http.StatusConflict, errDuplicateURL, r.URL.Path)
			return
		}
		h.cfg.Logger.Error("update webhook", "err", err, "id", id)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	updated, err := h.getWebhookMeta(r.Context(), id)
	if err != nil {
		h.cfg.Logger.Error("reload webhook after update", "err", err, "id", id)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toDTO(updated))
}

// HandleDelete removes the webhook + cascades to deliveries. An absent
// or cross-account id is 404 (the same shape, so presence never leaks);
// a retried delete therefore reads 404 as "already gone".
func (h *Handlers) HandleDelete(w http.ResponseWriter, r *http.Request) {
	sc, ok := dashboardauth.SessionFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication required", r.URL.Path)
		return
	}
	if !canManage(sc.User.Role) {
		writeProblem(w, http.StatusForbidden, "your role can't manage webhooks", r.URL.Path)
		return
	}
	id, ok := parseAndAuthorise(w, r, h, sc.Account.ID)
	if !ok {
		return
	}
	if err := h.cfg.Webhooks.DeleteWebhook(r.Context(), id); err != nil {
		h.cfg.Logger.Error("delete webhook", "err", err, "id", id)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// previousSecretOverlap is how long a rotated-out key keeps signing
// deliveries alongside the new one: time for the receiver to deploy the
// new key without rejecting anything in between.
const previousSecretOverlap = 24 * time.Hour

type rotateSecretResponse struct {
	WebhookID string `json:"webhook_id"`
	// Secret is the new signing key plaintext, returned once.
	Secret string `json:"secret"`
	// PreviousSecretExpiresAt ends the overlap: until then deliveries
	// also carry X-StellarIndex-Signature[-V2]-Previous from the old key.
	PreviousSecretExpiresAt wiretime.Time `json:"previous_secret_expires_at"`
}

// HandleRotateSecret replaces the webhook's signing key in place. Unlike
// delete + recreate, the webhook keeps its id, its queued and retrying
// deliveries and its delivery log, and the old key keeps signing for
// previousSecretOverlap. Wrapped in the Idempotency-Key middleware: a
// retried rotate would otherwise push the key the receiver still holds
// out of the overlap.
func (h *Handlers) HandleRotateSecret(w http.ResponseWriter, r *http.Request) {
	sc, ok := dashboardauth.SessionFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication required", r.URL.Path)
		return
	}
	if !canManage(sc.User.Role) {
		writeProblem(w, http.StatusForbidden, "your role can't manage webhooks", r.URL.Path)
		return
	}
	id, ok := parseAndAuthorise(w, r, h, sc.Account.ID)
	if !ok {
		return
	}
	secret, err := generateSecret()
	if err != nil {
		h.cfg.Logger.Error("generate webhook secret", "err", err)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	expiresAt := h.cfg.Now().Add(previousSecretOverlap)
	if err := h.cfg.Webhooks.RotateWebhookSecret(r.Context(), id, []byte(secret), expiresAt); err != nil {
		if errors.Is(err, platform.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "webhook not found", r.URL.Path)
			return
		}
		h.cfg.Logger.Error("rotate webhook secret", "err", err, "id", id)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rotateSecretResponse{
		WebhookID:               id.String(),
		Secret:                  secret,
		PreviousSecretExpiresAt: wiretime.Time(expiresAt),
	})
}

type deliveriesResponse struct {
	Deliveries []deliveryDTO `json:"deliveries"`
}

// HandleListDeliveries returns recent attempts for one webhook.
func (h *Handlers) HandleListDeliveries(w http.ResponseWriter, r *http.Request) {
	sc, ok := dashboardauth.SessionFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication required", r.URL.Path)
		return
	}
	if !canManage(sc.User.Role) {
		writeProblem(w, http.StatusForbidden, "your role can't view webhooks", r.URL.Path)
		return
	}
	id, ok := parseAndAuthorise(w, r, h, sc.Account.ID)
	if !ok {
		return
	}
	const defaultLimit = 100
	deliveries, err := h.cfg.Webhooks.ListDeliveries(r.Context(), id, defaultLimit)
	if err != nil {
		h.cfg.Logger.Error("list deliveries", "err", err, "id", id)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	out := deliveriesResponse{Deliveries: make([]deliveryDTO, 0, len(deliveries))}
	for _, d := range deliveries {
		out.Deliveries = append(out.Deliveries, toDeliveryDTO(d))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// ─── helpers ────────────────────────────────────────────────────

// canManage gates every route here, reads included: an endpoint URL often
// embeds the receiver's own credential, so viewer and billing see none.
func canManage(role platform.Role) bool {
	switch role {
	case platform.RoleOwner, platform.RoleAdmin, platform.RoleMember:
		return true
	default:
		return false
	}
}

// getWebhookMeta is GetWebhook for callers that never touch the signing
// key: an unsealable key must not block an owner from editing or deleting
// the webhook, which is the documented recovery from a lost seal key.
func (h *Handlers) getWebhookMeta(ctx context.Context, id uuid.UUID) (platform.CustomerWebhook, error) {
	w, err := h.cfg.Webhooks.GetWebhook(ctx, id)
	if errors.Is(err, platform.ErrWebhookKeyUnsealable) {
		w.SigningKey = nil
		return w, nil
	}
	return w, err
}

// parseAndAuthorise extracts the {id} path value, scopes it to the
// session's account (404 otherwise — don't leak presence). On
// failure writes the response and returns ok=false.
func parseAndAuthorise(w http.ResponseWriter, r *http.Request, h *Handlers, accountID uuid.UUID) (uuid.UUID, bool) {
	raw := r.PathValue("id")
	if raw == "" {
		writeProblem(w, http.StatusBadRequest, "missing id", r.URL.Path)
		return uuid.Nil, false
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "id is not a valid uuid", r.URL.Path)
		return uuid.Nil, false
	}
	current, err := h.getWebhookMeta(r.Context(), id)
	if err != nil {
		if errors.Is(err, platform.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "webhook not found", r.URL.Path)
			return uuid.Nil, false
		}
		h.cfg.Logger.Error("get webhook", "err", err, "id", id)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return uuid.Nil, false
	}
	if current.AccountID != accountID {
		// Don't leak existence — same wire shape as not-found.
		writeProblem(w, http.StatusNotFound, "webhook not found", r.URL.Path)
		return uuid.Nil, false
	}
	return id, true
}

func parseCreateRequest(ctx context.Context, r *http.Request) (createRequest, int, string) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 8<<10))
	if err != nil {
		return createRequest{}, http.StatusBadRequest, "request body too large (max 8 KiB)"
	}
	var req createRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return createRequest{}, http.StatusBadRequest, "invalid JSON: " + err.Error()
	}
	req.Name = strings.TrimSpace(req.Name)
	req.URL = strings.TrimSpace(req.URL)
	if err := validateWebhookName(req.Name); err != nil {
		return createRequest{}, http.StatusBadRequest, err.Error()
	}
	if err := validateWebhookURL(ctx, req.URL); err != nil {
		return createRequest{}, http.StatusBadRequest, err.Error()
	}
	if err := validateEvents(req.Events); err != nil {
		return createRequest{}, http.StatusBadRequest, err.Error()
	}
	return req, 0, ""
}

func validateWebhookName(name string) error {
	if name == "" || utf8.RuneCountInString(name) > 200 {
		return errors.New("name must be 1–200 chars")
	}
	return nil
}

// maxWebhookURLLen bounds a stored webhook URL; it is copied into every
// failed attempt's last_error, so the 8 KiB body cap is far too loose.
const maxWebhookURLLen = 2048

func validateWebhookURL(ctx context.Context, raw string) error {
	if raw == "" {
		return errors.New("url is required")
	}
	if utf8.RuneCountInString(raw) > maxWebhookURLLen {
		return fmt.Errorf("url must be at most %d characters", maxWebhookURLLen)
	}
	if !strings.HasPrefix(raw, "https://") {
		return errors.New("url must start with https:// (TLS required for HMAC integrity)")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("url is malformed: %w", err)
	}
	// SSRF defence-in-depth.
	// Reject embedded credentials, non-https schemes (already
	// caught above but defensive), and resolve the hostname to
	// confirm it isn't in a private / loopback / link-local /
	// reserved range. DNS-rebinding is also countered at delivery
	// time in the worker's dial-control hook.
	if u.User != nil {
		return errors.New("url must not embed userinfo")
	}
	if u.Hostname() == "" {
		return errors.New("url must have a hostname")
	}
	// Deliveries are signed POSTs from our egress; any other port would let
	// a registration aim them at an arbitrary TCP service on a public host.
	if p := u.Port(); p != "" && p != "443" {
		return errors.New("url must use the default https port (443)")
	}
	if err := rejectInternalHost(ctx, u.Hostname()); err != nil {
		return err
	}
	return nil
}

// rejectInternalHost resolves `host` and returns a non-nil error
// if any resolved address is in a non-public range. Used at
// registration time; the delivery worker performs the same check
// at send time to defeat DNS rebinding (the resolution can change
// between when the URL is saved and when the callback fires).
//
// Hostnames in RFC 2606 / RFC 6761 reserved TLDs (.example, .test,
// .invalid, .localhost) bypass the resolution check at registration
// — those names are guaranteed not to resolve to real
// infrastructure, and the delivery worker will reject them at send
// time when name resolution genuinely fails. This keeps tests
// terse without weakening the production check.
func rejectInternalHost(parent context.Context, host string) error {
	if nettools.IsReservedTLD(host) {
		return nil
	}
	// Literal IP gets checked directly; named host gets resolved.
	if ip := net.ParseIP(host); ip != nil {
		if nettools.IsBlockedIP(ip) {
			return fmt.Errorf("url host %q resolves to an internal address — webhook destinations must be publicly routable", host)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	addrs, lookupErr := net.DefaultResolver.LookupIPAddr(ctx, host)
	if lookupErr != nil {
		// Registration tolerates "doesn't resolve right now" so a
		// temporary DNS hiccup doesn't reject an otherwise-valid
		// URL. The delivery worker re-resolves at send time and
		// will surface the failure as a delivery error then.
		return nil //nolint:nilerr // intentional: tolerate transient DNS at registration
	}
	return blockedResolvedAddrError(host, addrs)
}

// blockedResolvedAddrError reports the first resolved address in addrs that
// falls in a non-public range, or nil if none do. Split out of
// rejectInternalHost so the client-facing wording is unit-testable without a
// real DNS resolution.
//
// RSEC-Y1: the error text carries only `host` — the customer's own input —
// never the resolved IP. validateWebhookURL's caller writes err.Error()
// straight into the 400 response body, and the resolved address is internal
// network topology, not something the customer's own request disclosed.
func blockedResolvedAddrError(host string, addrs []net.IPAddr) error {
	for _, ipa := range addrs {
		if nettools.IsBlockedIP(ipa.IP) {
			return fmt.Errorf("url host %q resolves to an internal address — webhook destinations must be publicly routable", host)
		}
	}
	return nil
}

// SSRF IP-block and reserved-TLD logic lives in internal/nettools, shared
// with SEP-1 resolution and webhook delivery so they agree by construction.

// supportedEventList renders [platform.WebhookEventTypes] for the 400 body.
func supportedEventList() string {
	all := platform.WebhookEventTypes()
	names := make([]string, len(all))
	for i, e := range all {
		names[i] = string(e)
	}
	return strings.Join(names, ", ")
}

func validateEvents(events []string) error {
	if len(events) == 0 {
		return errors.New("events must contain at least one entry")
	}
	for _, e := range events {
		if !platform.IsWebhookEventType(e) {
			return fmt.Errorf("event %q is not in the supported set (%s)", e, supportedEventList())
		}
	}
	return nil
}

// maxWebhooksFor resolves the webhook ceiling for an account tier:
// the Config.WebhookQuotas override when present and positive, else
// the [platform.Tier.MaxWebhooks] default ladder.
func (h *Handlers) maxWebhooksFor(tier platform.Tier) int {
	if v, ok := h.cfg.WebhookQuotas[tier]; ok && v > 0 {
		return v
	}
	return tier.MaxWebhooks()
}

func (h *Handlers) checkQuota(r *http.Request, accountID uuid.UUID, maxHooks int) (int, string) {
	hooks, err := h.cfg.Webhooks.ListWebhooksForAccount(r.Context(), accountID)
	if err != nil {
		h.cfg.Logger.Error("checkQuota: list webhooks", "err", err, "account_id", accountID)
		return http.StatusInternalServerError, "internal error"
	}
	if len(hooks) >= maxHooks {
		return http.StatusConflict, fmt.Sprintf("account already has %d webhooks (max %d)", len(hooks), maxHooks)
	}
	return 0, ""
}

// generateSecret mints a 32-byte URL-safe secret returned once to
// the customer. They store it server-side and use it to verify
// the X-StellarIndex-Signature header on inbound POSTs.
func generateSecret() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("read entropy: %w", err)
	}
	// Hex avoids URL-safe-base64 padding edge cases for customers
	// who store the secret in a config file.
	return "wsec_" + hex.EncodeToString(buf[:]), nil
}

// ─── response helpers (shared httpx; type URL pinned here) ─────

func writeProblem(w http.ResponseWriter, status int, detail, instance string) {
	httpx.WriteProblem(w, "https://api.stellarindex.io/errors/dashboard", status, detail, instance)
}
