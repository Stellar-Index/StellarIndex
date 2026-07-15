package dashboardpricealerts

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/dashboardauth"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/httpx"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// Config wires the handlers' dependencies.
type Config struct {
	// Alerts is the platform store powering CRUD. In production:
	// postgresstore.PriceAlertStore.
	Alerts platform.PriceAlertStore
	Logger *slog.Logger
	Now    func() time.Time

	// AlertQuotas optionally overrides the per-tier ceiling on
	// registered price alerts. Tiers absent from the map (or a nil map,
	// the production default) fall back to
	// [platform.Tier.MaxPriceAlerts]. Non-positive values are ignored.
	AlertQuotas map[platform.Tier]int
}

func (c *Config) validate() error {
	if c.Alerts == nil {
		return errors.New("dashboardpricealerts: Alerts store is required")
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = func() time.Time { return time.Now().UTC() }
	}
	return nil
}

// Handlers exposes the routes to be mounted in the v1 mux.
type Handlers struct{ cfg *Config }

// NewHandlers validates the config and returns a mount-ready Handlers.
func NewHandlers(cfg Config) (*Handlers, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Handlers{cfg: &cfg}, nil
}

// Mount installs the dashboard price-alert-management routes.
func (h *Handlers) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/dashboard/price-alerts", h.HandleList)
	mux.HandleFunc("POST /v1/dashboard/price-alerts", h.HandleCreate)
	mux.HandleFunc("PATCH /v1/dashboard/price-alerts/{id}", h.HandleUpdate)
	mux.HandleFunc("DELETE /v1/dashboard/price-alerts/{id}", h.HandleDelete)
}

// priceAlertDTO is the wire shape the dashboard reads.
type priceAlertDTO struct {
	ID              string    `json:"id"`
	BaseAsset       string    `json:"base_asset"`
	QuoteAsset      string    `json:"quote_asset"`
	Condition       string    `json:"condition"`
	Threshold       string    `json:"threshold"`
	CooldownSeconds int       `json:"cooldown_seconds"`
	Enabled         bool      `json:"enabled"`
	LastFiredAt     time.Time `json:"last_fired_at,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func toDTO(a platform.PriceAlert) priceAlertDTO {
	return priceAlertDTO{
		ID:              a.ID.String(),
		BaseAsset:       a.BaseAsset,
		QuoteAsset:      a.QuoteAsset,
		Condition:       string(a.Condition),
		Threshold:       a.Threshold,
		CooldownSeconds: a.CooldownSeconds,
		Enabled:         a.Enabled,
		LastFiredAt:     a.LastFiredAt,
		CreatedAt:       a.CreatedAt,
		UpdatedAt:       a.UpdatedAt,
	}
}

type listResponse struct {
	Alerts []priceAlertDTO `json:"alerts"`
}

// HandleList returns every alert for the session's account, newest first.
func (h *Handlers) HandleList(w http.ResponseWriter, r *http.Request) {
	sc, ok := dashboardauth.SessionFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication required", r.URL.Path)
		return
	}
	alerts, err := h.cfg.Alerts.ListPriceAlertsForAccount(r.Context(), sc.Account.ID)
	if err != nil {
		h.cfg.Logger.Error("list price alerts", "err", err, "account_id", sc.Account.ID)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	out := listResponse{Alerts: make([]priceAlertDTO, 0, len(alerts))}
	for _, a := range alerts {
		out.Alerts = append(out.Alerts, toDTO(a))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

type createRequest struct {
	BaseAsset       string `json:"base_asset"`
	QuoteAsset      string `json:"quote_asset"`
	Condition       string `json:"condition"`
	Threshold       string `json:"threshold"`
	CooldownSeconds int    `json:"cooldown_seconds,omitempty"`
	Enabled         *bool  `json:"enabled,omitempty"` // pointer so absent → true default
}

// HandleCreate registers a new price alert.
func (h *Handlers) HandleCreate(w http.ResponseWriter, r *http.Request) {
	sc, ok := dashboardauth.SessionFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication required", r.URL.Path)
		return
	}
	if !canManage(sc.User.Role) {
		writeProblem(w, http.StatusForbidden, "your role can't manage price alerts", r.URL.Path)
		return
	}

	req, status, problem := parseCreateRequest(r)
	if problem != "" {
		writeProblem(w, status, problem, r.URL.Path)
		return
	}

	maxAlerts := h.maxAlertsFor(sc.Account.Tier)
	// Fast-path UX pre-check (mirrors dashboardwebhooks): surfaces the
	// 409 without spending a write budget. The atomic cap inside
	// CreatePriceAlert is the actual gate against the concurrent-create
	// race.
	if st, pb := h.checkQuota(r, sc.Account.ID, maxAlerts); pb != "" {
		writeProblem(w, st, pb, r.URL.Path)
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	rec := platform.PriceAlert{
		AccountID:       sc.Account.ID,
		BaseAsset:       req.BaseAsset,
		QuoteAsset:      req.QuoteAsset,
		Condition:       platform.AlertCondition(req.Condition),
		Threshold:       req.Threshold,
		CooldownSeconds: req.CooldownSeconds,
		Enabled:         enabled,
	}
	out, err := h.cfg.Alerts.CreatePriceAlert(r.Context(), rec, maxAlerts)
	if err != nil {
		if errors.Is(err, platform.ErrPriceAlertQuotaExceeded) {
			writeProblem(w, http.StatusConflict,
				fmt.Sprintf("account already at the %d-alert quota for the %s tier", maxAlerts, sc.Account.Tier),
				r.URL.Path)
			return
		}
		h.cfg.Logger.Error("create price alert", "err", err, "account_id", sc.Account.ID)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toDTO(out))
}

type updateRequest struct {
	BaseAsset       *string `json:"base_asset,omitempty"`
	QuoteAsset      *string `json:"quote_asset,omitempty"`
	Condition       *string `json:"condition,omitempty"`
	Threshold       *string `json:"threshold,omitempty"`
	CooldownSeconds *int    `json:"cooldown_seconds,omitempty"`
	Enabled         *bool   `json:"enabled,omitempty"`
}

// HandleUpdate patches mutable fields. AccountID + ID are immutable.
func (h *Handlers) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	sc, ok := dashboardauth.SessionFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication required", r.URL.Path)
		return
	}
	if !canManage(sc.User.Role) {
		writeProblem(w, http.StatusForbidden, "your role can't manage price alerts", r.URL.Path)
		return
	}
	id, ok := parseAndAuthorise(w, r, h, sc.Account.ID)
	if !ok {
		return
	}
	current, err := h.cfg.Alerts.GetPriceAlert(r.Context(), id)
	if err != nil {
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
	if status, problem := applyUpdate(&current, req); problem != "" {
		writeProblem(w, status, problem, r.URL.Path)
		return
	}

	if err := h.cfg.Alerts.UpdatePriceAlert(r.Context(), current); err != nil {
		h.cfg.Logger.Error("update price alert", "err", err, "id", id)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	updated, _ := h.cfg.Alerts.GetPriceAlert(r.Context(), id)
	httpx.WriteJSON(w, http.StatusOK, toDTO(updated))
}

// HandleDelete removes the alert. Idempotent — deleting an absent ID
// returns 204 (via parseAndAuthorise's not-found → 404 for cross-account
// probes; a genuinely-absent-but-owned row can't happen since we looked
// it up).
func (h *Handlers) HandleDelete(w http.ResponseWriter, r *http.Request) {
	sc, ok := dashboardauth.SessionFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication required", r.URL.Path)
		return
	}
	if !canManage(sc.User.Role) {
		writeProblem(w, http.StatusForbidden, "your role can't manage price alerts", r.URL.Path)
		return
	}
	id, ok := parseAndAuthorise(w, r, h, sc.Account.ID)
	if !ok {
		return
	}
	if err := h.cfg.Alerts.DeletePriceAlert(r.Context(), id); err != nil {
		h.cfg.Logger.Error("delete price alert", "err", err, "id", id)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── helpers ────────────────────────────────────────────────────

func canManage(role platform.Role) bool {
	switch role {
	case platform.RoleOwner, platform.RoleAdmin, platform.RoleMember:
		return true
	default:
		return false
	}
}

// parseAndAuthorise extracts {id}, scopes it to the session's account
// (404 otherwise — don't leak presence). On failure writes the response
// and returns ok=false.
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
	current, err := h.cfg.Alerts.GetPriceAlert(r.Context(), id)
	if err != nil {
		if errors.Is(err, platform.ErrNotFound) {
			writeProblem(w, http.StatusNotFound, "price alert not found", r.URL.Path)
			return uuid.Nil, false
		}
		h.cfg.Logger.Error("get price alert", "err", err, "id", id)
		writeProblem(w, http.StatusInternalServerError, "internal error", r.URL.Path)
		return uuid.Nil, false
	}
	if current.AccountID != accountID {
		// Don't leak existence — same wire shape as not-found.
		writeProblem(w, http.StatusNotFound, "price alert not found", r.URL.Path)
		return uuid.Nil, false
	}
	return id, true
}

func parseCreateRequest(r *http.Request) (createRequest, int, string) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 8<<10))
	if err != nil {
		return createRequest{}, http.StatusBadRequest, "request body too large (max 8 KiB)"
	}
	var req createRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return createRequest{}, http.StatusBadRequest, "invalid JSON: " + err.Error()
	}
	req.BaseAsset = strings.TrimSpace(req.BaseAsset)
	req.QuoteAsset = strings.TrimSpace(req.QuoteAsset)
	req.Condition = strings.TrimSpace(req.Condition)
	req.Threshold = strings.TrimSpace(req.Threshold)
	if problem := validateAsset("base_asset", req.BaseAsset); problem != "" {
		return createRequest{}, http.StatusBadRequest, problem
	}
	if problem := validateAsset("quote_asset", req.QuoteAsset); problem != "" {
		return createRequest{}, http.StatusBadRequest, problem
	}
	if !platform.ValidAlertCondition(req.Condition) {
		return createRequest{}, http.StatusBadRequest, "condition must be 'above' or 'below'"
	}
	if problem := validateThreshold(req.Threshold); problem != "" {
		return createRequest{}, http.StatusBadRequest, problem
	}
	if req.CooldownSeconds < 0 {
		return createRequest{}, http.StatusBadRequest, "cooldown_seconds must be >= 0"
	}
	return req, 0, ""
}

// applyUpdate validates + applies a PATCH body to current. Returns
// (status, problem) on the first validation failure, or (0, "") on
// success.
func applyUpdate(current *platform.PriceAlert, req updateRequest) (int, string) {
	if req.BaseAsset != nil {
		v := strings.TrimSpace(*req.BaseAsset)
		if problem := validateAsset("base_asset", v); problem != "" {
			return http.StatusBadRequest, problem
		}
		current.BaseAsset = v
	}
	if req.QuoteAsset != nil {
		v := strings.TrimSpace(*req.QuoteAsset)
		if problem := validateAsset("quote_asset", v); problem != "" {
			return http.StatusBadRequest, problem
		}
		current.QuoteAsset = v
	}
	if req.Condition != nil {
		v := strings.TrimSpace(*req.Condition)
		if !platform.ValidAlertCondition(v) {
			return http.StatusBadRequest, "condition must be 'above' or 'below'"
		}
		current.Condition = platform.AlertCondition(v)
	}
	if req.Threshold != nil {
		v := strings.TrimSpace(*req.Threshold)
		if problem := validateThreshold(v); problem != "" {
			return http.StatusBadRequest, problem
		}
		current.Threshold = v
	}
	if req.CooldownSeconds != nil {
		if *req.CooldownSeconds < 0 {
			return http.StatusBadRequest, "cooldown_seconds must be >= 0"
		}
		current.CooldownSeconds = *req.CooldownSeconds
	}
	if req.Enabled != nil {
		current.Enabled = *req.Enabled
	}
	return 0, ""
}

func validateAsset(field, raw string) string {
	if raw == "" {
		return field + " is required"
	}
	if len(raw) > 120 {
		return field + " is too long (max 120 chars)"
	}
	if _, err := canonical.ParseAsset(raw); err != nil {
		return fmt.Sprintf("%s %q is not a canonical asset id (e.g. native, USDC-G…, C…, fiat:USD): %v", field, raw, err)
	}
	return ""
}

// decimalRe pins the threshold to a plain positive decimal so the
// NUMERIC insert is safe (no big.Rat fraction / scientific notation
// reaching the column) and the value round-trips cleanly.
var decimalRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

func validateThreshold(raw string) string {
	if raw == "" {
		return "threshold is required"
	}
	if !decimalRe.MatchString(raw) {
		return "threshold must be a positive decimal (e.g. \"0.15\", \"1200\")"
	}
	r, ok := new(big.Rat).SetString(raw)
	if !ok || r.Sign() <= 0 {
		return "threshold must be greater than zero"
	}
	return ""
}

// maxAlertsFor resolves the per-account ceiling: the AlertQuotas
// override when present and positive, else the tier default.
func (h *Handlers) maxAlertsFor(tier platform.Tier) int {
	if v, ok := h.cfg.AlertQuotas[tier]; ok && v > 0 {
		return v
	}
	return tier.MaxPriceAlerts()
}

func (h *Handlers) checkQuota(r *http.Request, accountID uuid.UUID, maxAlerts int) (int, string) {
	alerts, err := h.cfg.Alerts.ListPriceAlertsForAccount(r.Context(), accountID)
	if err != nil {
		h.cfg.Logger.Error("checkQuota: list price alerts", "err", err, "account_id", accountID)
		return http.StatusInternalServerError, "internal error"
	}
	if len(alerts) >= maxAlerts {
		return http.StatusConflict, fmt.Sprintf("account already has %d price alerts (max %d)", len(alerts), maxAlerts)
	}
	return 0, ""
}

func writeProblem(w http.ResponseWriter, status int, detail, instance string) {
	httpx.WriteProblem(w, "https://api.stellarindex.io/errors/dashboard", status, detail, instance)
}
