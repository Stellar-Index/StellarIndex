package v1

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestADR0026PinsThe1224AmendmentNotice guards RSWP-121: ADR-0026's two
// "#1224" citations (Context intro, References > Implementation) no
// longer identify the `/v1/vwap` + `/v1/twap` proxy fallback they
// describe. No PR #1224 has ever existed in this repo; GitHub has
// since assigned #1224 to a real but unrelated open issue about
// `ClosedVWAPAtOrBefore`'s closed-bucket bound, so a reader following
// the citation lands on wrong content instead of a 404.
//
// Per docs/adr/README.md's amendment rule, the original ADR body is
// left intact (both "#1224" mentions stay, as historical record) and
// the correction lives in a dated Amendment blockquote. This test
// pins that the blockquote exists and still calls out both the wrong
// resolution and the unrelated target, so it can't be silently
// dropped in a future edit of the doc.
func TestADR0026PinsThe1224AmendmentNotice(t *testing.T) {
	path := filepath.Join(repoRoot(t), "docs", "adr", "0026-stablecoin-fiat-proxy-late-binding.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	doc := string(b)

	for _, want := range []string{
		"Amendment (2026-09-22, RSWP-121)",
		"No PR #1224\n> has ever existed in this repo",
		"ClosedVWAPAtOrBefore",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("%s is missing amendment text %q; the dangling #1224 citation is no longer flagged as misdirecting a reader to an unrelated issue", path, want)
		}
	}

	// The original citations must survive untouched (README.md's
	// "amend, don't rewrite" rule) — the amendment explains them, it
	// doesn't replace them.
	for _, original := range []string{
		"chain. PRs #1217 / #1218 / #1224 / #1225 / #1226 (etc.) added",
		"PR #1224 — `/v1/vwap` + `/v1/twap` proxy fallback",
	} {
		if !strings.Contains(doc, original) {
			t.Errorf("%s: original citation %q was rewritten or removed; ADR body text must only be amended, per docs/adr/README.md", path, original)
		}
	}
}

// TestADR0026PinsThe1225AmendmentNotice guards RSWP-122: ADR-0026's two
// "PR #1225" citations (Context intro's PR list, References >
// Implementation surface) no longer identify the SEP-40 oracle
// endpoints proxy fallback they describe. No PR #1225 has ever existed
// in this repo; GitHub has since assigned #1225 to a real but unrelated
// open issue about test-vacuity residue across several endpoints, so a
// reader following the citation lands on wrong content instead of a
// 404 — worse than a dead link.
//
// Per docs/adr/README.md's amendment rule, the original ADR body is
// left intact (both "#1225" mentions stay, as historical record) and
// the correction lives in a dated Amendment blockquote. This test pins
// that the blockquote exists and still calls out both the wrong
// resolution and the unrelated target, so it can't be silently dropped
// in a future edit of the doc.
func TestADR0026PinsThe1225AmendmentNotice(t *testing.T) {
	path := filepath.Join(repoRoot(t), "docs", "adr", "0026-stablecoin-fiat-proxy-late-binding.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	doc := string(b)

	for _, want := range []string{
		"Amendment (2026-09-22, RSWP-122)",
		"No PR\n> #1225 has ever existed in this repo",
		"test-vacuity residue",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("%s is missing amendment text %q; the dangling PR #1225 citation is no longer flagged as misdirecting a reader to an unrelated issue", path, want)
		}
	}

	// The original citations must survive untouched (README.md's
	// "amend, don't rewrite" rule) — the amendment explains them, it
	// doesn't replace them.
	for _, original := range []string{
		"PRs #1217 / #1218 / #1224 / #1225 / #1226 (etc.) added",
		"PR #1225 — SEP-40 oracle endpoints proxy fallback",
	} {
		if !strings.Contains(doc, original) {
			t.Errorf("%s: original citation %q was rewritten or removed; ADR body text must only be amended, per docs/adr/README.md", path, original)
		}
	}
}

// TestADR0026PinsThe1226AmendmentNotice guards RSWP-123: ADR-0026's two
// "#1226" citations (Context intro, References > Implementation) no
// longer identify the `/v1/ohlc` proxy fallback they describe. GitHub
// has since assigned #1226 to a real but unrelated merged PR (the
// `pkg/client` VWAP/TWAP/Pools SDK methods), so a reader following the
// citation lands on wrong content instead of a 404.
//
// Per docs/adr/README.md's amendment rule, the original ADR body is
// left intact (both "#1226" mentions stay, as historical record) and
// the correction lives in a dated Amendment blockquote. This test
// pins that the blockquote exists and still calls out both the wrong
// resolution and the unrelated target, so it can't be silently
// dropped in a future edit of the doc.
func TestADR0026PinsThe1226AmendmentNotice(t *testing.T) {
	path := filepath.Join(repoRoot(t), "docs", "adr", "0026-stablecoin-fiat-proxy-late-binding.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	doc := string(b)

	for _, want := range []string{
		"Amendment (2026-09-23, RSWP-123)",
		"VWAP`/`TWAP`/`Pools` SDK methods",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("%s is missing amendment text %q; the dangling #1226 citation is no longer flagged as misdirecting a reader to an unrelated PR", path, want)
		}
	}

	// The original citations must survive untouched (README.md's
	// "amend, don't rewrite" rule) — the amendment explains them, it
	// doesn't replace them.
	for _, original := range []string{
		"PRs #1217 / #1218 / #1224 / #1225 / #1226 (etc.) added",
		"PR #1226 — `/v1/ohlc` proxy fallback",
	} {
		if !strings.Contains(doc, original) {
			t.Errorf("%s: original citation %q was rewritten or removed; ADR body text must only be amended, per docs/adr/README.md", path, original)
		}
	}
}

// TestADR0026PinsThe1219AmendmentNotice guards RSWP-118: ADR-0026's
// "PR #1219" citation (References → Implementation surface) no longer
// identifies the `/v1/chart` proxy fallback it describes. No PR #1219
// has ever existed in this repo; GitHub has since assigned #1219 to a
// real but unrelated open issue about oracle_unparsed_metric_test
// incrementing the dropped-row counter itself, so a reader following
// the citation lands on wrong content instead of a 404.
//
// Per docs/adr/README.md's amendment rule, the original ADR body is
// left intact (the "#1219" mention stays, as historical record) and
// the correction lives in a dated Amendment blockquote. This test
// pins that the blockquote exists and still calls out both the wrong
// resolution and the unrelated target, so it can't be silently
// dropped in a future edit of the doc.
func TestADR0026PinsThe1219AmendmentNotice(t *testing.T) {
	path := filepath.Join(repoRoot(t), "docs", "adr", "0026-stablecoin-fiat-proxy-late-binding.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	doc := string(b)

	for _, want := range []string{
		"Amendment (2026-09-23, RSWP-118)",
		"No PR #1219 has ever\n> existed in this repo",
		"oracle_unparsed_metric_test",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("%s is missing amendment text %q; the dangling #1219 citation is no longer flagged as misdirecting a reader to an unrelated issue", path, want)
		}
	}

	// The original citation must survive untouched (README.md's
	// "amend, don't rewrite" rule) — the amendment explains it, it
	// doesn't replace it.
	if original := "PR #1219 — `/v1/chart` proxy fallback"; !strings.Contains(doc, original) {
		t.Errorf("%s: original citation %q was rewritten or removed; ADR body text must only be amended, per docs/adr/README.md", path, original)
	}
}
