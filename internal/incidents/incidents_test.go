package incidents

import (
	"strings"
	"testing"
)

// TestLoad_RealCorpus exercises the actual embedded markdown
// files. At time of writing the corpus is one real incident
// (2026-05-06 Postgres lock-table) plus _template.md (skipped).
//
// A new entry shipping with a future PR should still make this
// pass — Load is content-shape agnostic; this asserts the loader
// round-trips the embedded fixtures.
func TestLoad_RealCorpus(t *testing.T) {
	got, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("Load: returned 0 incidents; expected at least one real entry under data/")
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

// TestLoad_SortedByStartedAtDesc — newest first.
//
// Exercises the sort.Slice in Load with whatever the corpus is.
// One-element corpus passes vacuously; multi-element catches
// regressions.
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

// An out-of-enum severity or status must fail the post loudly rather
// than shipping through. The OpenAPI contract declares both as required
// enums, and every downstream severity mapping is a ternary chain whose
// final branch renders "maintenance" — GREEN on the public status page.
// So a one-keystroke slip (`sev-1`) used to publish an ongoing SEV-1
// looking like routine maintenance, while also breaking the typed
// clients (cold audit 2026-08-03).
func TestParseFile_RejectsOutOfEnumSeverityAndStatus(t *testing.T) {
	t.Parallel()

	const good = `---
title: Test
severity: SEV-1
status: investigating
started_at: 2026-05-06T10:00:00Z
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
			src := "---\ntitle: Test\n" + frontmatter + "\nstarted_at: 2026-05-06T10:00:00Z\n---\nbody\n"
			if _, err := parseSource("2026-05-06-x.md", src); err == nil {
				t.Errorf("accepted an out-of-enum post — it would publish with a green badge")
			}
		})
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
---
body
`
	if _, err := parseSource("2026-05-06-x.md", src); err == nil {
		t.Error("accepted a malformed resolved_at — the incident publishes as never-resolved")
	}
}
