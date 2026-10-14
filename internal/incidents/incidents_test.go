package incidents

import (
	"io/fs"
	"strings"
	"testing"
)

// TestLoad_RealCorpus exercises the embedded markdown files (_template.md skipped). Load silently drops a post that fails
// frontmatter validation, so a `len(got) != 0` check cannot tell all-parsed from all-but-one; cross-check against every
// non-template *.md the binary embeds, which is what /v1/incidents and the status page serve.
func TestLoad_RealCorpus(t *testing.T) {
	got, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("Load: returned 0 incidents; expected at least one real entry under data/")
	}

	entries, err := fs.ReadDir(contentFS, "data")
	if err != nil {
		t.Fatalf("ReadDir(data): %v", err)
	}
	wantCount := 0
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), "_") || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		wantCount++
	}
	if wantCount == 0 {
		t.Fatalf("no non-template *.md posts embedded under data/; the test would be vacuous")
	}
	if len(got) != wantCount {
		t.Fatalf("Load: parsed %d incident(s), but data/ embeds %d non-template .md file(s) — a post is being silently dropped (see the Warn log for which)", len(got), wantCount)
	}

	for i, inc := range got {
		if inc.Slug == "" {
			t.Errorf("[%d] empty slug", i)
		}
		if inc.Title == "" {
			t.Errorf("[%d] %s: empty title", i, inc.Slug)
		}
		if inc.StartedAt.IsZero() {
			t.Errorf("[%d] %s: zero StartedAt", i, inc.Slug)
		}
		if strings.HasPrefix(inc.Slug, "_") {
			t.Errorf("[%d] %s: template prefix '_' should be skipped", i, inc.Slug)
		}
	}
}

// TestLoad_SortedByStartedAtDesc: newest first. A one-element corpus passes vacuously;
// multi-element catches regressions in Load's sort.
func TestLoad_SortedByStartedAtDesc(t *testing.T) {
	got, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].StartedAt.Before(got[i].StartedAt) {
			t.Errorf("[%d] sort regression: %s started %s but precedes %s started %s",
				i, got[i-1].Slug, got[i-1].StartedAt,
				got[i].Slug, got[i].StartedAt)
		}
	}
}

// TestSplitFrontmatter_Happy — well-formed file separates cleanly.
func TestSplitFrontmatter_Happy(t *testing.T) {
	src := "---\ntitle: Hello\nseverity: SEV-3\n---\nbody line 1\nbody line 2\n"
	front, body, err := splitFrontmatter(src)
	if err != nil {
		t.Fatalf("splitFrontmatter: %v", err)
	}
	if !strings.Contains(front, "title: Hello") {
		t.Errorf("frontmatter missing title: %q", front)
	}
	if !strings.Contains(body, "body line 1") {
		t.Errorf("body missing line 1: %q", body)
	}
}

// TestSplitFrontmatter_MissingOpenDelimiter — file that doesn't
// start with `---` should error rather than silently emit garbage.
func TestSplitFrontmatter_MissingOpenDelimiter(t *testing.T) {
	src := "title: this is not a real frontmatter\nbody\n"
	_, _, err := splitFrontmatter(src)
	if err == nil {
		t.Fatal("expected error for missing opening delimiter, got nil")
	}
}

// TestSplitFrontmatter_MissingCloseDelimiter — file that opens
// `---` but never closes it should error too.
func TestSplitFrontmatter_MissingCloseDelimiter(t *testing.T) {
	src := "---\ntitle: forever frontmatter\nstill frontmatter\n"
	_, _, err := splitFrontmatter(src)
	if err == nil {
		t.Fatal("expected error for missing closing delimiter, got nil")
	}
}

// TestParseTimestamp_RFC3339 — the expected canonical shape.
func TestParseTimestamp_RFC3339(t *testing.T) {
	got, err := parseTimestamp("2026-05-06T22:39:00Z", "")
	if err != nil {
		t.Fatalf("parseTimestamp: %v", err)
	}
	if got.Year() != 2026 || got.Month() != 5 || got.Day() != 6 {
		t.Errorf("got %v want 2026-05-06", got)
	}
}

// TestParseTimestamp_DateFallback — `started_at:` empty falls
// back to the `date:` field which is YYYY-MM-DD only.
func TestParseTimestamp_DateFallback(t *testing.T) {
	got, err := parseTimestamp("", "2026-05-06")
	if err != nil {
		t.Fatalf("parseTimestamp: %v", err)
	}
	if got.Year() != 2026 || got.Month() != 5 || got.Day() != 6 {
		t.Errorf("got %v want 2026-05-06", got)
	}
	if got.Hour() != 0 || got.Minute() != 0 {
		t.Errorf("date-only fallback should land at midnight UTC, got %v", got)
	}
}

// TestParseTimestamp_NoneParseable — both inputs garbage.
func TestParseTimestamp_NoneParseable(t *testing.T) {
	_, err := parseTimestamp("not-a-date", "also-garbage")
	if err == nil {
		t.Fatal("expected error for unparseable inputs, got nil")
	}
}

// An out-of-enum severity or status must fail the post loudly: the OpenAPI contract requires both enums and severity mapping
// falls through to "maintenance" (GREEN), so a slip like `sev-1` would publish an ongoing SEV-1 as routine maintenance.
func TestParseFile_RejectsOutOfEnumSeverityAndStatus(t *testing.T) {
	t.Parallel()

	const good = `---
title: Test
severity: SEV-1
status: investigating
started_at: 2026-05-06T10:00:00Z
affected_components: [api]
---
body
`
	if _, err := parseSource("2026-05-06-x.md", good); err != nil {
		t.Fatalf("valid post rejected: %v", err)
	}

	for name, frontmatter := range map[string]string{
		"lowercase severity": "severity: sev-1\nstatus: investigating",
		"unknown severity":   "severity: SEV-9\nstatus: investigating",
		"missing severity":   "status: investigating",
		"capitalised status": "severity: SEV-1\nstatus: Investigating",
		"unknown status":     "severity: SEV-1\nstatus: pending",
		"missing status":     "severity: SEV-1",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			src := "---\ntitle: Test\n" + frontmatter + "\nstarted_at: 2026-05-06T10:00:00Z\naffected_components: [api]\n---\nbody\n"
			if _, err := parseSource("2026-05-06-x.md", src); err == nil {
				t.Errorf("accepted an out-of-enum post — it would publish with a green badge")
			}
		})
	}
}

// TestParseSource_RejectsUnknownFrontmatterField guards that a misspelled frontmatter key (e.g. "sevority") fails the post
// loudly instead of parsing with severity/status at zero value and publishing with missing data.
func TestParseSource_RejectsUnknownFrontmatterField(t *testing.T) {
	t.Parallel()

	const src = `---
title: Test
sevority: SEV-1
status: investigating
started_at: 2026-05-06T10:00:00Z
---
body
`
	_, err := parseSource("2026-05-06-x.md", src)
	if err == nil {
		t.Fatal("parseSource: nil err, want error rejecting unknown field sevority")
	}
	if !strings.Contains(err.Error(), "sevority") {
		t.Errorf("err = %q, want it to name the unknown field sevority", err)
	}
}

// TestLoad_RedisDiskFullPostmortem_NoDanglingPRRefs: the Redis disk-full post must not cite
// "PR #1228" or "PR #1229", which do not exist. The loader publishes BodyMarkdown verbatim to
// the status site, so readers would hit a 404; the real remediation commits are cited by SHA.
func TestLoad_RedisDiskFullPostmortem_NoDanglingPRRefs(t *testing.T) {
	got, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const slug = "2026-05-10-redis-writes-blocked-disk-full"
	var found bool
	for _, inc := range got {
		if inc.Slug != slug {
			continue
		}
		found = true
		for _, dangling := range []string{"#1228", "#1229"} {
			if strings.Contains(inc.BodyMarkdown, dangling) {
				t.Errorf("%s: body still cites dangling reference %q (no such issue/PR exists in this repo)",
					slug, dangling)
			}
		}
	}
	if !found {
		t.Fatalf("incident %q not found in corpus", slug)
	}
}

// TestLoad_CredentialPIIExposureIncidentRecord asserts the customer-facing record for the log-store credential/PII exposure
// exists, parses, and is "resolved" (neither dropped as malformed nor stuck at "investigating").
func TestLoad_CredentialPIIExposureIncidentRecord(t *testing.T) {
	got, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const slug = "2026-09-02-log-store-credential-pii-exposure"
	var found *Incident
	for i := range got {
		if got[i].Slug == slug {
			found = &got[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("incident record %q not found in corpus — the #480 credential/PII exposure has no filed incident", slug)
	}
	if found.Status != StatusResolved {
		t.Errorf("%s: status = %q, want %q", slug, found.Status, StatusResolved)
	}
	if found.Severity != SeverityMinor {
		t.Errorf("%s: severity = %q, want %q", slug, found.Severity, SeverityMinor)
	}
}

// A malformed resolved_at must fail rather than silently publishing a
// RESOLVED incident as never-resolved.
func TestParseFile_RejectsMalformedResolvedAt(t *testing.T) {
	t.Parallel()

	src := `---
title: Test
severity: SEV-2
status: resolved
started_at: 2026-05-06T10:00:00Z
resolved_at: not-a-date
affected_components: [api]
---
body
`
	if _, err := parseSource("2026-05-06-x.md", src); err == nil {
		t.Error("accepted a malformed resolved_at — the incident publishes as never-resolved")
	}
}

// A blank affected_components must fail: the webhook contract requires a
// non-empty array, and a post without one would fan out as null or [].
func TestParseSource_RequiresAffectedComponents(t *testing.T) {
	t.Parallel()

	const head = "---\ntitle: Test\nseverity: SEV-1\nstatus: investigating\nstarted_at: 2026-05-06T10:00:00Z\n"
	got, err := parseSource("2026-05-06-x.md", head+"affected_components:\n  - \" api \"\n---\nbody\n")
	if err != nil {
		t.Fatalf("valid post rejected: %v", err)
	}
	if len(got.AffectedComponents) != 1 || got.AffectedComponents[0] != "api" {
		t.Errorf("AffectedComponents = %q, want [api]", got.AffectedComponents)
	}

	for name, line := range map[string]string{
		"missing key": "",
		"blank key":   "affected_components:\n",
		"empty list":  "affected_components: []\n",
		"blank entry": "affected_components:\n  - api\n  - \"  \"\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := parseSource("2026-05-06-x.md", head+line+"---\nbody\n")
			if err == nil || !strings.Contains(err.Error(), "affected_components") {
				t.Errorf("err = %v, want an affected_components rejection", err)
			}
		})
	}
}
