package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// StatusResponse is the /v1/status wire shape, the customer-facing health rollup behind the public status page, distinct from /v1/healthz and /v1/readyz.
type StatusResponse struct {
	// Overall is "ok" when every signal is healthy, "degraded" when a signal is unhealthy but the
	// API is serving, "down" when no live signal is available.
	Overall string `json:"overall"`

	// Region identifies which region this response came from (single-region today).
	Region StatusRegion `json:"region"`

	// Services reports per-binary heartbeats; the API entry is always present.
	Services []StatusService `json:"services"`

	// Latency is the API histogram percentiles over the last 5 minutes; zero values mean
	// Prometheus is unwired or the window has no samples.
	Latency StatusLatency `json:"latency"`

	// Freshness summarises the ingest layer.
	Freshness StatusFreshness `json:"freshness"`

	// Incidents counts active Alertmanager alerts by severity. On IncidentsStatus "unknown" the
	// counts are the zero value of a FAILED query and MUST NOT be read as an all-clear.
	Incidents StatusIncidents `json:"incidents"`

	// IncidentsStatus is the trust signal for the Incidents block: a failed Alertmanager query
	// zeroes the counts, identical to "no alerts firing". "ok" = no alerts; "degraded" = alerts
	// firing; "unknown" = query FAILED or no backend wired, and the UI must render "unknown", not
	// "0 active". Always present (no omitempty) so an omitted field is never read as a zero count.
	IncidentsStatus string `json:"incidents_status"`

	// FreshnessStatus is the trust signal for the Freshness block, for the same reason: a failed
	// count query must not read as a measured zero.
	//   - "ok":       counts measured, every enabled source active.
	//   - "degraded": counts measured, active_sources < total_sources.
	//   - "unknown":  a freshness query FAILED or no backend is wired; the counts are omitted.
	// Always present.
	FreshnessStatus string `json:"freshness_status"`
}

// Freshness-block trust states for [StatusResponse.FreshnessStatus].
const (
	freshnessStatusOK       = "ok"
	freshnessStatusDegraded = "degraded"
	freshnessStatusUnknown  = "unknown"
)

// freshnessStatusFor classifies the freshness block as incidentsStatusFor does the incidents block.
func freshnessStatusFor(f StatusFreshness, err error) string {
	switch {
	case err != nil, f.ActiveSources == nil, f.TotalSources == nil:
		return freshnessStatusUnknown
	case *f.ActiveSources < *f.TotalSources:
		return freshnessStatusDegraded
	default:
		return freshnessStatusOK
	}
}

// Incident-block trust states for [StatusResponse.IncidentsStatus].
const (
	incidentsStatusOK       = "ok"
	incidentsStatusDegraded = "degraded"
	incidentsStatusUnknown  = "unknown"
)

// incidentsStatusFor classifies the incidents trust state from the query outcome; a query error is "unknown", never an all-clear
// ([StatusResponse.IncidentsStatus]).
func incidentsStatusFor(inc StatusIncidents, err error) string {
	switch {
	case err != nil:
		return incidentsStatusUnknown
	case inc.ActiveCount > 0:
		return incidentsStatusDegraded
	default:
		return incidentsStatusOK
	}
}

type StatusRegion struct {
	Name       string `json:"name"`
	Deployment string `json:"deployment"`
}

type StatusService struct {
	Name     string   `json:"name"`
	Status   string   `json:"status"` // "ok" | "down" | "unknown"
	LastSeen WireTime `json:"last_seen,omitempty"`
}

type StatusLatency struct {
	P50Ms      float64 `json:"p50_ms"`
	P95Ms      float64 `json:"p95_ms"`
	P99Ms      float64 `json:"p99_ms"`
	WindowSecs int     `json:"window_secs"`

	// Targets are echoed so the status page renders the same numbers the roll-up judges
	// against; targets held only in the frontend can contradict the overall banner.
	P95TargetMs float64 `json:"p95_target_ms"`
	P99TargetMs float64 `json:"p99_target_ms"`
}

// Latency SLO targets for the status roll-up, declared once and consumed by both the API and the
// status page.
const (
	statusLatencyP95TargetMs = 200
	statusLatencyP99TargetMs = 500
)

// breached reports whether either percentile is over target. WindowSecs == 0 means nothing was
// measurable, which is not a breach; absence of data is handled by the backendErr path.
func (l StatusLatency) breached() bool {
	if l.WindowSecs == 0 {
		return false
	}
	return l.P95Ms > statusLatencyP95TargetMs || l.P99Ms > statusLatencyP99TargetMs
}

// StatusFreshness counts are pointers so a failed probe serialises as absent, not "0 / 0".
type StatusFreshness struct {
	LastAggregatorTick WireTime `json:"last_aggregator_tick,omitempty"`
	ActiveSources      *int     `json:"active_sources,omitempty"`
	TotalSources       *int     `json:"total_sources,omitempty"`
}

type StatusIncidents struct {
	ActiveCount        int `json:"active_count"`
	PageCount          int `json:"page_count"`
	TicketCount        int `json:"ticket_count"`
	InformationalCount int `json:"informational_count"`

	// Active is the deduplicated, page-severity-first list of firing alerts, capped
	// server-side at 16 because this is a customer-facing summary, not an operator view.
	// Empty when nothing fires or no Prometheus backend is wired.
	Active []ActiveIncident `json:"active,omitempty"`
}

// ActiveIncident is one entry in [StatusIncidents.Active]; internal labels (component, team, instance) are deliberately excluded to keep it public-safe.
type ActiveIncident struct {
	Name       string `json:"name"`
	Severity   string `json:"severity"`
	RunbookURL string `json:"runbook_url,omitempty"`
}

// StatusBackend pulls signals from a metrics and alerting stack ([PrometheusStatusBackend]); nil degrades to in-process data only.
type StatusBackend interface {
	// Heartbeats returns the latest successful scrape time per friendly service name
	// ("indexer", "aggregator", "api"). A zero time means the scrape has never succeeded.
	Heartbeats(ctx context.Context) (map[string]time.Time, error)

	// Latency returns the 5-minute p50/p95/p99 of the API histogram; zero means no samples.
	Latency(ctx context.Context) (StatusLatency, error)

	// Freshness returns aggregator and source-count signals.
	Freshness(ctx context.Context) (StatusFreshness, error)

	// Incidents returns currently-firing alert counts by severity.
	Incidents(ctx context.Context) (StatusIncidents, error)

	// SourceEntries24h returns per-source events ingested in the trailing 24h, from the same
	// counter (stellarindex_source_events_total) that backs active_sources. It covers on-chain
	// and external sources and is cheap (a Prometheus range-increase, not a trades scan).
	// Sources with no events in the window are absent.
	SourceEntries24h(ctx context.Context) (map[string]int64, error)

	// SourceEnabled reports which registry sources are switched on (stellarindex_source_enabled);
	// absent sources were never enabled here. Without it a built-but-off source would render a
	// count of 0, indistinguishable from a dead connector.
	SourceEnabled(ctx context.Context) (map[string]bool, error)
}

// PrometheusStatusBackend queries a Prometheus HTTP API; URL is the root (http://localhost:9090).
type PrometheusStatusBackend struct {
	URL    string
	Client *http.Client
}

func (p *PrometheusStatusBackend) Heartbeats(ctx context.Context) (map[string]time.Time, error) {
	// Prometheus writes an up sample on failed scrapes too; filtering on ==1 before taking the
	// timestamp lets a crashed target's heartbeat go stale instead of refreshing.
	const q = `max_over_time(timestamp(up{job=~"stellarindex-indexer|stellarindex-aggregator|stellarindex-api"} == 1)[5m:15s])`
	res, err := p.queryVector(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make(map[string]time.Time, len(res))
	for _, sample := range res {
		job, _ := sample.Labels["job"].(string)
		short := strings.TrimPrefix(job, "stellarindex-")
		if t, ok := sample.Float(); ok {
			out[short] = time.Unix(int64(t), 0).UTC()
		}
	}
	return out, nil
}

func (p *PrometheusStatusBackend) Latency(ctx context.Context) (StatusLatency, error) {
	out := StatusLatency{WindowSecs: 300}
	for _, q := range []struct {
		expr   string
		target *float64
	}{
		{
			`histogram_quantile(0.5, sum by (le) (rate(http_request_duration_seconds_bucket{job="stellarindex-api"}[5m])))`,
			&out.P50Ms,
		},
		{
			`histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{job="stellarindex-api"}[5m])))`,
			&out.P95Ms,
		},
		{
			`histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket{job="stellarindex-api"}[5m])))`,
			&out.P99Ms,
		},
	} {
		res, err := p.queryVector(ctx, q.expr)
		if err != nil {
			return out, err
		}
		for _, sample := range res {
			if v, ok := sample.Float(); ok {
				*q.target = v * 1000 // s → ms
			}
		}
	}
	return out, nil
}

// activeSourcesQuery and totalSourcesQuery are the "Active sources" numerator and denominator. The numerator MUST join on
// stellarindex_source_enabled,
// or the populations differ and the ratio can exceed 1; a feed with no gauge counts on NEITHER side. Both assume one scrape target per
// source.
// TestStatusSourceCountQueries_MatchPromtoolFixture holds deploy/monitoring/rule-tests/status-source-counts_test.yml to these exact
// strings.
const activeSourcesQuery = `count(
	(rate(stellarindex_source_events_total[7d]) > 0)
	and on (source) (stellarindex_source_enabled == 1)
)`

const totalSourcesQuery = `count(stellarindex_source_enabled == 1)`

// Freshness returns the first query error rather than a zero count so a blind probe trips the canary, and uses a 7-day active window because rozo, phoenix, ecb and band emit few events per day.
func (p *PrometheusStatusBackend) Freshness(ctx context.Context) (StatusFreshness, error) {
	var out StatusFreshness

	active, err := p.queryCount(ctx, activeSourcesQuery)
	if err != nil {
		return StatusFreshness{}, err
	}
	total, err := p.queryCount(ctx, totalSourcesQuery)
	if err != nil {
		return StatusFreshness{}, err
	}
	out.ActiveSources, out.TotalSources = &active, &total

	// Last aggregator tick is the most recent vwap-write counter increment.
	res, err := p.queryVector(ctx, `max(timestamp(stellarindex_aggregator_vwap_writes_total))`)
	if err != nil {
		return StatusFreshness{}, err
	}
	for _, s := range res {
		if v, ok := s.Float(); ok && v > 0 {
			out.LastAggregatorTick = WireTime(time.Unix(int64(v), 0).UTC())
		}
	}

	return out, nil
}

// queryCount evaluates a PromQL count(); an empty vector is a measured zero, not a failure.
func (p *PrometheusStatusBackend) queryCount(ctx context.Context, expr string) (int, error) {
	res, err := p.queryVector(ctx, expr)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, s := range res {
		if v, ok := s.Float(); ok {
			n = int(v)
		}
	}
	return n, nil
}

func (p *PrometheusStatusBackend) Incidents(ctx context.Context) (StatusIncidents, error) {
	var out StatusIncidents

	// One ALERTS query yields names and per-severity counts. It excludes the deadmansswitch (fires
	// by design) and recording-rule artefacts with alertname="".
	res, err := p.queryVector(ctx,
		`ALERTS{alertstate="firing",alertname!="stellarindex_deadmansswitch",alertname!=""}`)
	if err != nil {
		return out, err
	}

	const maxActive = 16
	seen := make(map[string]bool, len(res))
	for _, sample := range res {
		name, _ := sample.Labels["alertname"].(string)
		severity, _ := sample.Labels["severity"].(string)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true

		// Only the three documented severities are published verbatim; an alert can set severity
		// freely, so anything else is normalized to "unknown" before reaching the public JSON.
		switch severity {
		case "page":
			out.PageCount++
		case "ticket":
			out.TicketCount++
		case "informational":
			out.InformationalCount++
		default:
			severity = "unknown"
		}

		if len(out.Active) < maxActive {
			runbookURL, _ := sample.Labels["runbook_url"].(string)
			out.Active = append(out.Active, ActiveIncident{
				Name:       name,
				Severity:   severity,
				RunbookURL: runbookURL,
			})
		}
	}

	// Most urgent first, so truncation drops the least important.
	sort.SliceStable(out.Active, func(i, j int) bool {
		return severityRank(out.Active[i].Severity) < severityRank(out.Active[j].Severity)
	})

	out.ActiveCount = out.PageCount + out.TicketCount + out.InformationalCount
	return out, nil
}

func severityRank(s string) int {
	switch s {
	case "page":
		return 0
	case "ticket":
		return 1
	case "informational":
		return 2
	default:
		return 3
	}
}

// promSample is one (metric, value) pair from a Prometheus instant query.
type promSample struct {
	Labels map[string]any
	Value  []any // [unix_ts, "value"]
}

func (s promSample) Float() (float64, bool) {
	if len(s.Value) != 2 {
		return 0, false
	}
	str, ok := s.Value[1].(string)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(str, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// SourceEntries24h implements [StatusBackend]. See the interface doc.
func (p *PrometheusStatusBackend) SourceEntries24h(ctx context.Context) (map[string]int64, error) {
	const q = `sum by (source) (increase(stellarindex_source_events_total[24h]))`
	res, err := p.queryVector(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(res))
	for _, sample := range res {
		src, _ := sample.Labels["source"].(string)
		if src == "" {
			continue
		}
		if v, ok := sample.Float(); ok && v > 0 {
			out[src] = int64(v + 0.5) // round to nearest whole event
		}
	}
	return out, nil
}

// SourceEnabled implements [StatusBackend]. See the interface doc.
func (p *PrometheusStatusBackend) SourceEnabled(ctx context.Context) (map[string]bool, error) {
	const q = `stellarindex_source_enabled == 1`
	res, err := p.queryVector(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(res))
	for _, sample := range res {
		src, _ := sample.Labels["source"].(string)
		if src == "" {
			continue
		}
		out[src] = true
	}
	return out, nil
}

func (p *PrometheusStatusBackend) queryVector(ctx context.Context, expr string) ([]promSample, error) {
	if p.URL == "" {
		return nil, fmt.Errorf("prometheus URL not configured")
	}
	u := p.URL + "/api/v1/query?query=" + url.QueryEscape(expr)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus HTTP %d", resp.StatusCode)
	}
	var env struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Metric map[string]any `json:"metric"`
				Value  []any          `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, err
	}
	if env.Status != "success" {
		return nil, fmt.Errorf("prometheus query non-success")
	}
	out := make([]promSample, 0, len(env.Data.Result))
	for _, r := range env.Data.Result {
		out = append(out, promSample{Labels: r.Metric, Value: r.Value})
	}
	return out, nil
}

// handleStatus serves GET /v1/status anonymously. It always returns 200; `overall` carries the
// degraded state so dashboards can poll one endpoint without alerting on 503s.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	out := StatusResponse{
		Overall: "ok",
		Region: StatusRegion{
			Name:       s.regionName,
			Deployment: s.regionDeployment,
		},
		Services: []StatusService{
			{Name: "api", Status: "ok", LastSeen: WireTime(time.Now().UTC())},
		},
	}

	if s.StatusBackend == nil {
		// No backend: every heartbeat and the incidents signal are unknown, since zero counts
		// without an Alertmanager query are absence of signal, not an all-clear.
		out.IncidentsStatus = incidentsStatusUnknown
		out.FreshnessStatus = freshnessStatusUnknown
		out.Services = append(out.Services, unknownServices(s.statusServices)...)
		out.Overall = rollupOverall(out.Services, false, false, false)
		writeJSON(w, out, Flags{Stale: out.Overall != "ok"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	// The four queries run concurrently so wall time is the slowest one, not the sum.
	var (
		wg             sync.WaitGroup
		hb             map[string]time.Time
		latency        StatusLatency
		freshness      StatusFreshness
		incidents      StatusIncidents
		hbErr, latErr  error
		freErr, incErr error
	)
	wg.Add(4)
	// Each read runs DETACHED from the handler goroutine, so middleware.Recoverer does not cover
	// it and a panic would take the API down. Recovering leaves the value zero and err nil, which
	// consumers treat as "backend returned nothing": that panel degrades and the rest renders.
	go func() {
		defer wg.Done()
		defer worker.Recover(s.logger, "api-status-heartbeats")
		hb, hbErr = s.StatusBackend.Heartbeats(ctx)
	}()
	go func() {
		defer wg.Done()
		defer worker.Recover(s.logger, "api-status-latency")
		latency, latErr = s.StatusBackend.Latency(ctx)
	}()
	go func() {
		defer wg.Done()
		defer worker.Recover(s.logger, "api-status-freshness")
		freshness, freErr = s.StatusBackend.Freshness(ctx)
	}()
	go func() {
		defer wg.Done()
		defer worker.Recover(s.logger, "api-status-incidents")
		incidents, incErr = s.StatusBackend.Incidents(ctx)
	}()
	wg.Wait()

	// A nil heartbeat map means "we could not look", and the wire cannot say so: every declared
	// service renders "unknown" and rollupOverall turns that into "degraded". Backend-unreachable
	// and answered-with-nothing are INDISTINGUISHABLE, so an unreachable Prometheus reports
	// degraded while every unit is active. Refusing to claim "ok" beats a false all-clear; a
	// `services_status` field could carry the reason but is a public wire change.
	if hbErr != nil {
		hb = nil
	}
	out.Services = append(out.Services, heartbeatServices(s.statusServices, hb)...)

	if latErr == nil {
		out.Latency = latency
		// Echo the targets the roll-up judges against so the page cannot drift from them.
		out.Latency.P95TargetMs = statusLatencyP95TargetMs
		out.Latency.P99TargetMs = statusLatencyP99TargetMs
	}
	if freErr == nil {
		out.Freshness = freshness
	}
	out.FreshnessStatus = freshnessStatusFor(out.Freshness, freErr)
	// On a failed query the counts stay zero but the block is marked "unknown", so a downstream
	// `?? 0` chain cannot render "0 active alerts" while alerting is blind.
	if incErr == nil {
		out.Incidents = incidents
	}
	out.IncidentsStatus = incidentsStatusFor(incidents, incErr)

	backendErr := hbErr != nil || latErr != nil || freErr != nil || incErr != nil
	pageFiring := incErr == nil && incidents.PageCount > 0
	// A breached latency SLO is degradation by definition (see rollupOverall).
	latencyBreached := latErr == nil && out.Latency.breached()
	out.Overall = rollupOverall(out.Services, backendErr, pageFiring, latencyBreached)

	// flags.stale trips on any non-"ok" overall so polling clients can tell a healthy
	// snapshot from a degraded one without parsing every per-service entry.
	writeJSON(w, out, Flags{Stale: out.Overall != "ok"})
}

// unknownServices marks every declared service "unknown" for the no-backend surface, which
// rollupOverall turns into a non-"ok" overall rather than a silent all-clear.
func unknownServices(names []string) []StatusService {
	out := make([]StatusService, 0, len(names))
	for _, name := range names {
		out = append(out, StatusService{Name: name, Status: "unknown"})
	}
	return out
}

// heartbeatServices grades each DECLARED service against the heartbeat map: older than statusHeartbeatStaleAfter is down; missing or zero
// (incl. a nil map) is "unknown", never an all-clear.
// Undeclared services are not graded, or a permanent "unknown" would pin `overall` at "degraded" on lean deployments.
func heartbeatServices(names []string, hb map[string]time.Time) []StatusService {
	now := time.Now().UTC()
	out := make([]StatusService, 0, len(names))
	for _, name := range names {
		svc := StatusService{Name: name, Status: "unknown"}
		if t, ok := hb[name]; ok && !t.IsZero() {
			svc.LastSeen = WireTime(t)
			if now.Sub(t) <= statusHeartbeatStaleAfter {
				svc.Status = "ok"
			} else {
				svc.Status = "down"
			}
		}
		out = append(out, svc)
	}
	return out
}

// statusHeartbeatStaleAfter is the heartbeat age beyond which a service is "down".
const statusHeartbeatStaleAfter = 60 * time.Second

// rollupOverall computes `overall` from the per-service rollup plus backend-unreachable and page-alert signals. Worst wins: down;
// degraded (incl. mixed ok+unknown, an SLO breach);
// unknown (every service unknown, so a full metrics outage never reads ok); ok. TICKET incidents and an active-source SHORTFALL are
// deliberately NOT inputs:
// `page` means customers are affected, and changing that changes what "ok" PROMISES publicly.
func rollupOverall(services []StatusService, backendErr, pageFiring, latencyBreached bool) string {
	var anyDown, anyDegraded, anyOK, anyUnknown bool
	for _, svc := range services {
		switch svc.Status {
		case "down":
			anyDown = true
		case "degraded":
			anyDegraded = true
		case "ok":
			// "ok" with a zero LastSeen was never observed, so it counts as unknown; the in-process
			// "api" entry stamps LastSeen per request and is a real "ok".
			if svc.LastSeen.IsZero() && svc.Name != "api" {
				anyUnknown = true
			} else {
				anyOK = true
			}
		default:
			anyUnknown = true
		}
	}

	switch {
	case anyDown:
		return "down"
	case anyDegraded, backendErr, pageFiring, latencyBreached:
		return "degraded"
	case anyUnknown && !anyOK:
		return "unknown"
	case anyUnknown && anyOK:
		// Partial visibility surfaces as degraded, never full green.
		return "degraded"
	default:
		return "ok"
	}
}
