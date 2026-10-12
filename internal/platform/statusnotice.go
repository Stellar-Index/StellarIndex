package platform

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// StatusNoticeSeverity is the human-authored banner severity for a status notice, distinct from the Alertmanager-derived incident severities and the post-mortem SEV ladder.
type StatusNoticeSeverity string

const (
	// NoticeMaintenance is a planned-work banner ("scheduled maintenance
	// 02:00–03:00 UTC"). Not an incident.
	NoticeMaintenance StatusNoticeSeverity = "maintenance"
	// NoticeMinor / NoticeMajor / NoticeCritical are unplanned incident
	// banners in ascending customer impact.
	NoticeMinor    StatusNoticeSeverity = "minor"
	NoticeMajor    StatusNoticeSeverity = "major"
	NoticeCritical StatusNoticeSeverity = "critical"
)

// ValidNoticeSeverity reports whether s is a known severity; callers validate at the API boundary and the DB CHECK is the backstop.
func ValidNoticeSeverity(s StatusNoticeSeverity) bool {
	switch s {
	case NoticeMaintenance, NoticeMinor, NoticeMajor, NoticeCritical:
		return true
	default:
		return false
	}
}

// StatusNoticeStatus is the lifecycle state of a notice.
type StatusNoticeStatus string

const (
	// NoticeActive is shown on the public status surface.
	NoticeActive StatusNoticeStatus = "active"
	// NoticeResolved is retained for the operator history + audit trail
	// but no longer surfaced to customers.
	NoticeResolved StatusNoticeStatus = "resolved"
)

// StatusNotice is one operator-posted customer-facing status banner, written only by audit-logged operator-tier admin endpoints and read by the public `/v1/status/notices`.
type StatusNotice struct {
	ID         uuid.UUID
	Title      string
	Body       string
	Severity   StatusNoticeSeverity
	Status     StatusNoticeStatus
	CreatedBy  string // acting operator credential reference; empty ok
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ResolvedAt time.Time // zero until resolved
}

// StatusNoticeStore is the persistence boundary for [StatusNotice], implemented by postgresstore.StatusNoticeStore.
type StatusNoticeStore interface {
	// Create inserts a new notice (status defaults to active). Returns
	// the created row with server-generated ID + timestamps populated.
	Create(ctx context.Context, n StatusNotice) (StatusNotice, error)

	// Get returns the notice by ID; ErrNotFound if absent.
	Get(ctx context.Context, id uuid.UUID) (StatusNotice, error)

	// ListActive returns the active notices, newest first. Backs the
	// public `/v1/status/notices` surface.
	ListActive(ctx context.Context) ([]StatusNotice, error)

	// List returns notices (any status), newest first, capped at limit
	// (0 = default). Backs the operator history view.
	List(ctx context.Context, limit int) ([]StatusNotice, error)

	// Resolve flips a notice to resolved (stamping resolved_at). Returns
	// the updated row. Idempotent — resolving an already-resolved notice
	// keeps the original resolved_at. ErrNotFound if absent.
	Resolve(ctx context.Context, id uuid.UUID) (StatusNotice, error)
}
