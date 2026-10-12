package v1

import (
	"context"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestEnrichIssuer_DBWins: when home_domain and org_name both come from the DB, the curated
// map MUST NOT override. The DB (refreshed from SEP-1) is the source of truth; the static map
// is only a fallback for issuers not yet resolved.
func TestEnrichIssuer_DBWins(t *testing.T) {
	usdc := "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	domain, org := enrichIssuer(usdc, "centre.io.fresh", "Circle Fresh")
	if domain != "centre.io.fresh" {
		t.Errorf("domain = %q; want centre.io.fresh (DB value should win)", domain)
	}
	if org != "Circle Fresh" {
		t.Errorf("org = %q; want \"Circle Fresh\" (DB value should win)", org)
	}
}

// TestEnrichIssuer_FallbackOnEmpty — the curated map fills in only
// when the DB column is empty. Tests both fields independently.
func TestEnrichIssuer_FallbackOnEmpty(t *testing.T) {
	usdc := "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

	domain, org := enrichIssuer(usdc, "", "")
	if domain != "circle.com" {
		t.Errorf("empty/empty: domain = %q; want circle.com", domain)
	}
	if org != "Circle" {
		t.Errorf("empty/empty: org = %q; want Circle", org)
	}

	// Partial — DB has org but not domain.
	domain, org = enrichIssuer(usdc, "", "Circle Custom")
	if domain != "circle.com" {
		t.Errorf("partial: domain = %q; want circle.com (filled from map)", domain)
	}
	if org != "Circle Custom" {
		t.Errorf("partial: org = %q; want Circle Custom (DB value preserved)", org)
	}
}

// TestEnrichIssuer_UnknownPassthrough — a G-strkey not in the
// curated map returns whatever the DB had, even if empty. The
// caller then renders empty as a truncated G-strkey.
func TestEnrichIssuer_UnknownPassthrough(t *testing.T) {
	unknown := "GUNKNOWNAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	domain, org := enrichIssuer(unknown, "", "")
	if domain != "" || org != "" {
		t.Errorf("unknown returned (%q, %q); want (\"\", \"\")", domain, org)
	}
}

// TestKnownIssuers_GStrkeyShape — every key is a 56-char G-strkey
// starting with 'G'. Catches pasted whitespace / partial copies.
func TestKnownIssuers_GStrkeyShape(t *testing.T) {
	if len(knownIssuers) == 0 {
		t.Fatal("knownIssuers is empty — bootstrap entries (Circle USDC etc.) missing")
	}
	for g := range knownIssuers {
		if len(g) != 56 {
			t.Errorf("knownIssuers key %q: length %d, want 56", g, len(g))
		}
		if !strings.HasPrefix(g, "G") {
			t.Errorf("knownIssuers key %q: must start with 'G'", g)
		}
	}
}

// TestKnownIssuers_AllFieldsPopulated — every entry has BOTH
// home_domain and org_name. A half-populated entry is a curation
// bug — the whole point is to fill in for the explorer.
func TestKnownIssuers_AllFieldsPopulated(t *testing.T) {
	for g, entry := range knownIssuers {
		if entry.HomeDomain == "" {
			t.Errorf("knownIssuers[%s]: HomeDomain is empty", g)
		}
		if entry.OrgName == "" {
			t.Errorf("knownIssuers[%s]: OrgName is empty", g)
		}
	}
}

// erroringAccountStateReader fails every AccountStateCached call, the
// same shape as a store error or a context deadline reaching the
// explorer seam.
type erroringAccountStateReader struct{ ExplorerReader }

func (erroringAccountStateReader) AccountStateCached(
	context.Context, string,
) (clickhouse.AccountState, bool, error) {
	return clickhouse.AccountState{}, false, context.DeadlineExceeded
}

// TestBackfillHomeDomain_ReadFailureDoesNotFallBackToStaticMap: an on-chain read failure is not a verified absence. Falling
// back to the hand-maintained knownIssuers map would serve a possibly stale domain as live-confirmed, so HomeDomain stays nil
// and the caller is told the read degraded.
func TestBackfillHomeDomain_ReadFailureDoesNotFallBackToStaticMap(t *testing.T) {
	usdc := "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	s := &Server{Options: Options{Explorer: erroringAccountStateReader{}}}
	issuer := usdc
	detail := &AssetDetail{Issuer: &issuer}

	degraded := s.backfillHomeDomain(context.Background(), detail)

	if !degraded {
		t.Fatal("backfillHomeDomain reported degraded=false on an explorer read error; want true")
	}
	if detail.HomeDomain != nil {
		t.Errorf("HomeDomain = %q; want nil — a failed read must not fall back to the curated map (would have served circle.com)",
			*detail.HomeDomain)
	}
}

// TestKnownIssuers_NoOverlapWithScams: a G-strkey both "known legitimate" and "known scam" is a curation contradiction that
// should not exist in the source, even though the scam path wins in the API.
func TestKnownIssuers_NoOverlapWithScams(t *testing.T) {
	for g := range knownIssuers {
		if _, scam := scamIssuers[g]; scam {
			t.Errorf("G-strkey %s is in BOTH knownIssuers AND scamIssuers — pick one", g)
		}
	}
}
