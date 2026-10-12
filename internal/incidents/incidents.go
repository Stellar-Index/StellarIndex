// Package incidents loads customer-facing incident post-mortems from `internal/incidents/data/*.md`
// (embedded via go:embed) and exposes the parsed list to API handlers. The checked-in directory is
// the source of truth for the status page; there is no runtime write path.
//
// Files are named `<YYYY-MM-DD>-<short-slug>.md`; files starting with `_` are templates and are
// skipped. A file that fails YAML frontmatter validation is dropped with a log entry and never
// panics the binary, so one bad post cannot break the rest of the feed.
package incidents

import (
	"bufio"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed data/*.md
var contentFS embed.FS

// Severity matches the SEV-1/2/3 convention from docs/operations/sev-playbook.md; customer-facing only, internal SEV-A/B/C ladders are not exposed.
type Severity string

const (
	SeverityMajor       Severity = "SEV-1"
	SeverityMinor       Severity = "SEV-2"
	SeverityInformative Severity = "SEV-3"
)

// Status tracks the customer-facing incident lifecycle, mirroring Statuspage-style fields so status-page UIs can render familiar pills.
type Status string

const (
	StatusInvestigating Status = "investigating"
	StatusIdentified    Status = "identified"
	StatusMonitoring    Status = "monitoring"
	StatusResolved      Status = "resolved"
)

// valid reports whether s is one of the three published severities.
//
// The OpenAPI contract declares `severity` as a required enum [SEV-1, SEV-2, SEV-3]. Casting the
// raw YAML string would let a typo (`sev-1`) reach typed clients, and downstream severity mappings
// fall through to `maintenance`, which the status page renders green, so an ongoing SEV-1 could
// look routine. Validating here fails the post loudly instead.
func (s Severity) valid() bool {
	switch s {
	case SeverityMajor, SeverityMinor, SeverityInformative:
		return true
	}
	return false
}

// valid reports whether s is one of the four published statuses. Same
// rationale as [Severity.valid] — and note the explorer's build-time
// loader defaults a MISSING status to "resolved", so an unvalidated
// blank would print "Resolved" during a live outage.
func (s Status) valid() bool {
	switch s {
	case StatusInvestigating, StatusIdentified, StatusMonitoring, StatusResolved:
		return true
	}
	return false
}

// Incident is one customer-facing incident post.
type Incident struct {
	Slug               string     `json:"slug"`
	Title              string     `json:"title"`
	Severity           Severity   `json:"severity"`
	Status             Status     `json:"status"`
	StartedAt          time.Time  `json:"started_at"`
	ResolvedAt         *time.Time `json:"resolved_at,omitempty"`
	AffectedComponents []string   `json:"affected_components,omitempty"`
	PostmortemRef      string     `json:"postmortem,omitempty"`
	BodyMarkdown       string     `json:"body_markdown"`
}

// rawFrontmatter is the YAML shape we parse out of each file's
// `---`-delimited header. Strings everywhere; conversion to
// typed fields happens after parse so a bad date doesn't fail
// the YAML pass.
type rawFrontmatter struct {
	Title              string   `yaml:"title"`
	Severity           string   `yaml:"severity"`
	Status             string   `yaml:"status"`
	StartedAt          string   `yaml:"started_at"`
	ResolvedAt         string   `yaml:"resolved_at"`
	AffectedComponents []string `yaml:"affected_components"`
	Postmortem         string   `yaml:"postmortem"`
	Date               string   `yaml:"date"`
}

// Load parses every `data/*.md` file in the embedded directory, newest `started_at` first, skipping `_`-prefixed files; per-file errors are logged and skipped, and it fails only if the embedded FS is unreadable.
func Load(logger *slog.Logger) ([]Incident, error) {
	if logger == nil {
		logger = slog.Default()
	}
	entries, err := contentFS.ReadDir("data")
	if err != nil {
		return nil, fmt.Errorf("incidents: read embedded dir: %w", err)
	}

	var out []Incident
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "_") {
			continue
		}
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		inc, err := parseFile(name)
		if err != nil {
			logger.Warn("incidents: skip malformed post",
				"file", name, "err", err)
			continue
		}
		out = append(out, inc)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out, nil
}

// parseFile reads `data/<name>` and returns one Incident. The body
// is everything after the closing `---` line.
func parseFile(name string) (Incident, error) {
	raw, err := fs.ReadFile(contentFS, "data/"+name)
	if err != nil {
		return Incident{}, fmt.Errorf("read: %w", err)
	}
	return parseSource(name, string(raw))
}

// parseSource parses one post's raw bytes. Split out of [parseFile] so
// the frontmatter rules — especially the enum and timestamp validation
// below, which decide whether a post is published at all — are testable
// without writing into the embedded corpus.
func parseSource(name, raw string) (Incident, error) {
	frontYAML, body, err := splitFrontmatter(raw)
	if err != nil {
		return Incident{}, fmt.Errorf("split: %w", err)
	}

	var fm rawFrontmatter
	dec := yaml.NewDecoder(strings.NewReader(frontYAML))
	dec.KnownFields(true) // reject a misspelled/renamed key instead of silently dropping it
	if err := dec.Decode(&fm); err != nil {
		return Incident{}, fmt.Errorf("yaml: %w", err)
	}

	startedAt, err := parseTimestamp(fm.StartedAt, fm.Date)
	if err != nil {
		return Incident{}, fmt.Errorf("started_at: %w", err)
	}
	var resolvedAt *time.Time
	if fm.ResolvedAt != "" {
		t, err := parseTimestamp(fm.ResolvedAt, "")
		if err != nil {
			// Do NOT silently drop it: a nil resolved_at publishes a RESOLVED incident as
			// never-resolved (/v1/incidents omits the field, the Atom <updated> falls back to
			// started_at, and `emit-incident -event resolved` ships no resolved_at while still
			// passing its status==resolved gate).
			return Incident{}, fmt.Errorf("resolved_at: %w", err)
		}
		resolvedAt = &t
	}

	severity := Severity(strings.TrimSpace(fm.Severity))
	if !severity.valid() {
		return Incident{}, fmt.Errorf("severity: %q is not one of %s/%s/%s",
			fm.Severity, SeverityMajor, SeverityMinor, SeverityInformative)
	}
	status := Status(strings.TrimSpace(fm.Status))
	if !status.valid() {
		return Incident{}, fmt.Errorf("status: %q is not one of %s/%s/%s/%s",
			fm.Status, StatusInvestigating, StatusIdentified, StatusMonitoring, StatusResolved)
	}
	// The webhook contract requires a non-empty array; a blank key would
	// otherwise fan out as null or [] to every subscriber.
	components := make([]string, 0, len(fm.AffectedComponents))
	for _, c := range fm.AffectedComponents {
		c = strings.TrimSpace(c)
		if c == "" {
			return Incident{}, fmt.Errorf("affected_components: blank entry")
		}
		components = append(components, c)
	}
	if len(components) == 0 {
		return Incident{}, fmt.Errorf("affected_components: at least one component is required")
	}

	slug := strings.TrimSuffix(name, ".md")
	return Incident{
		Slug:               slug,
		Title:              strings.TrimSpace(fm.Title),
		Severity:           severity,
		Status:             status,
		StartedAt:          startedAt,
		ResolvedAt:         resolvedAt,
		AffectedComponents: components,
		PostmortemRef:      strings.TrimSpace(fm.Postmortem),
		BodyMarkdown:       strings.TrimSpace(body),
	}, nil
}

// splitFrontmatter splits a markdown file with YAML frontmatter
// delimited by `---` lines. Returns (frontYAML, body, err).
// File MUST start with `---` on its own line; otherwise we treat
// it as bodyless garbage and return an error.
func splitFrontmatter(src string) (string, string, error) {
	scanner := bufio.NewScanner(strings.NewReader(src))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var (
		state       = "expect-open"
		front, body strings.Builder
		seenClose   bool
	)
	for scanner.Scan() {
		line := scanner.Text()
		switch state {
		case "expect-open":
			if strings.TrimSpace(line) != "---" {
				return "", "", fmt.Errorf("missing opening --- delimiter")
			}
			state = "in-front"
		case "in-front":
			if strings.TrimSpace(line) == "---" {
				state = "in-body"
				seenClose = true
				continue
			}
			front.WriteString(line)
			front.WriteByte('\n')
		case "in-body":
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return "", "", err
	}
	if !seenClose {
		return "", "", fmt.Errorf("missing closing --- delimiter")
	}
	return front.String(), body.String(), nil
}

// parseTimestamp accepts RFC 3339 first, then falls back to a
// date-only YYYY-MM-DD (treated as midnight UTC). The fallback
// argument lets the parser pick up `date:` from frontmatter when
// `started_at:` is missing — older posts may only have one.
func parseTimestamp(primary, fallback string) (time.Time, error) {
	tries := []string{primary, fallback}
	for _, s := range tries {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.UTC(), nil
		}
		if t, err := time.Parse("2006-01-02", s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("no parseable timestamp in %q / %q", primary, fallback)
}
