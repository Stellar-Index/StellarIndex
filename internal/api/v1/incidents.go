package v1

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Stellar-Index/StellarIndex/internal/incidents"
)

// IncidentsList is the /v1/incidents shape; `incidents` is sorted started_at desc by the loader.
type IncidentsList struct {
	Incidents []incidents.Incident `json:"incidents"`
	Count     int                  `json:"count"`
}

// handleIncidents serves GET /v1/incidents.
//
// Returns every incident post embedded in the binary (`internal/incidents/data/*.md`), parsed at
// startup and cached on the Server; new posts ship with a redeploy. There is no filtering or
// pagination because the corpus is a few entries per year.
func (s *Server) handleIncidents(w http.ResponseWriter, r *http.Request) {
	// Nil-to-empty: a fresh deployment with no embedded posts
	// (s.incidents == nil) would otherwise marshal as `null` rather
	// than `[]`, breaking the pkg/client SDK + explorer JS that
	// .map() over the array.
	rows := s.incidents
	if rows == nil {
		rows = []incidents.Incident{}
	}
	writeJSON(w, IncidentsList{
		Incidents: rows,
		Count:     len(rows),
	}, Flags{})
}

// atomFeed mirrors the RFC-4287 Atom syndication shape we emit.
// Hand-rolled rather than pulling in a feed-generator dep —
// Atom has only a handful of required fields and the writer
// stays under a screen of code.
type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Xmlns   string      `xml:"xmlns,attr"`
	ID      string      `xml:"id"`
	Title   string      `xml:"title"`
	Link    []atomLink  `xml:"link"`
	Updated string      `xml:"updated"`
	Author  *atomAuthor `xml:"author,omitempty"`
	Entries []atomEntry `xml:"entry"`
}

type atomLink struct {
	Rel  string `xml:"rel,attr,omitempty"`
	Href string `xml:"href,attr"`
	Type string `xml:"type,attr,omitempty"`
}

type atomAuthor struct {
	Name string `xml:"name"`
	URI  string `xml:"uri,omitempty"`
}

type atomEntry struct {
	ID      string     `xml:"id"`
	Title   string     `xml:"title"`
	Link    []atomLink `xml:"link"`
	Updated string     `xml:"updated"`
	Summary string     `xml:"summary,omitempty"`
	Content struct {
		Type     string `xml:"type,attr"`
		CharData string `xml:",chardata"`
	} `xml:"content"`
}

// handleIncidentsAtom serves GET /v1/incidents.atom.
//
// Atom 1.0 feed of the same data as /v1/incidents, with <link> pointing at the status page. Per
// RFC 4287 the feed's `<updated>` is the most recent `<entry>`'s `<updated>`; each entry's `<id>`
// is a stable URN derived from the slug so feed readers dedupe across crawls.
func (s *Server) handleIncidentsAtom(w http.ResponseWriter, r *http.Request) {
	const baseURL = "https://status.stellarindex.io"
	// feedEstablished is the stable <updated> for a feed with zero entries: time.Now() would make
	// an empty feed look freshly updated on every crawl, a dishonest freshness signal.
	feedEstablished := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	feed := atomFeed{
		Xmlns: "http://www.w3.org/2005/Atom",
		ID:    baseURL + "/feed",
		Title: "Stellar Index — incident history",
		Link: []atomLink{
			{Rel: "self", Href: "https://api.stellarindex.io/v1/incidents.atom", Type: "application/atom+xml"},
			{Rel: "alternate", Href: baseURL + "/", Type: "text/html"},
		},
		Author: &atomAuthor{Name: "Stellar Index", URI: "https://stellarindex.io"},
	}

	// Per RFC 4287 the feed-level <updated> is the most recent entry's
	// <updated>. Track the max of the same per-entry `updated` value we
	// compute below (started_at, bumped to resolved_at when later) so
	// the two never diverge.
	var feedUpdated time.Time
	for _, inc := range s.incidents {
		updated := inc.StartedAt
		if inc.ResolvedAt != nil && inc.ResolvedAt.After(updated) {
			updated = *inc.ResolvedAt
		}
		if updated.After(feedUpdated) {
			feedUpdated = updated
		}
		entry := atomEntry{
			ID:      fmt.Sprintf("urn:stellarindex:incident:%s", inc.Slug),
			Title:   inc.Title,
			Updated: updated.UTC().Format(time.RFC3339),
			Link: []atomLink{
				// Per-incident detail page, not a homepage `#<slug>` anchor: the home page renders no
				// `id` per incident, so readers would land with no scroll target.
				{Rel: "alternate", Href: baseURL + "/incident/" + inc.Slug, Type: "text/html"},
			},
			Summary: summaryFromMarkdown(inc.BodyMarkdown),
		}
		entry.Content.Type = "text"
		entry.Content.CharData = inc.BodyMarkdown
		feed.Entries = append(feed.Entries, entry)
	}

	// No entries → emit the stable sentinel rather than time.Now().
	if feedUpdated.IsZero() {
		feedUpdated = feedEstablished
	}
	feed.Updated = feedUpdated.UTC().Format(time.RFC3339)

	w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	if _, err := w.Write([]byte(xml.Header)); err != nil {
		s.logger.Warn("incidents atom write header", "err", err)
		return
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(feed); err != nil {
		s.logger.Warn("incidents atom encode", "err", err)
	}
}

// summaryFromMarkdown extracts the first paragraph from a markdown
// post — same heuristic the status page's `normaliseIncident` uses,
// kept simple so it stays readable in a feed reader.
func summaryFromMarkdown(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	// Split on blank line (paragraph break) — take the first
	// non-frontmatter paragraph.
	parts := strings.Split(body, "\n\n")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// Skip headings and HTML comments.
		if strings.HasPrefix(p, "#") || strings.HasPrefix(p, "<!--") {
			continue
		}
		if len(p) > 400 {
			// Walk back to a rune boundary at or before byte 397: a naive `p[:397]` can slice a
			// multi-byte codepoint, yielding invalid UTF-8 that strict feed validators reject whole.
			end := 397
			for end > 0 && !utf8.RuneStart(p[end]) {
				end--
			}
			p = p[:end] + "..."
		}
		return p
	}
	return ""
}
