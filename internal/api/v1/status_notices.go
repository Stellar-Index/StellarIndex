// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/platform"
)

// StatusNoticeStore is the boundary the status-notice endpoints need.
// Production impl: postgresstore.StatusNoticeStore over the
// `status_notices` table (migration 0082).
type StatusNoticeStore interface {
	Create(ctx context.Context, n platform.StatusNotice) (platform.StatusNotice, error)
	Get(ctx context.Context, id uuid.UUID) (platform.StatusNotice, error)
	ListActive(ctx context.Context) ([]platform.StatusNotice, error)
	List(ctx context.Context, limit int) ([]platform.StatusNotice, error)
	Resolve(ctx context.Context, id uuid.UUID) (platform.StatusNotice, error)
}

// StatusNotice is the wire shape for status-notice responses — the
// operator-posted customer-facing banner (platform-spec §7.1). Distinct
// from the Alertmanager-derived StatusIncidents block on /v1/status and
// from the embedded post-mortem incidents.Incident: this is a live,
// human-authored maintenance / incident banner.
type StatusNotice struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	Severity   string `json:"severity"` // maintenance | minor | major | critical
	Status     string `json:"status"`   // active | resolved
	CreatedAt  string `json:"created_at,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
	ResolvedAt string `json:"resolved_at,omitempty"`
}

// StatusNoticesList is the wire envelope for the notice list endpoints.
type StatusNoticesList struct {
	Notices []StatusNotice `json:"notices"`
	Count   int            `json:"count"`
}

func statusNoticeView(n platform.StatusNotice) StatusNotice {
	v := StatusNotice{
		ID:       n.ID.String(),
		Title:    n.Title,
		Body:     n.Body,
		Severity: string(n.Severity),
		Status:   string(n.Status),
	}
	if !n.CreatedAt.IsZero() {
		v.CreatedAt = n.CreatedAt.UTC().Format(time.RFC3339)
	}
	if !n.UpdatedAt.IsZero() {
		v.UpdatedAt = n.UpdatedAt.UTC().Format(time.RFC3339)
	}
	if !n.ResolvedAt.IsZero() {
		v.ResolvedAt = n.ResolvedAt.UTC().Format(time.RFC3339)
	}
	return v
}

func statusNoticeViews(in []platform.StatusNotice) []StatusNotice {
	out := make([]StatusNotice, 0, len(in))
	for _, n := range in {
		out = append(out, statusNoticeView(n))
	}
	return out
}

// handleStatusNotices serves GET /v1/status/notices — the public,
// anonymous-friendly list of ACTIVE operator-posted banners the status
// page renders. Nil-to-empty: an unwired store or zero active notices
// returns `{"notices":[],"count":0}` rather than null so SDK/JS
// consumers can .map() unconditionally.
func (s *Server) handleStatusNotices(w http.ResponseWriter, r *http.Request) {
	if s.statusNotices == nil {
		writeJSON(w, StatusNoticesList{Notices: []StatusNotice{}, Count: 0}, Flags{})
		return
	}
	rows, err := s.statusNotices.ListActive(r.Context())
	if err != nil {
		// A notice-store blip must never fail the status surface — the
		// banner is a nicety layered over the SLA-truth /v1/status.
		s.logger.Warn("status notices list failed; returning empty", "err", err)
		writeJSON(w, StatusNoticesList{Notices: []StatusNotice{}, Count: 0}, Flags{})
		return
	}
	views := statusNoticeViews(rows)
	writeJSON(w, StatusNoticesList{Notices: views, Count: len(views)}, Flags{})
}

// handleAdminStatusNoticesList serves GET /v1/admin/status-notices —
// the operator history view: every notice, active + resolved, newest
// first. Operator-tier only; read-only (no audit row).
func (s *Server) handleAdminStatusNoticesList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireOperator(w, r, "/v1/admin/status-notices"); !ok {
		return
	}
	if s.statusNotices == nil {
		writeStatusNoticeStoreUnavailable(w, r)
		return
	}
	rows, err := s.statusNotices.List(r.Context(), 0)
	if err != nil {
		s.logger.Error("admin status notices list failed", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/status-notice-list-failed",
			"Could not list status notices", http.StatusInternalServerError,
			"see X-Request-ID in server logs")
		return
	}
	views := statusNoticeViews(rows)
	writeJSON(w, StatusNoticesList{Notices: views, Count: len(views)}, Flags{})
}

// adminCreateNoticeRequest is the POST /v1/admin/status-notices body.
type adminCreateNoticeRequest struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	Severity string `json:"severity"`
}

// handleAdminStatusNoticeCreate serves POST /v1/admin/status-notices —
// post a new active customer-facing banner. Operator-tier only;
// requires an `X-Reason` header (platform-spec §7.2). Audit-logged
// ("status_notice.create").
func (s *Server) handleAdminStatusNoticeCreate(w http.ResponseWriter, r *http.Request) {
	subject, ok := s.requireOperator(w, r, "/v1/admin/status-notices")
	if !ok {
		return
	}
	if s.statusNotices == nil {
		writeStatusNoticeStoreUnavailable(w, r)
		return
	}
	reason := r.Header.Get("X-Reason")
	if reason == "" {
		writeMissingReason(w, r)
		return
	}
	req, ok := parseCreateNoticeRequest(w, r)
	if !ok {
		return
	}

	created, err := s.statusNotices.Create(r.Context(), platform.StatusNotice{
		Title:     req.Title,
		Body:      req.Body,
		Severity:  platform.StatusNoticeSeverity(req.Severity),
		CreatedBy: noticeActorRef(subject),
	})
	if err != nil {
		if clientAborted(r, err) {
			return
		}
		s.logger.Error("admin status notice create failed", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/status-notice-create-failed",
			"Could not create status notice", http.StatusInternalServerError,
			"see X-Request-ID in server logs")
		return
	}

	s.logger.Info("admin status notice create",
		"actor_key_id", subject.KeyID,
		"actor_identifier", subject.Identifier,
		"notice_id", created.ID,
		"severity", req.Severity,
		"reason", reason)
	s.recordStatusNoticeAudit(r, subject, "status_notice.create", created.ID.String(), reason, map[string]any{
		"title":    created.Title,
		"severity": string(created.Severity),
	})

	writeEnvelopeStatus(w, http.StatusCreated, Envelope{
		Data:  statusNoticeView(created),
		AsOf:  created.CreatedAt,
		Flags: Flags{},
	})
}

// handleAdminStatusNoticeResolve serves POST
// /v1/admin/status-notices/{id}/resolve — flip an active banner to
// resolved (clears it from the public surface). Operator-tier only;
// requires `X-Reason`. Idempotent. Audit-logged ("status_notice.resolve").
func (s *Server) handleAdminStatusNoticeResolve(w http.ResponseWriter, r *http.Request) {
	subject, ok := s.requireOperator(w, r, "/v1/admin/status-notices/{id}/resolve")
	if !ok {
		return
	}
	if s.statusNotices == nil {
		writeStatusNoticeStoreUnavailable(w, r)
		return
	}
	reason := r.Header.Get("X-Reason")
	if reason == "" {
		writeMissingReason(w, r)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-notice-id",
			"Invalid notice id", http.StatusBadRequest,
			"path must be /v1/admin/status-notices/{id}/resolve with id a UUID")
		return
	}

	resolved, err := s.statusNotices.Resolve(r.Context(), id)
	if errors.Is(err, platform.ErrNotFound) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/status-notice-not-found",
			"Status notice not found", http.StatusNotFound,
			"no status notice with that id")
		return
	}
	if err != nil {
		s.logger.Error("admin status notice resolve failed", "err", err, "notice_id", id)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/status-notice-resolve-failed",
			"Could not resolve status notice", http.StatusInternalServerError,
			"see X-Request-ID in server logs")
		return
	}

	s.logger.Info("admin status notice resolve",
		"actor_key_id", subject.KeyID,
		"actor_identifier", subject.Identifier,
		"notice_id", id,
		"reason", reason)
	s.recordStatusNoticeAudit(r, subject, "status_notice.resolve", id.String(), reason, nil)

	writeJSON(w, statusNoticeView(resolved), Flags{})
}

// parseCreateNoticeRequest reads + validates the create body. ok=false
// means a problem+json was already written.
func parseCreateNoticeRequest(w http.ResponseWriter, r *http.Request) (adminCreateNoticeRequest, bool) {
	var req adminCreateNoticeRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8*1024))
	if err != nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/body-too-large",
			"Request body too large", http.StatusBadRequest,
			"/v1/admin/status-notices body must be under 8 KiB")
		return req, false
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-body",
				"Malformed JSON body", http.StatusBadRequest,
				"could not parse request body as JSON")
			return req, false
		}
	}
	if req.Title == "" || len(req.Title) > 200 {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-title",
			"Title is required", http.StatusBadRequest,
			"title must be 1–200 characters")
		return req, false
	}
	if req.Body == "" || len(req.Body) > 5000 {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-notice-body",
			"Body is required", http.StatusBadRequest,
			"body must be 1–5000 characters")
		return req, false
	}
	if !platform.ValidNoticeSeverity(platform.StatusNoticeSeverity(req.Severity)) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-severity",
			"Invalid severity", http.StatusBadRequest,
			"severity must be one of maintenance, minor, major, critical")
		return req, false
	}
	return req, true
}

// noticeActorRef derives the free-form "who posted this" reference
// stored on the row. Prefers the operator key id, falls back to the
// identifier.
func noticeActorRef(subject auth.Subject) string {
	if subject.KeyID != "" {
		return subject.KeyID
	}
	return subject.Identifier
}

func writeStatusNoticeStoreUnavailable(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/status-notice-store-unavailable",
		"Status notice store not configured", http.StatusServiceUnavailable,
		"this deployment has no StatusNoticeStore wired — typically because Postgres is unavailable")
}

func writeMissingReason(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/missing-reason",
		"X-Reason header required", http.StatusBadRequest,
		"every admin write captures an X-Reason header into the audit log")
}

// recordStatusNoticeAudit persists the audit row for a notice mutation.
// Best-effort — mirrors recordAdminKeyMintAudit's posture.
func (s *Server) recordStatusNoticeAudit(
	r *http.Request, actor auth.Subject, action, noticeID, reason string, extra map[string]any,
) {
	if s.audit == nil {
		return
	}
	fields := map[string]any{
		"actor_key_id":     actor.KeyID,
		"actor_identifier": actor.Identifier,
		"reason":           reason,
	}
	for k, v := range extra {
		fields[k] = v
	}
	meta, err := json.Marshal(fields)
	if err != nil {
		s.logger.Warn("admin status notice: audit metadata marshal failed (skipping audit row)",
			"err", err, "notice_id", noticeID)
		return
	}
	entry := platform.AuditEntry{
		ActorKind:  platform.ActorStaff,
		Action:     action,
		TargetKind: "status_notice",
		TargetID:   noticeID,
		Metadata:   meta,
		UserAgent:  r.UserAgent(),
		Timestamp:  time.Now().UTC(),
	}
	if ip := middleware.RemoteIP(r); ip != "" {
		entry.IP = net.ParseIP(ip)
	}
	if err := s.audit.Append(r.Context(), entry); err != nil {
		s.logger.Warn("admin status notice: audit append failed (best-effort)",
			"err", err, "notice_id", noticeID, "action", action)
	}
}
