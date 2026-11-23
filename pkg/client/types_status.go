package client

import "time"

// Cursor is one per-source ingest progress marker from [Client.Cursors]; LagSeconds is computed server-side, and State is "live", "stale" or "abandoned" (abandoned rows are omitted unless requested).
type Cursor struct {
	Source      string `json:"source"`
	SubSource   string `json:"sub_source,omitempty"`
	LastLedger  uint32 `json:"last_ledger"`
	LastUpdated string `json:"last_updated"`
	LagSeconds  int64  `json:"lag_seconds"`
	State       string `json:"state"`
}

// Status is the data shape of [Client.Status], the customer-facing system-health rollup of /v1/status.
type Status struct {
	Overall   string          `json:"overall"`
	Region    StatusRegion    `json:"region"`
	Services  []StatusService `json:"services"`
	Latency   StatusLatency   `json:"latency"`
	Freshness StatusFreshness `json:"freshness"`
	Incidents StatusIncidents `json:"incidents"`
	// IncidentsStatus is the explicit tri-state trust signal for the Incidents block: "ok", "degraded", or
	// "unknown". Read it BEFORE the counts: on "unknown" the Alertmanager query failed (or no metrics backend is
	// wired) and the counts are the zero value of a failure, NOT an all-clear.
	IncidentsStatus string `json:"incidents_status"`
	// FreshnessStatus is the same trust signal for the Freshness block:
	// "ok" (every enabled source active), "degraded" (active < total) or
	// "unknown" (a count query failed; the counts are nil).
	FreshnessStatus string `json:"freshness_status"`
}

// StatusRegion identifies which region produced the response.
type StatusRegion struct {
	Name       string `json:"name"`
	Deployment string `json:"deployment"`
}

// StatusService is a per-binary heartbeat in [Status.Services]: "ok" if the last scrape was within 60 s, "down" when stale, "unknown" with no Prometheus backend.
type StatusService struct {
	Name     string    `json:"name"`
	Status   string    `json:"status"`
	LastSeen time.Time `json:"last_seen,omitempty"`
}

// StatusLatency reports histogram-derived API percentiles over the last [WindowSecs] seconds; zero means no backend or no samples.
type StatusLatency struct {
	P50Ms      float64 `json:"p50_ms"`
	P95Ms      float64 `json:"p95_ms"`
	P99Ms      float64 `json:"p99_ms"`
	WindowSecs int     `json:"window_secs"`
	// P95TargetMs / P99TargetMs are the server's own SLO thresholds,
	// echoed so a consumer judges latency against the SAME numbers the
	// status rollup does. Hardcoding them client-side risks
	// two red SLO bars under a green "All systems operational" banner.
	P95TargetMs float64 `json:"p95_target_ms"`
	P99TargetMs float64 `json:"p99_target_ms"`
}

// StatusFreshness summarises the ingest layer; a nil count was not measured (its query failed), while a served 0 is a non-nil 0.
type StatusFreshness struct {
	LastAggregatorTick time.Time `json:"last_aggregator_tick,omitempty"`
	ActiveSources      *int      `json:"active_sources,omitempty"`
	TotalSources       *int      `json:"total_sources,omitempty"`
}

// StatusIncidents counts currently-firing alerts by severity; zero means no Alertmanager backend or no alerts.
type StatusIncidents struct {
	ActiveCount        int              `json:"active_count"`
	PageCount          int              `json:"page_count"`
	TicketCount        int              `json:"ticket_count"`
	InformationalCount int              `json:"informational_count"`
	Active             []ActiveIncident `json:"active,omitempty"`
}

// ActiveIncident is the customer-facing summary of one currently-firing alert in [StatusIncidents.Active]; internal labels are deliberately excluded.
type ActiveIncident struct {
	Name       string `json:"name"`
	Severity   string `json:"severity"`
	RunbookURL string `json:"runbook_url,omitempty"`
}

// Health is the data shape of [Client.Healthz] and [Client.Readyz].
type Health struct {
	Status string `json:"status"`
	Uptime string `json:"uptime"`
	// Checks is populated on /readyz with per-dependency ping
	// results; absent on /healthz.
	Checks []HealthCheck `json:"checks,omitempty"`
	// StatusRoot points at /v1/status — the SLA-truth rollup
	// covering ingest lag, freshness, and per-pair latency.
	StatusRoot string `json:"status_root,omitempty"`
}

// HealthCheck is one per-dependency result in [Health].Checks.
type HealthCheck struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// Error is populated only when OK is false.
	Error string `json:"error,omitempty"`
}

// Version is the data shape of [Client.Version]: build metadata for the API binary.
type Version struct {
	Version   string `json:"version"`
	BuildDate string `json:"build_date"`
	Commit    string `json:"commit"`
	Dirty     string `json:"dirty"`
	GoVersion string `json:"go_version"`
}

// Incident is one customer-facing incident post from [Client.Incidents]; Severity is SEV-1..SEV-4, Status is investigating/identified/monitoring/resolved, and BodyMarkdown is the full post.
type Incident struct {
	Slug               string     `json:"slug"`
	Title              string     `json:"title"`
	Severity           string     `json:"severity"`
	Status             string     `json:"status"`
	StartedAt          time.Time  `json:"started_at"`
	ResolvedAt         *time.Time `json:"resolved_at,omitempty"`
	AffectedComponents []string   `json:"affected_components,omitempty"`
	PostmortemRef      string     `json:"postmortem,omitempty"`
	BodyMarkdown       string     `json:"body_markdown"`
}

// IncidentsList wraps the [Client.Incidents] response, sorted started_at descending.
type IncidentsList struct {
	Incidents []Incident `json:"incidents"`
	Count     int        `json:"count"`
}
