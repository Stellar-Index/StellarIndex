package dashboardauth

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// adminLookupInstance is the problem `instance` for the staff look-up.
const adminLookupInstance = "/v1/account/admin/lookup"

// AdminAccountView is the account half of the staff customer look-up.
type AdminAccountView struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Slug            string `json:"slug"`
	Tier            string `json:"tier"`
	Status          string `json:"status"`
	BillingEmail    string `json:"billing_email,omitempty"`
	CreatedAt       string `json:"created_at,omitempty"`
	SuspendedReason string `json:"suspended_reason,omitempty"`
	// RateLimitPerMinOverride / MonthlyRequestQuotaOverride are 0 when the
	// account inherits its tier default.
	RateLimitPerMinOverride     int   `json:"rate_limit_per_min_override,omitempty"`
	MonthlyRequestQuotaOverride int64 `json:"monthly_request_quota_override,omitempty"`
}

// AdminUserView is one user under the looked-up account.
type AdminUserView struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	DisplayName   string `json:"display_name,omitempty"`
	Role          string `json:"role"`
	IsStaff       bool   `json:"is_staff"`
	EmailVerified bool   `json:"email_verified"`
	LastLoginAt   string `json:"last_login_at,omitempty"`
}

// AdminLookupResponse is the staff customer look-up wire shape.
type AdminLookupResponse struct {
	Account AdminAccountView `json:"account"`
	Users   []AdminUserView  `json:"users"`
}

// HandleAdminLookup serves GET /v1/account/admin/lookup?email=|slug= — the
// staff "Customer look-up" tool (platform spec §6): resolve an account by a
// user's email or by account slug and return its tier/status plus the users
// on it. Staff-only: the route is wrapped in RequireSession and this handler
// additionally gates on the session user's IsStaff flag (a logged-in
// non-staff customer gets 403, never another customer's data). Read-only.
func (h *Handlers) HandleAdminLookup(w http.ResponseWriter, r *http.Request) {
	sc, ok := SessionFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "authentication required", adminLookupInstance)
		return
	}
	if !sc.User.IsStaff {
		writeProblem(w, http.StatusForbidden, "staff access required", adminLookupInstance)
		return
	}

	email := strings.TrimSpace(r.URL.Query().Get("email"))
	slug := strings.TrimSpace(r.URL.Query().Get("slug"))

	acct, err := h.resolveLookupAccount(r, email, slug)
	if errors.Is(err, errLookupNoQuery) {
		writeProblem(w, http.StatusBadRequest, "provide ?email= or ?slug=", adminLookupInstance)
		return
	}
	if errors.Is(err, platform.ErrNotFound) {
		writeProblem(w, http.StatusNotFound, "no matching customer", adminLookupInstance)
		return
	}
	if err != nil {
		if h.cfg.Logger != nil {
			h.cfg.Logger.Warn("admin lookup failed", "err", err, "actor", maskEmail(sc.User.Email))
		}
		writeProblem(w, http.StatusInternalServerError, "internal error", adminLookupInstance)
		return
	}

	users, _ := h.cfg.Users.ListUsersForAccount(r.Context(), acct.ID)
	// Staff access to customer data is auditable — record who looked up what.
	if h.cfg.Logger != nil {
		h.cfg.Logger.Info("staff customer lookup", "actor", maskEmail(sc.User.Email), "account", acct.Slug)
	}
	h.recordAdminLookupAudit(r, sc, acct, lookupQueryKind(email), len(users))

	resp := AdminLookupResponse{Account: adminAccountView(acct), Users: adminUserViews(users)}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}

// errLookupNoQuery signals neither email nor slug was supplied.
var errLookupNoQuery = errors.New("dashboardauth: admin lookup needs email or slug")

// lookupQueryKind names the dimension the staff member searched on, so
// the audit row records HOW a customer was found (an email probe and a
// slug probe are different investigative acts) without copying the
// probed address into a second store.
func lookupQueryKind(email string) string {
	if email != "" {
		return "email"
	}
	return "slug"
}

// recordAdminLookupAudit persists the "staff.customer.lookup" audit row
// (C3-056, audit-2026-07-23). Pre-fix this surface recorded a staff read
// of another customer's PII — account tier/status/billing email plus
// every user's email and last-login — with a single `Logger.Info` line,
// while every sibling admin surface (`admin_accounts.go`,
// `admin_keys.go`, `status_notices.go`) wrote a durable
// [platform.AuditEntry]. A log line is not a privacy audit trail: it is
// rotated on a short retention, is not queryable per-account, and is
// absent from the dashboard's audit view.
//
// Best-effort, matching the sibling contract: the read has already
// happened by the time this runs, so a sink failure logs at WARN and
// increments the shared write-failure counter under its own `surface`
// label rather than failing the request.
func (h *Handlers) recordAdminLookupAudit(
	r *http.Request, sc SessionContext, acct platform.Account, queryKind string, usersReturned int,
) {
	if h.cfg.Audit == nil {
		return
	}
	meta, err := json.Marshal(map[string]any{
		"actor_user_id":   sc.User.ID.String(),
		"actor_email":     sc.User.Email,
		"query_kind":      queryKind,
		"account_slug":    acct.Slug,
		"users_returned":  usersReturned,
		"account_tier":    string(acct.Tier),
		"account_status":  string(acct.Status),
		"session_id":      sc.Session.ID.String(),
		"lookup_endpoint": adminLookupInstance,
	})
	if err != nil {
		// Unreachable — a map of strings/ints always marshals. Surface
		// it rather than writing a row with no metadata.
		h.cfg.Logger.Warn("staff customer lookup: audit metadata marshal failed (skipping audit row)",
			"err", err, "account_id", acct.ID)
		obs.AdminAuditWriteFailuresTotal.WithLabelValues(auditSurfaceStaffLookup).Inc()
		return
	}
	entry := platform.AuditEntry{
		AccountID:   acct.ID,
		ActorUserID: sc.User.ID,
		ActorKind:   platform.ActorStaff,
		Action:      "staff.customer.lookup",
		TargetKind:  "account",
		TargetID:    acct.ID.String(),
		Metadata:    meta,
		IP:          clientIP(r),
		UserAgent:   truncateUA(r.UserAgent()),
		Timestamp:   h.cfg.Now(),
	}
	if err := h.cfg.Audit.Append(r.Context(), entry); err != nil {
		// C3-067's counter, C3-056's surface: the PII read already
		// happened and cannot be un-done, so the only honest response is
		// to make the missing row visible.
		obs.AdminAuditWriteFailuresTotal.WithLabelValues(auditSurfaceStaffLookup).Inc()
		h.cfg.Logger.Warn("staff customer lookup: audit append failed (best-effort)",
			"err", err, "account_id", acct.ID, "actor", maskEmail(sc.User.Email))
	}
}

// auditSurfaceStaffLookup is this surface's label value on
// [obs.AdminAuditWriteFailuresTotal]. Kept as a named constant so the
// increment site and the zero-seed in internal/obs cannot drift.
const auditSurfaceStaffLookup = "staff_customer_lookup"

// resolveLookupAccount resolves the target account by email (via its user) or
// by slug. Returns errLookupNoQuery when neither is set, platform.ErrNotFound
// when nothing matches.
func (h *Handlers) resolveLookupAccount(r *http.Request, email, slug string) (platform.Account, error) {
	switch {
	case email != "":
		u, err := h.cfg.Users.GetUserByEmail(r.Context(), strings.ToLower(email))
		if err != nil {
			return platform.Account{}, err
		}
		return h.cfg.Accounts.Get(r.Context(), u.AccountID)
	case slug != "":
		return h.cfg.Accounts.GetBySlug(r.Context(), strings.ToLower(slug))
	default:
		return platform.Account{}, errLookupNoQuery
	}
}

func adminAccountView(a platform.Account) AdminAccountView {
	v := AdminAccountView{
		ID:                          a.ID.String(),
		Name:                        a.Name,
		Slug:                        a.Slug,
		Tier:                        string(a.Tier),
		Status:                      string(a.Status),
		BillingEmail:                a.BillingEmail,
		SuspendedReason:             a.SuspendedReason,
		RateLimitPerMinOverride:     a.RateLimitPerMinOverride,
		MonthlyRequestQuotaOverride: a.MonthlyRequestQuotaOverride,
	}
	if !a.CreatedAt.IsZero() {
		v.CreatedAt = a.CreatedAt.UTC().Format(time.RFC3339)
	}
	return v
}

func adminUserViews(users []platform.User) []AdminUserView {
	out := make([]AdminUserView, 0, len(users))
	for _, u := range users {
		uv := AdminUserView{
			ID:            u.ID.String(),
			Email:         u.Email,
			DisplayName:   u.DisplayName,
			Role:          string(u.Role),
			IsStaff:       u.IsStaff,
			EmailVerified: !u.EmailVerifiedAt.IsZero(),
		}
		if !u.LastLoginAt.IsZero() {
			uv.LastLoginAt = u.LastLoginAt.UTC().Format(time.RFC3339)
		}
		out = append(out, uv)
	}
	return out
}
