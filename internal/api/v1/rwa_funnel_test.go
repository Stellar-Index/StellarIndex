package v1_test

// GET /v1/rwa/assets — the funnel (#352).
//
// The surface used to narrow a population of tens of thousands of SEP-1
// attestations down to single digits and publish a refusal tally of
// three, with no way to tell a network that holds six real-world assets
// from a pipeline discarding everything else in silence. Measured on
// production 2026-09-10: 6 assets served, 3 refusals reported, against
// 44,376 issuer accounts with a home_domain and 14,635 with a fetched
// SEP-1 payload.
//
// Every test below pins one stage of the accounting. They exist as much
// to stop a future drop going unreported as to check today's numbers:
// a `continue` with no counter beside it is how the funnel got here.

import (
	"errors"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// errRWAScan stands in for the attestation read failing.
var errRWAScan = errors.New("issuers scan unavailable")

// rwaFunnelStages indexes a served funnel by stage name.
func rwaFunnelStages(t *testing.T, v v1.RWAAssetsView) map[string]v1.RWAFunnelStage {
	t.Helper()
	out := map[string]v1.RWAFunnelStage{}
	for _, s := range v.Funnel.Stages {
		if _, dup := out[s.Stage]; dup {
			t.Fatalf("stage %q served twice — a funnel with a repeated stage cannot be reconciled", s.Stage)
		}
		out[s.Stage] = s
	}
	return out
}

// rwaDropCount reads one drop off one stage.
func rwaDropCount(stages map[string]v1.RWAFunnelStage, stage, reason string) int {
	for _, d := range stages[stage].Dropped {
		if d.Reason == reason {
			return d.Count
		}
	}
	return 0
}

// checkFunnelArithmetic re-derives the narrowing INDEPENDENTLY of the
// production balance check, so `balanced: true` is never self-certifying.
// Adjacent stages counted in the same unit must satisfy
// count - sum(dropped) == next.count.
func checkFunnelArithmetic(t *testing.T, v v1.RWAAssetsView) {
	t.Helper()
	st := v.Funnel.Stages
	for i := 0; i+1 < len(st); i++ {
		cur, next := st[i], st[i+1]
		// The one place a subtraction means nothing: issuer accounts to
		// the declarations they publish. That pair must carry no drops.
		// Every other unit change is a relabelling of a population that
		// maps one-to-one, so it still has to reconcile.
		if cur.Unit == "issuer_accounts" && next.Unit != "issuer_accounts" {
			if len(cur.Dropped) > 0 {
				t.Errorf("stage %q is the last counted in issuer accounts and drops %d reasons — "+
					"a drop across that change of unit cannot be reconciled", cur.Stage, len(cur.Dropped))
			}
			continue
		}
		sum := 0
		for _, d := range cur.Dropped {
			// issuer_asset_page_truncated counts ISSUERS whose asset
			// tail went unread, not assets; it is a signal, not a term
			// in the asset arithmetic.
			if d.Reason == "issuer_asset_page_truncated" {
				continue
			}
			if d.Actor == "" {
				t.Errorf("stage %q drop %q carries no actor — a reader cannot tell a coverage gap from a refusal",
					cur.Stage, d.Reason)
			}
			sum += d.Count
		}
		if cur.Count-sum != next.Count {
			t.Errorf("funnel does not close: %s %d less %d dropped is %d, but %s is %d",
				cur.Stage, cur.Count, sum, cur.Count-sum, next.Stage, next.Count)
		}
	}
	if !v.Funnel.Balanced {
		t.Errorf("funnel.balanced = false for an accounting that adds up: %+v", v.Funnel.Stages)
	}
	if v.Funnel.Basis == "" {
		t.Error("funnel.basis is empty — the units change down the funnel and nothing else says so")
	}
}

// rwaServerWithUpstream is rwaServer with the population UPSTREAM of the
// bound entries stated: the issuer rows and declarations a real
// deployment holds, which no fixture of bound entries can express.
func rwaServerWithUpstream(
	t *testing.T,
	upstream timescale.Sep1BoundCensus,
	bound []timescale.Sep1BoundCurrency,
	dir map[string]timescale.DirectoryEntry,
	rows map[string][]timescale.AssetRow,
) *v1.Server {
	t.Helper()
	return v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{bound: bound, upstream: upstream},
		Directory: &stubDirectoryReader{entries: dir},
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer:            rows,
			supply:              rwaSupplyFor(rows),
		},
	})
}

// TestRWAAssets_FunnelAccountsForTheWholePopulation is the headline
// regression. A response that serves one asset must state the size of
// the population it narrowed from and where the rest went — every
// stage, every drop, with the arithmetic closing.
func TestRWAAssets_FunnelAccountsForTheWholePopulation(t *testing.T) {
	// The shape of the production deployment in miniature: most issuer
	// accounts have never had their toml fetched, a few payloads will
	// not decode, a few declare nothing, and of the declarations that
	// exist the overwhelming majority name somebody else's account.
	// The fixture materialises only the three survivors, so the
	// pre-filter drop absorbs the rest of the bound population.
	upstream := timescale.Sep1BoundCensus{
		IssuersWithHomeDomain:      44376,
		IssuersWithPayload:         14635,
		IssuersPayloadUnreadable:   41,
		IssuersDeclaringNothing:    2109,
		IssuersDeclaring:           12485,
		Entries:                    1182000,
		EntriesMissingCode:         3140,
		EntriesMissingIssuer:       9612,
		EntriesNamingAnotherIssuer: 1145346,
		EntriesBound:               23902,
		EntriesFiltered:            23902,
	}
	bound := []timescale.Sep1BoundCurrency{
		rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
		rwaBound("BENJI", rwaUnknownIssuer, "franklintempleton.reallumens.com", "bond"),
		rwaBound("USTRY", rwaScamIssuer, "stellar.us.org", "bond"),
	}
	dir := map[string]timescale.DirectoryEntry{
		rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse"),
		rwaScamIssuer: {
			Address: rwaScamIssuer, Tags: []string{"issuer", "malicious"}, Source: "stellar-expert",
		},
	}
	rows := map[string][]timescale.AssetRow{
		rwaGoodIssuer:    {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)},
		rwaScamIssuer:    {rwaRow("USTRY", rwaScamIssuer, sptr("1.0412"), 13705)},
		rwaUnknownIssuer: {rwaRow("BENJI", rwaUnknownIssuer, sptr("1.1408"), 8299)},
	}

	v := getRWA(t, rwaServerWithUpstream(t, upstream, bound, dir, rows))
	if len(v.Funnel.Stages) == 0 {
		t.Fatal("no funnel served: the response narrows a population and must account for it")
	}
	checkFunnelArithmetic(t, v)

	st := rwaFunnelStages(t, v)
	// The population the surface narrows from, which nothing on the
	// response used to state at all.
	if got := st["issuers_with_home_domain"].Count; got != 44376 {
		t.Errorf("issuers_with_home_domain = %d, want 44376", got)
	}
	if got := st["issuers_with_sep1_attestation"].Count; got != 14635 {
		t.Errorf("issuers_with_sep1_attestation = %d, want 14635", got)
	}
	// 44,376 - 14,635 = 29,741 issuer accounts whose toml has never
	// been fetched. This is the largest single coverage lever on the
	// surface and it belongs to an operator, not to the definition.
	if got := rwaDropCount(st, "issuers_with_home_domain", "sep1_attestation_never_fetched"); got != 29741 {
		t.Errorf("sep1_attestation_never_fetched = %d, want 29741", got)
	}
	for _, d := range st["issuers_with_home_domain"].Dropped {
		if d.Reason == "sep1_attestation_never_fetched" && d.Actor != "operator" {
			t.Errorf("never-fetched drop actor = %q, want operator — it is a fetch nobody ran, not a refusal", d.Actor)
		}
	}
	// The two silent stages.
	if got := rwaDropCount(st, "issuers_with_sep1_attestation", "sep1_payload_unreadable"); got != 41 {
		t.Errorf("sep1_payload_unreadable = %d, want 41 — the swallowed-parse-error bucket", got)
	}
	if got := rwaDropCount(st, "issuers_with_sep1_attestation", "sep1_declares_no_currencies"); got != 2109 {
		t.Errorf("sep1_declares_no_currencies = %d, want 2109", got)
	}
	// The provenance rule, which is the definition working and not a gap.
	if got := rwaDropCount(st, "sep1_currency_entries", "entry_declares_another_issuer"); got != 1145346 {
		t.Errorf("entry_declares_another_issuer = %d, want 1145346", got)
	}
	// Requirement 4's pre-filter, its own stage because it runs before
	// requirement 3 is evaluated for those entries.
	if got := rwaDropCount(st, "issuer_bound_entries", "no_real_world_instrument_basis"); got != 23902 {
		t.Errorf("pre-filter drop = %d, want 23902", got)
	}
	// Three candidates reached the ordered evaluation: one admitted,
	// one scam-flagged, one unrecognised.
	if got := st["candidate_assets_evaluated"].Count; got != 3 {
		t.Errorf("candidate_assets_evaluated = %d, want 3", got)
	}
	if got := rwaDropCount(st, "candidate_assets_evaluated", "issuer_scam_flagged"); got != 1 {
		t.Errorf("issuer_scam_flagged = %d, want 1", got)
	}
	if got := rwaDropCount(st, "candidate_assets_evaluated", "issuer_not_independently_recognised"); got != 1 {
		t.Errorf("issuer_not_independently_recognised = %d, want 1", got)
	}
	if got := st["assets_served"].Count; got != 1 || len(v.Assets) != 1 {
		t.Errorf("assets_served = %d with %d rows, want 1/1", got, len(v.Assets))
	}
}

// TestRWAAssets_FunnelSeparatesNeverFetchedFromDeclaresNothing pins the
// distinction the surface most needs and least had. An issuer whose
// stellar.toml nobody has fetched, one whose payload will not decode,
// and one that decoded and declares nothing are three different
// findings with three different owners. Collapsed into one silent
// `continue`, they are indistinguishable — and the first of the three
// is the one an operator can fix today.
func TestRWAAssets_FunnelSeparatesNeverFetchedFromDeclaresNothing(t *testing.T) {
	upstream := timescale.Sep1BoundCensus{
		IssuersWithHomeDomain:    10,
		IssuersWithPayload:       4,
		IssuersPayloadUnreadable: 1,
		IssuersDeclaringNothing:  2,
		IssuersDeclaring:         1,
	}
	v := getRWA(t, rwaServerWithUpstream(t, upstream,
		[]timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 3)}},
	))
	checkFunnelArithmetic(t, v)

	st := rwaFunnelStages(t, v)
	byOwner := map[string]int{
		"never_fetched":    rwaDropCount(st, "issuers_with_home_domain", "sep1_attestation_never_fetched"),
		"unreadable":       rwaDropCount(st, "issuers_with_sep1_attestation", "sep1_payload_unreadable"),
		"declares_nothing": rwaDropCount(st, "issuers_with_sep1_attestation", "sep1_declares_no_currencies"),
	}
	want := map[string]int{"never_fetched": 6, "unreadable": 1, "declares_nothing": 2}
	for k, w := range want {
		if byOwner[k] != w {
			t.Errorf("%s = %d, want %d — the three fates must stay apart (%+v)", k, byOwner[k], w, byOwner)
		}
	}
	if got := st["issuers_declaring_currencies"].Count; got != 1 {
		t.Errorf("issuers_declaring_currencies = %d, want 1", got)
	}
}

// TestRWAAssets_DuplicateDeclarationIsServedOnceAndCounted — SEP-1 does
// not forbid a toml declaring the same asset twice, and identity here is
// (code, issuer), so the second declaration is the SAME asset. Serving
// it twice would put its market cap into the summary, the class total
// and the issuer total twice each.
func TestRWAAssets_DuplicateDeclarationIsServedOnceAndCounted(t *testing.T) {
	bound := []timescale.Sep1BoundCurrency{
		rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
		rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
	}
	v := getRWA(t, rwaServer(t, bound,
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)}},
	))
	if len(v.Assets) != 1 {
		t.Fatalf("assets = %v, want ONE row — (code, issuer) is the identity, so the second declaration "+
			"is the same asset and serving it twice doubles its market cap in every total", rwaAssetIDs(v))
	}
	if v.Summary.Assets != 1 {
		t.Errorf("summary.assets = %d, want 1", v.Summary.Assets)
	}
	// The exact total, not an approximation: 12336218000000 stroops of
	// supply at 1.0412 is 1284507193.16 dollars, once.
	if v.Summary.MarketCapUSD == nil {
		t.Fatal("summary.market_cap_usd absent, want the single asset's cap")
	}
	single := *v.Summary.MarketCapUSD
	for _, g := range v.ByClass {
		if g.Assets != 1 {
			t.Errorf("by_class[%s].assets = %d, want 1", g.Class, g.Assets)
		}
		if g.MarketCapUSD == nil || *g.MarketCapUSD != single {
			t.Errorf("by_class[%s].market_cap_usd = %v, want the same %q the summary carries",
				g.Class, g.MarketCapUSD, single)
		}
	}
	for _, i := range v.ByIssuer {
		if i.Assets != 1 {
			t.Errorf("by_issuer[%s].assets = %d, want 1", i.Issuer, i.Assets)
		}
	}
	st := rwaFunnelStages(t, v)
	if got := rwaDropCount(st, "candidate_assets_evaluated", "duplicate_declaration_of_the_same_asset"); got != 1 {
		t.Errorf("duplicate drop = %d, want 1 — a deduplicated candidate is still a candidate that entered", got)
	}
	checkFunnelArithmetic(t, v)
}

// TestRWAAssets_AdmittedButNeverObservedIsCounted — an asset can meet
// every requirement and still have no row in the catalogue, because the
// index has never seen it on chain. It is correctly absent from the set;
// it was NOT correct for it to vanish without a count, which made
// "admitted" and "served" silently different numbers.
func TestRWAAssets_AdmittedButNeverObservedIsCounted(t *testing.T) {
	bound := []timescale.Sep1BoundCurrency{
		rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
		rwaBound("NEVERSEEN", rwaGoodIssuer, "etherfuse.com", "bond"),
	}
	v := getRWA(t, rwaServer(t, bound,
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		// The catalogue holds USTRY only.
		map[string][]timescale.AssetRow{rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)}},
	))
	if len(v.Assets) != 1 {
		t.Fatalf("assets = %v, want only the observed one", rwaAssetIDs(v))
	}
	st := rwaFunnelStages(t, v)
	if got := st["assets_admitted"].Count; got != 2 {
		t.Errorf("assets_admitted = %d, want 2 — both met the definition", got)
	}
	if got := rwaDropCount(st, "assets_admitted", "admitted_but_never_observed_on_chain"); got != 1 {
		t.Errorf("never-observed drop = %d, want 1 — admitted and served were silently different numbers", got)
	}
	checkFunnelArithmetic(t, v)
}

// TestRWAAssets_IssuerAssetPageTruncationIsReported — the per-issuer
// listing read is capped, and the cap has always been DOCUMENTED as
// reported ("the cap is reported the same way the issuer cap is") while
// nothing reported it. An issuer with more classic assets than one page
// has its tail unread, so a member in that tail disappears from the set
// with nothing to show for it.
//
// The count is issuers-with-an-unread-tail, not assets, so it is served
// as its own signal and kept out of the asset arithmetic. Counting it
// as assets would make the funnel close by inventing a number.
func TestRWAAssets_IssuerAssetPageTruncationIsReported(t *testing.T) {
	const page = 500
	rows := make([]timescale.AssetRow, 0, page)
	rows = append(rows, rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312))
	for i := 1; i < page; i++ {
		rows = append(rows, rwaRow("FILLER"+string(rune('A'+i%26))+itoaRWA(i), rwaGoodIssuer, sptr("0.01"), int64(i)))
	}
	v := getRWA(t, rwaServer(t,
		[]timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{rwaGoodIssuer: rows},
	))
	st := rwaFunnelStages(t, v)
	if got := rwaDropCount(st, "assets_admitted", "issuer_asset_page_truncated"); got != 1 {
		t.Errorf("issuer_asset_page_truncated = %d, want 1 — a full page means an unread tail, and a member "+
			"in it would vanish from the set unreported", got)
	}
	// The signal must not be allowed into the asset subtraction.
	checkFunnelArithmetic(t, v)
}

// itoaRWA is a tiny int-to-string so the filler codes above stay
// distinct without pulling strconv into the fixture's reading.
func itoaRWA(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestRWAAssets_FunnelIsNotServedAsZerosWhenUnmeasured — when the
// attestation read fails there is no population to report. Publishing a
// funnel of zeros would read as a measured network with nothing in it,
// which is the same lie as a market cap of "0.00".
func TestRWAAssets_FunnelIsNotServedAsZerosWhenUnmeasured(t *testing.T) {
	srv := v1.New(v1.Options{
		Sep1Cache:    &stubSep1BoundReader{err: errRWAScan},
		Directory:    &stubDirectoryReader{},
		AssetsReader: &rwaListStub{stubAssetsReaderExt: &stubAssetsReaderExt{}},
	})
	v := getRWA(t, srv)
	if len(v.Funnel.Stages) != 0 {
		t.Errorf("funnel served %d stages for an unmeasured population: %+v", len(v.Funnel.Stages), v.Funnel.Stages)
	}
	if v.Funnel.Balanced {
		t.Error("funnel.balanced = true for a population that was never walked")
	}
	if v.Funnel.Basis == "" {
		t.Error("funnel.basis is empty — the absence has to be stated, not implied by empty stages")
	}
}

// TestRWAAssets_FunnelSaysUnbalancedWhenTheCensusDoesNot — an
// accounting that cannot be reconciled must SAY so on the wire. A
// reader silently failing to make the numbers meet is the outcome the
// whole structure exists to prevent, and it is worse than publishing no
// numbers at all.
//
// The fixture is a census that cannot describe any deployment: more
// issuers carrying a fetched payload than carrying a home_domain, when
// the payload is fetched FROM the home_domain.
func TestRWAAssets_FunnelSaysUnbalancedWhenTheCensusDoesNot(t *testing.T) {
	upstream := timescale.Sep1BoundCensus{
		// Fewer issuers with a home_domain than with a payload, which
		// cannot happen: a payload is fetched FROM a home_domain.
		IssuersWithHomeDomain:   1,
		IssuersWithPayload:      9,
		IssuersDeclaringNothing: 8,
		IssuersDeclaring:        1,
	}
	v := getRWA(t, rwaServerWithUpstream(t, upstream,
		[]timescale.Sep1BoundCurrency{rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond")},
		map[string]timescale.DirectoryEntry{rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse")},
		map[string][]timescale.AssetRow{rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 3)}},
	))
	if v.Funnel.Balanced {
		t.Errorf("funnel.balanced = true over a census that cannot be reconciled: %+v", v.Funnel.Stages)
	}
}

// TestRWAAssets_RefusalTallyStaysConsistentWithTheFunnel — the two views
// answer different questions (an ordered requirement tally versus the
// whole narrowing) and must never disagree about the requirement
// refusals they both report.
func TestRWAAssets_RefusalTallyStaysConsistentWithTheFunnel(t *testing.T) {
	bound := []timescale.Sep1BoundCurrency{
		rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
		rwaBound("USTRY", rwaScamIssuer, "stellar.us.org", "bond"),
		rwaBound("BENJI", rwaUnknownIssuer, "franklintempleton.reallumens.com", "bond"),
		rwaBound("MEME", rwaGoodIssuer, "etherfuse.com", "crypto"),
	}
	dir := map[string]timescale.DirectoryEntry{
		rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse"),
		rwaScamIssuer: {Address: rwaScamIssuer, Tags: []string{"issuer", "malicious"}, Source: "stellar-expert"},
	}
	rows := map[string][]timescale.AssetRow{
		rwaGoodIssuer: {
			rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312),
			rwaRow("MEME", rwaGoodIssuer, sptr("0.02"), 12),
		},
		rwaScamIssuer:    {rwaRow("USTRY", rwaScamIssuer, sptr("1.0412"), 13705)},
		rwaUnknownIssuer: {rwaRow("BENJI", rwaUnknownIssuer, sptr("1.1408"), 8299)},
	}
	v := getRWA(t, rwaServer(t, bound, dir, rows))
	checkFunnelArithmetic(t, v)

	refused := map[string]int{}
	for _, r := range v.Refused {
		refused[r.Reason] = r.Assets
	}
	st := rwaFunnelStages(t, v)
	for _, reason := range []string{"issuer_scam_flagged", "issuer_not_independently_recognised"} {
		if got, want := rwaDropCount(st, "candidate_assets_evaluated", reason), refused[reason]; got != want {
			t.Errorf("%s: funnel says %d, refused[] says %d — the two views must never disagree",
				reason, got, want)
		}
	}
	// The requirement-4 pre-filter appears in BOTH views on purpose: as
	// its own funnel stage (where it happened) and in refused[] under
	// requirement 4 (which requirement it is). The funnel must not
	// double-count it into the evaluated stage.
	if got := rwaDropCount(st, "issuer_bound_entries", "no_real_world_instrument_basis"); got != 1 {
		t.Errorf("pre-filter stage = %d, want 1 (the crypto-anchored token)", got)
	}
	if got := rwaDropCount(st, "candidate_assets_evaluated", "no_real_world_instrument_basis"); got != 0 {
		t.Errorf("evaluated stage also counts %d under requirement 4 — the pre-filter drop is counted twice "+
			"and the funnel cannot close", got)
	}
	if refused["no_real_world_instrument_basis"] != 1 {
		t.Errorf("refused[no_real_world_instrument_basis] = %d, want 1",
			refused["no_real_world_instrument_basis"])
	}
}
