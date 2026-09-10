package v1_test

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// GET /v1/rwa/assets — the CONTRACT arm (#352).
//
// The arm exists because the entities that actually hold real-world
// assets on Stellar are invisible to the classic one. Measured on r1
// 2026-09-10: Franklin Templeton and Spiko are both in the curated
// directory, tagged `issuer`, with the correct domains — and both are
// absent from the `issuers` table, so `sep1-refresh -issuer` returns
// `sql: no rows in result set` for each. That table is written from ONE
// site, the classic-asset registration path, so an entity whose Stellar
// presence is contract-issued is never collected at all.
//
// So these tests cover two things a refusal tally cannot: that a
// contract the definition admits is served AND valued, and that an
// entity nothing can reach is REPORTED rather than silently absent.

// Valid C-strkeys used as fixtures. They stand for nothing on the
// network.
const (
	rwaContractGood = "CAAQEAYEAUDAOCAJBIFQYDIOB4IBCEQTCQKRMFYYDENBWHA5DYPSBFLM"
	rwaContractPool = "CABAIBQIBIGA4EASCQLBQGQ4DYQCEJBGFAVCYLRQGI2DMOB2HQ7EA4R4"
)

// stubRWAContractReader serves canned curated-directory scans. It
// censuses what it serves the way the real query does, so a test
// asserting on the funnel asserts against an accounting that closes
// rather than against numbers a stub invented.
type stubRWAContractReader struct {
	contracts []timescale.DirectoryEntry
	unreached []timescale.DirectoryEntry
	// accounts is the number of ACCOUNT rows in the curated set —
	// the population the contract arm's first stage drops.
	accounts int
	// scamContracts and untaggedContracts are contract rows the scan
	// excluded, counted but never returned.
	scamContracts     int
	untaggedContracts int
	err               error
}

func (s *stubRWAContractReader) DirectoryRecognisedContracts(
	context.Context, []string,
) ([]timescale.DirectoryEntry, timescale.DirectoryRWACensus, error) {
	if s.err != nil {
		return nil, timescale.DirectoryRWACensus{}, s.err
	}
	c := timescale.DirectoryRWACensus{
		Accounts:                    s.accounts,
		Contracts:                   len(s.contracts) + s.scamContracts + s.untaggedContracts,
		ContractsScamFlagged:        s.scamContracts,
		ContractsWithoutIssuingTag:  s.untaggedContracts,
		ContractsRecognised:         len(s.contracts),
		AccountsIssuingTagged:       len(s.unreached),
		AccountsIssuingWithoutAsset: len(s.unreached),
	}
	c.Entries = c.Accounts + c.Contracts
	return s.contracts, c, nil
}

func (s *stubRWAContractReader) DirectoryRecognisedIssuersWithoutAsset(
	context.Context, []string, int,
) ([]timescale.DirectoryEntry, error) {
	return s.unreached, nil
}

// stubContractCatalogue answers the volume-gate-free catalogue read.
type stubContractCatalogue struct {
	rows map[string]timescale.AssetRow
}

func (s *stubContractCatalogue) ContractCatalogueRows(
	_ context.Context, ids []string,
) (map[string]timescale.AssetRow, error) {
	out := map[string]timescale.AssetRow{}
	for _, id := range ids {
		if r, ok := s.rows[id]; ok {
			out[id] = r
		}
	}
	return out, nil
}

// stubTokenSymbols / stubTokenSupplies / stubTokenDecimalsRdr are the
// three lake reads the contract arm makes per candidate.
type stubTokenSymbols struct{ byID map[string]string }

func (s *stubTokenSymbols) TokenSymbol(_ context.Context, id string) (string, bool, error) {
	v, ok := s.byID[id]
	return v, ok, nil
}

type stubTokenSupplies struct{ byID map[string]string }

func (s *stubTokenSupplies) TokenSupply(_ context.Context, id string) (clickhouse.TokenSupply, error) {
	raw, ok := s.byID[id]
	if !ok {
		return clickhouse.TokenSupply{ContractID: id}, nil
	}
	n, _ := new(big.Int).SetString(raw, 10)
	return clickhouse.TokenSupply{ContractID: id, Total: n, Incomplete: n.Sign() < 0}, nil
}

// NativeTotalCoins satisfies the rest of the TokenSupplyReader seam. The
// contract arm never calls it — XLM is not a real-world asset — so a
// test reaching it is a bug in the arm rather than in the stub.
func (s *stubTokenSupplies) NativeTotalCoins(context.Context) (int64, uint32, error) {
	return 0, 0, errors.New("the RWA contract arm must not read the native total supply")
}

type stubTokenDecimalsRdr struct{ byID map[string]uint32 }

func (s *stubTokenDecimalsRdr) TokenDecimals(_ context.Context, id string) (uint32, bool, error) {
	v, ok := s.byID[id]
	return v, ok, nil
}

// rwaContractRow builds a catalogue row for a contract: no code, no
// issuer, a price and a volume comfortably above the dust floor so a
// suppressed cap is always the condition under test.
func rwaContractRow(id string, price *string) timescale.AssetRow {
	return timescale.AssetRow{
		AssetID:          id,
		Slug:             id,
		FirstSeenLedger:  55008233,
		LastSeenLedger:   63410221,
		ObservationCount: 4211,
		PriceUSD:         price,
		Volume24hUSD:     sptr("8214.55"),
		SourceCount:      iptr(3),
	}
}

func iptr(i int) *int { return &i }

func recognisedContract(addr, name string, tags ...string) timescale.DirectoryEntry {
	if len(tags) == 0 {
		tags = []string{"issuer"}
	}
	return timescale.DirectoryEntry{
		Address: addr, Name: name, Domain: "example-fund.com",
		Tags: tags, Source: "stellar-expert",
	}
}

// rwaContractServer wires the classic arm empty and the contract arm
// fully, so anything served is the contract arm's doing.
func rwaContractServer(
	t *testing.T,
	contracts *stubRWAContractReader,
	rows map[string]timescale.AssetRow,
	symbols map[string]string,
	supplies map[string]string,
	decimals map[string]uint32,
	dir map[string]timescale.DirectoryEntry,
) *v1.Server {
	t.Helper()
	return v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{},
		Directory: &stubDirectoryReader{entries: dir},
		AssetsReader: &rwaListStub{
			stubAssetsReaderExt: &stubAssetsReaderExt{},
			byIssuer:            map[string][]timescale.AssetRow{},
		},
		RWAContracts:      contracts,
		ContractCatalogue: &stubContractCatalogue{rows: rows},
		TokenSymbol:       &stubTokenSymbols{byID: symbols},
		TokenSupply:       &stubTokenSupplies{byID: supplies},
		TokenDecimals:     &stubTokenDecimalsRdr{byID: decimals},
		// A real floor, so the dust guard is live rather than disabled
		// by a zero that would make every cap publish unconditionally.
		MinMarketCapVolumeUSD: 1000,
	})
}

// TestRWAContracts_AdmitsAndValuesARecognisedContract is the positive
// path end to end: the curated directory names the exact contract with
// an issuing tag, an oracle prices an instrument of its on-chain symbol,
// and the row is served WITH a market cap derived from lake supply and
// the served price.
func TestRWAContracts_AdmitsAndValuesARecognisedContract(t *testing.T) {
	view := getRWA(t, rwaContractServer(t,
		&stubRWAContractReader{
			contracts: []timescale.DirectoryEntry{recognisedContract(rwaContractGood, "Example Treasury Fund")},
			accounts:  18000,
		},
		map[string]timescale.AssetRow{rwaContractGood: rwaContractRow(rwaContractGood, sptr("1.0740000000"))},
		map[string]string{rwaContractGood: "USTRY"},
		// 1,000,000 tokens at 6 decimals.
		map[string]string{rwaContractGood: "1000000000000"},
		map[string]uint32{rwaContractGood: 6},
		nil,
	))

	if len(view.Assets) != 1 {
		t.Fatalf("assets = %d, want 1: %+v", len(view.Assets), view.Assets)
	}
	a := view.Assets[0]
	if a.ContractID != rwaContractGood {
		t.Errorf("contract_id = %q, want %q", a.ContractID, rwaContractGood)
	}
	// Identity is the contract address and nothing else. A code or an
	// issuer here would mean a self-declared string had been promoted
	// into the field this surface identifies assets by.
	if a.Code != "" || a.Issuer != "" {
		t.Errorf("contract row carries code=%q issuer=%q, want both empty", a.Code, a.Issuer)
	}
	if a.Symbol != "USTRY" {
		t.Errorf("symbol = %q, want USTRY", a.Symbol)
	}
	if a.Basis != "contract_oracle_rwa_feed" {
		t.Errorf("basis = %q", a.Basis)
	}
	if a.Valuation.Status != v1.RWAValuationPublished {
		t.Fatalf("valuation status = %q, want published (%+v)", a.Valuation.Status, a.Valuation)
	}
	// 1,000,000 × 1.074 = 1,074,000.00. Decimals are load-bearing: at
	// the hardcoded 7 the catalogue row carries, this would publish
	// 107,400.00 — a tenth of the real figure.
	if got := deref(a.Valuation.MarketCapUSD); got != "1074000.00" {
		t.Errorf("market_cap_usd = %q, want 1074000.00", got)
	}
	if got := deref(view.Summary.MarketCapUSD); got != "1074000.00" {
		t.Errorf("summary market_cap_usd = %q, want 1074000.00", got)
	}
	if view.Summary.LowerBound {
		t.Error("lower_bound is true with every member valued")
	}
}

// TestRWAContracts_DecimalsDriveTheValuation isolates the decimals
// dependency. The catalogue row hardcodes 7 for every asset; a token
// declaring 6 must be valued at 6.
func TestRWAContracts_DecimalsDriveTheValuation(t *testing.T) {
	for _, tc := range []struct {
		decimals uint32
		want     string
	}{
		{6, "1074000.00"},
		{7, "107400.00"},
		{18, "0.00"},
	} {
		view := getRWA(t, rwaContractServer(t,
			&stubRWAContractReader{contracts: []timescale.DirectoryEntry{
				recognisedContract(rwaContractGood, "Example Treasury Fund"),
			}},
			map[string]timescale.AssetRow{rwaContractGood: rwaContractRow(rwaContractGood, sptr("1.0740000000"))},
			map[string]string{rwaContractGood: "USTRY"},
			map[string]string{rwaContractGood: "1000000000000"},
			map[string]uint32{rwaContractGood: tc.decimals},
			nil,
		))
		if len(view.Assets) != 1 {
			t.Fatalf("decimals %d: assets = %d, want 1", tc.decimals, len(view.Assets))
		}
		if got := deref(view.Assets[0].Valuation.MarketCapUSD); got != tc.want {
			t.Errorf("decimals %d: market_cap_usd = %q, want %q", tc.decimals, got, tc.want)
		}
	}
}

// TestRWAContracts_IncompleteSupplyIsUnavailableNeverZero pins the
// refusal the lake reader's Incomplete flag exists for. A negative net
// means the flows are incompletely seeded, NOT that supply is negative,
// and clamping it to zero would read as a fully-burned token.
func TestRWAContracts_IncompleteSupplyIsUnavailableNeverZero(t *testing.T) {
	view := getRWA(t, rwaContractServer(t,
		&stubRWAContractReader{contracts: []timescale.DirectoryEntry{
			recognisedContract(rwaContractGood, "Example Treasury Fund"),
		}},
		map[string]timescale.AssetRow{rwaContractGood: rwaContractRow(rwaContractGood, sptr("1.0740000000"))},
		map[string]string{rwaContractGood: "USTRY"},
		map[string]string{rwaContractGood: "-500"},
		map[string]uint32{rwaContractGood: 6},
		nil,
	))
	if len(view.Assets) != 1 {
		t.Fatalf("assets = %d, want 1 — an unvaluable member must stay in the set", len(view.Assets))
	}
	a := view.Assets[0]
	if a.Valuation.MarketCapUSD != nil {
		t.Errorf("market_cap_usd = %q on an incomplete supply, want absent", *a.Valuation.MarketCapUSD)
	}
	if a.CirculatingSupply != nil {
		t.Errorf("circulating_supply = %q on an incomplete supply, want absent", *a.CirculatingSupply)
	}
	if a.Valuation.Status != v1.RWAValuationNoSupply {
		t.Errorf("status = %q, want %q", a.Valuation.Status, v1.RWAValuationNoSupply)
	}
	// The set is larger than the total, and the summary has to say so.
	if !view.Summary.LowerBound {
		t.Error("lower_bound is false while a member is unvalued")
	}
	if view.Summary.AssetsUnvalued != 1 {
		t.Errorf("assets_unvalued = %d, want 1", view.Summary.AssetsUnvalued)
	}
	if view.Summary.MarketCapUSD != nil {
		t.Errorf("summary market_cap_usd = %q, want ABSENT rather than a zero total", *view.Summary.MarketCapUSD)
	}
}

// TestRWAContracts_ScamFlagAcquiredAfterAdmissionWithholdsTheValuation
// pins the gap this arm closes.
//
// fillIssuerDirectoryTags keys on the issuer G-address and skips every
// row without one, so a contract asset has never been subject to the
// directory scam gate on ANY surface. On a page that admits contracts on
// the strength of a directory entry, declining to re-read that entry
// when it turns hostile would be indefensible.
func TestRWAContracts_ScamFlagAcquiredAfterAdmissionWithholdsTheValuation(t *testing.T) {
	view := getRWA(t, rwaContractServer(t,
		&stubRWAContractReader{contracts: []timescale.DirectoryEntry{
			recognisedContract(rwaContractGood, "Example Treasury Fund"),
		}},
		map[string]timescale.AssetRow{rwaContractGood: rwaContractRow(rwaContractGood, sptr("1.0740000000"))},
		map[string]string{rwaContractGood: "USTRY"},
		map[string]string{rwaContractGood: "1000000000000"},
		map[string]uint32{rwaContractGood: 6},
		// The valuation-time directory read now flags it.
		map[string]timescale.DirectoryEntry{
			rwaContractGood: {
				Address: rwaContractGood, Name: "Example Treasury Fund",
				Tags: []string{"issuer", "malicious"}, Source: "stellar-expert",
			},
		},
	))
	if len(view.Assets) != 1 {
		t.Fatalf("assets = %d, want 1 — this surface hides nothing it admitted", len(view.Assets))
	}
	a := view.Assets[0]
	if a.Valuation.Status != v1.RWAValuationIssuerFlagged {
		t.Fatalf("status = %q, want %q", a.Valuation.Status, v1.RWAValuationIssuerFlagged)
	}
	if a.Valuation.MarketCapUSD != nil || a.Valuation.PriceUSD != nil {
		t.Errorf("flagged contract still publishes price=%v cap=%v", a.Valuation.PriceUSD, a.Valuation.MarketCapUSD)
	}
	if view.Summary.MarketCapUSD != nil {
		t.Errorf("summary total = %q, want absent — the only member is withheld", *view.Summary.MarketCapUSD)
	}
}

// TestRWAContracts_RefusesAPoolContract is the arm's own safety case at
// the HTTP boundary rather than in the definition unit test: a contract
// the directory names `defi` is not served, and the refusal is counted.
func TestRWAContracts_RefusesAPoolContract(t *testing.T) {
	view := getRWA(t, rwaContractServer(t,
		&stubRWAContractReader{contracts: []timescale.DirectoryEntry{
			recognisedContract(rwaContractGood, "Example Treasury Fund"),
		}, untaggedContracts: 1},
		map[string]timescale.AssetRow{
			rwaContractGood: rwaContractRow(rwaContractGood, sptr("1.0740000000")),
			rwaContractPool: rwaContractRow(rwaContractPool, sptr("2.00")),
		},
		map[string]string{rwaContractGood: "USTRY", rwaContractPool: "XAUm"},
		map[string]string{rwaContractGood: "1000000000000", rwaContractPool: "9000000000000"},
		map[string]uint32{rwaContractGood: 6, rwaContractPool: 7},
		nil,
	))
	for _, a := range view.Assets {
		if a.ContractID == rwaContractPool {
			t.Fatalf("served a pool contract as a real-world asset: %+v", a)
		}
	}
	// The untagged contract never reached the definition — the scan
	// excluded it — so it is reported as a funnel stage drop, not a
	// refusal. Both views are served for exactly this reason.
	assertContractDrop(t, view, "directory_contract_addresses", "contract_named_without_issuing_tag", 1)
}

// TestRWAContracts_ReportsRecognisedEntitiesItCannotReach is the
// coverage statement. Franklin Templeton is recognised, unflagged, real,
// and this index holds no token for it — so it appears by NAME rather
// than being silently absent.
func TestRWAContracts_ReportsRecognisedEntitiesItCannotReach(t *testing.T) {
	const ftAddr = "GBHNGLLIE3KWGKCHIKMHJ5HVZHYIK7WTBE4QF5PLAKL4CJGSEU7HZIW5"
	view := getRWA(t, rwaContractServer(t,
		&stubRWAContractReader{
			accounts: 18000,
			unreached: []timescale.DirectoryEntry{{
				Address: ftAddr, Name: "Franklin Templeton", Domain: "franklintempleton.com",
				Tags: []string{"issuer"}, Source: "stellar-expert",
			}},
		},
		nil, nil, nil, nil, nil,
	))
	if len(view.UnreachedEntities) != 1 {
		t.Fatalf("unreached_entities = %d, want 1: %+v", len(view.UnreachedEntities), view.UnreachedEntities)
	}
	e := view.UnreachedEntities[0]
	if e.Address != ftAddr || e.Name != "Franklin Templeton" {
		t.Errorf("unreached entity = %+v", e)
	}
	// And the exact count is a terminal funnel stage, so a reader who
	// only walks the funnel still sees it.
	assertContractStage(t, view, "directory_recognised_issuing_accounts", 1)
}

// TestRWAContracts_FunnelBalancesAcrossBothArms is the accounting
// property. The two arms narrow different populations from different
// roots, so the arithmetic has to close WITHIN each arm and must not be
// attempted across the boundary.
func TestRWAContracts_FunnelBalancesAcrossBothArms(t *testing.T) {
	view := getRWA(t, rwaContractServer(t,
		&stubRWAContractReader{
			contracts:         []timescale.DirectoryEntry{recognisedContract(rwaContractGood, "Example Treasury Fund")},
			accounts:          18000,
			scamContracts:     7,
			untaggedContracts: 431,
			unreached: []timescale.DirectoryEntry{{
				Address: "GBHNGLLIE3KWGKCHIKMHJ5HVZHYIK7WTBE4QF5PLAKL4CJGSEU7HZIW5",
				Name:    "Franklin Templeton", Tags: []string{"issuer"},
			}},
		},
		map[string]timescale.AssetRow{rwaContractGood: rwaContractRow(rwaContractGood, sptr("1.0740000000"))},
		map[string]string{rwaContractGood: "USTRY"},
		map[string]string{rwaContractGood: "1000000000000"},
		map[string]uint32{rwaContractGood: 6},
		nil,
	))
	if !view.Funnel.Balanced {
		t.Fatalf("funnel does not balance:\n%s", funnelDump(view))
	}
	// Both arms present, and every stage says which it belongs to. An
	// unlabelled stage would let a reader subtract across the boundary.
	arms := map[string]int{}
	for _, s := range view.Funnel.Stages {
		if s.Arm == "" {
			t.Errorf("stage %q carries no arm", s.Stage)
		}
		arms[s.Arm]++
	}
	if arms["classic"] == 0 || arms["contract"] == 0 {
		t.Fatalf("arms = %v, want both populated", arms)
	}
	// The contract arm's first stage really did start from the whole
	// curated set, not from the contracts alone.
	assertContractStage(t, view, "curated_directory_entries", 18000+1+7+431)
	assertContractDrop(t, view, "curated_directory_entries", "directory_entry_names_an_account", 18000)
	assertContractStage(t, view, "contract_assets_served", 1)
}

// TestRWAContracts_UnwiredReaderSaysNotMeasured pins the distinction the
// whole funnel exists for: nothing was looked at is not the same finding
// as nothing was there.
func TestRWAContracts_UnwiredReaderSaysNotMeasured(t *testing.T) {
	srv := v1.New(v1.Options{
		Sep1Cache:    &stubSep1BoundReader{},
		Directory:    &stubDirectoryReader{},
		AssetsReader: &rwaListStub{stubAssetsReaderExt: &stubAssetsReaderExt{}, byIssuer: map[string][]timescale.AssetRow{}},
	})
	view := getRWA(t, srv)
	for _, s := range view.Funnel.Stages {
		if s.Arm == "contract" {
			t.Fatalf("contract stage %q served with no reader wired — a narrowing of zeros reads as a measured population", s.Stage)
		}
	}
	if !strings.Contains(view.Funnel.Basis, "NOT MEASURED") {
		t.Errorf("funnel basis does not say the contract arm was unmeasured:\n%s", view.Funnel.Basis)
	}
}

// ─── helpers ────────────────────────────────────────────────────────

func assertContractStage(t *testing.T, v v1.RWAAssetsView, stage string, want int) {
	t.Helper()
	for _, s := range v.Funnel.Stages {
		if s.Stage == stage {
			if s.Count != want {
				t.Errorf("stage %s count = %d, want %d", stage, s.Count, want)
			}
			return
		}
	}
	t.Errorf("stage %s absent:\n%s", stage, funnelDump(v))
}

func assertContractDrop(t *testing.T, v v1.RWAAssetsView, stage, reason string, want int) {
	t.Helper()
	for _, s := range v.Funnel.Stages {
		if s.Stage != stage {
			continue
		}
		for _, d := range s.Dropped {
			if d.Reason == reason {
				if d.Count != want {
					t.Errorf("stage %s drop %s = %d, want %d", stage, reason, d.Count, want)
				}
				return
			}
		}
	}
	t.Errorf("stage %s has no drop %s:\n%s", stage, reason, funnelDump(v))
}

// deref renders an optional money string for an assertion message,
// keeping absent and empty distinguishable.
func deref(p *string) string {
	if p == nil {
		return "<absent>"
	}
	return *p
}

// funnelDump renders the funnel for a failure message. The whole point
// of the structure is that a human can follow the arithmetic, and a
// struct print of it cannot be followed.
func funnelDump(v v1.RWAAssetsView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "balanced=%v\n", v.Funnel.Balanced)
	for _, s := range v.Funnel.Stages {
		fmt.Fprintf(&b, "  [%-8s] %-42s %-22s %d\n", s.Arm, s.Stage, s.Unit, s.Count)
		for _, d := range s.Dropped {
			fmt.Fprintf(&b, "             -%-8d %-45s (%s)\n", d.Count, d.Reason, d.Actor)
		}
	}
	return b.String()
}
