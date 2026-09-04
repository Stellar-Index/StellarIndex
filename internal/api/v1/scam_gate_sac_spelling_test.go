package v1_test

import (
	"context"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// The scam gate is keyed on the issuer G-address that only a CLASSIC
// asset carries, and every serving path hands it the RAW requested base.
// A Stellar Asset Contract wrapper is the same asset as the classic
// issuance it wraps but spells that asset as a C-address, so the gate's
// classic check rejected it as "nothing to flag" and the flagged
// issuer's aggregated price stayed servable to anyone who asked for the
// contract id instead of CODE-ISSUER — on /v1/price, /v1/vwap, /v1/twap,
// /v1/price/tip and /v1/chart alike (R8).
//
// These cases drive the REAL pricingguard.ScamGate, not a stub: the
// resolution being pinned lives inside it, so a stub gate keyed on the
// asset id would pass either way. The registry is process-global, so
// none of these run parallel (the convention
// TestTVLValuer_GateSeesTheCanonicalIdentity follows).

// sacScamDirectory is the curated-directory seam
// pricingguard.ScamGate reads, flagging exactly the listed G-addresses
// and recording which addresses it was asked about — so a test can
// assert the gate resolved the SAC to its issuer rather than merely
// that a 404 appeared.
type sacScamDirectory struct {
	flagged map[string]bool
	asked   []string
}

func (d *sacScamDirectory) DirectoryEntryByAddress(_ context.Context, address string) (timescale.DirectoryEntry, bool, error) {
	d.asked = append(d.asked, address)
	if d.flagged[address] {
		return timescale.DirectoryEntry{Address: address, Tags: []string{"unsafe"}}, true, nil
	}
	return timescale.DirectoryEntry{}, false, nil
}

// sacSpellingFixture is one flagged classic issuance, its declared SAC
// wrapper, and a directory that flags the issuer. Installing the alias
// registry is what makes the two spellings one asset; without the
// operator's [supply].sac_wrappers entry there is no classic twin to
// resolve to and the gate is unchanged.
type sacSpellingFixture struct {
	classic canonical.Asset
	sac     canonical.Asset
	dir     *sacScamDirectory
}

const (
	sacSpellingFlaggedCode   = "RIO"
	sacSpellingFlaggedIssuer = "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
)

// newSACSpellingFixture installs the wrapper registry for the test's
// lifetime and returns the two spellings of the one flagged asset.
// flagged=false builds the same shape with a CLEAN directory, which is
// the blast-radius control: resolution must not withhold anything the
// directory has not flagged.
func newSACSpellingFixture(t *testing.T, flagged bool) sacSpellingFixture {
	t.Helper()
	classic, err := canonical.NewClassicAsset(sacSpellingFlaggedCode, sacSpellingFlaggedIssuer)
	if err != nil {
		t.Fatalf("classic asset: %v", err)
	}
	sacID, err := classic.SacContractID()
	if err != nil {
		t.Fatalf("derive SAC: %v", err)
	}
	sac, err := canonical.NewSorobanAsset(sacID)
	if err != nil {
		t.Fatalf("soroban asset: %v", err)
	}
	reg, err := canonical.NewAliasRegistry(map[string]string{
		sacID: sacSpellingFlaggedCode + ":" + sacSpellingFlaggedIssuer,
	})
	if err != nil {
		t.Fatalf("alias registry: %v", err)
	}
	canonical.InstallAliasRegistry(reg)
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })

	dir := &sacScamDirectory{flagged: map[string]bool{}}
	if flagged {
		dir.flagged[sacSpellingFlaggedIssuer] = true
	}
	return sacSpellingFixture{classic: classic, sac: sac, dir: dir}
}

func (f sacSpellingFixture) gate() *pricingguard.ScamGate {
	return pricingguard.NewScamGate(f.dir, pricingguard.ScamGateOptions{})
}

// sacSpellingHistory serves trades for every spelling of the flagged
// asset against native, so an UNGATED path answers 200 with a real
// price and the withheld assertions cannot pass vacuously. Two
// timestamps because the surfaces read different windows: /v1/price/tip
// needs one inside its rolling seconds-wide window, /v1/twap and
// /v1/vwap read a closed-bucket hour that a two-second-old trade sits
// outside of.
func sacSpellingHistory(t *testing.T, f sacSpellingFixture) *pairAwareHistoryReader {
	t.Helper()
	native, err := canonical.ParseAsset("native")
	if err != nil {
		t.Fatalf("parse native: %v", err)
	}
	now := time.Now().UTC()
	byPair := map[string][]canonical.Trade{}
	for i, base := range []canonical.Asset{f.classic, f.sac} {
		pair, err := canonical.NewPair(base, native)
		if err != nil {
			t.Fatalf("NewPair(%s): %v", base, err)
		}
		trades := make([]canonical.Trade, 0, 2)
		for j, at := range []time.Time{now.Add(-30 * time.Minute), now.Add(-2 * time.Second)} {
			trades = append(trades, canonical.Trade{
				Source: "sdex", Ledger: uint32(1 + 2*i + j), //nolint:gosec // small literal loop indices
				TxHash:      strings.Repeat("0", 63) + string(rune('1'+2*i+j)),
				Timestamp:   at,
				Pair:        pair,
				BaseAmount:  canonical.NewAmount(big.NewInt(1_000_000)),
				QuoteAmount: canonical.NewAmount(big.NewInt(160_000)),
			})
		}
		byPair[base.String()+"/native"] = trades
	}
	return &pairAwareHistoryReader{tradesByPair: byPair}
}

// sacSpellingSurfaces are the five price-claim surfaces the raw base
// reaches the gate on. /v1/price is pinned at its chokepoint in
// cmd/stellarindex-api (TestPriceWithheldChokepointResolvesSACSpelling)
// because its gate call lives in the store-backed reader; the other
// four gate in this package's handlers and are exercised over HTTP.
func sacSpellingSurfaces(f sacSpellingFixture) []struct {
	name string
	path string
} {
	base := f.sac.String()
	return []struct {
		name string
		path string
	}{
		{"/v1/vwap", "/v1/vwap?base=" + base + "&quote=native"},
		{"/v1/twap", "/v1/twap?base=" + base + "&quote=native"},
		{"/v1/price/tip", "/v1/price/tip?asset=" + base + "&quote=native&window_seconds=60"},
		{"/v1/chart", "/v1/chart?base=" + base + "&quote=native&timeframe=24h"},
	}
}

// TestScamGateWithholdsSACSpellingOnEveryPriceSurface is the headline
// case: the flagged issuer's price must be refused when the caller
// spells the asset as its SAC contract id, on every surface that
// consults the gate.
func TestScamGateWithholdsSACSpellingOnEveryPriceSurface(t *testing.T) {
	f := newSACSpellingFixture(t, true)
	for _, s := range sacSpellingSurfaces(f) {
		t.Run(s.name, func(t *testing.T) {
			srv := v1.New(v1.Options{
				History: sacSpellingHistory(t, f),
				Prices:  &stubPriceReader{},
				Scam:    f.gate(),
			})
			ts := httpTestServer(t, srv)

			resp := mustGet(t, ts.URL+s.path)
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s with the SAC spelling status = %d, want 404 — the wrapper "+
					"is the same asset as the flagged classic issuance, so the C-address "+
					"must not be a way around the withholding decision. Body: %s",
					s.name, resp.StatusCode, body)
			}
			if !strings.Contains(string(body), "errors/price-withheld") {
				t.Errorf("%s body missing the price-withheld problem type: %s", s.name, body)
			}
		})
	}
}

// TestScamGateSACSpellingAsksAboutTheClassicIssuer pins the MECHANISM,
// not just the status code. A 404 could come from anywhere; the gate
// resolving the wrapper is proven by the directory being asked about the
// classic issuance's G-address, which the SAC spelling does not contain.
func TestScamGateSACSpellingAsksAboutTheClassicIssuer(t *testing.T) {
	f := newSACSpellingFixture(t, true)
	srv := v1.New(v1.Options{History: sacSpellingHistory(t, f), Prices: &stubPriceReader{}, Scam: f.gate()})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/vwap?base="+f.sac.String()+"&quote=native")
	_ = resp.Body.Close()

	if len(f.dir.asked) == 0 {
		t.Fatal("the directory was never consulted for a SAC-spelled base — the gate " +
			"short-circuited on the classic check instead of resolving the wrapper")
	}
	if f.dir.asked[0] != sacSpellingFlaggedIssuer {
		t.Errorf("directory asked about %q, want the classic issuance's issuer %q",
			f.dir.asked[0], sacSpellingFlaggedIssuer)
	}
}

// TestScamGateClassicSpellingUnchanged is the direction guard. A
// configured classic↔SAC family is ordered classic-first, so resolving
// a classic base returns it untouched — the spelling that already
// withheld must keep withholding, by the same issuer lookup.
func TestScamGateClassicSpellingUnchanged(t *testing.T) {
	f := newSACSpellingFixture(t, true)
	for _, s := range []struct{ name, path string }{
		{"/v1/vwap", "/v1/vwap?base=" + f.classic.String() + "&quote=native"},
		{"/v1/twap", "/v1/twap?base=" + f.classic.String() + "&quote=native"},
		{"/v1/price/tip", "/v1/price/tip?asset=" + f.classic.String() + "&quote=native&window_seconds=60"},
		{"/v1/chart", "/v1/chart?base=" + f.classic.String() + "&quote=native&timeframe=24h"},
	} {
		t.Run(s.name, func(t *testing.T) {
			srv := v1.New(v1.Options{
				History: sacSpellingHistory(t, f),
				Prices:  &stubPriceReader{},
				Scam:    f.gate(),
			})
			ts := httpTestServer(t, srv)

			resp := mustGet(t, ts.URL+s.path)
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s with the CLASSIC spelling status = %d, want 404 unchanged. Body: %s",
					s.name, resp.StatusCode, body)
			}
		})
	}
}

// TestScamGateUnflaggedSACSpellingStillServes is the blast-radius
// guard. Resolution must change WHICH identity the directory is asked
// about and nothing else: an asset the directory has not flagged keeps
// serving under both spellings, or the fix has taken four public
// endpoints dark for every wrapped asset on the network.
func TestScamGateUnflaggedSACSpellingStillServes(t *testing.T) {
	f := newSACSpellingFixture(t, false)
	for _, s := range sacSpellingSurfaces(f) {
		t.Run(s.name, func(t *testing.T) {
			srv := v1.New(v1.Options{
				History: sacSpellingHistory(t, f),
				Prices:  &stubPriceReader{},
				Scam:    f.gate(),
			})
			ts := httpTestServer(t, srv)

			resp := mustGet(t, ts.URL+s.path)
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("%s for an UNFLAGGED wrapped asset returned %d, want 200 — "+
					"resolving the base must not withhold markets the directory has "+
					"nothing to say about. Body: %s", s.name, resp.StatusCode, body)
			}
		})
	}
}

// TestScamGateUndeclaredSACSpellingIsANoOp pins the deployment-shape
// promise: with no [supply].sac_wrappers entry for the asset there is
// no classic twin to resolve to, so the wrapper stays its own identity,
// the directory is never asked, and behaviour is exactly what it was.
func TestScamGateUndeclaredSACSpellingIsANoOp(t *testing.T) {
	canonical.InstallAliasRegistry(nil) // XLM-only baseline: no operator wrappers
	t.Cleanup(func() { canonical.InstallAliasRegistry(nil) })

	classic, err := canonical.NewClassicAsset(sacSpellingFlaggedCode, sacSpellingFlaggedIssuer)
	if err != nil {
		t.Fatalf("classic asset: %v", err)
	}
	sacID, err := classic.SacContractID()
	if err != nil {
		t.Fatalf("derive SAC: %v", err)
	}
	sac, err := canonical.NewSorobanAsset(sacID)
	if err != nil {
		t.Fatalf("soroban asset: %v", err)
	}
	dir := &sacScamDirectory{flagged: map[string]bool{sacSpellingFlaggedIssuer: true}}
	f := sacSpellingFixture{classic: classic, sac: sac, dir: dir}

	srv := v1.New(v1.Options{History: sacSpellingHistory(t, f), Prices: &stubPriceReader{}, Scam: f.gate()})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/vwap?base="+sac.String()+"&quote=native")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("an UNDECLARED wrapper returned %d, want 200 — a deployment with no "+
			"wrapper configured must see no behaviour change. Body: %s", resp.StatusCode, body)
	}
	if len(dir.asked) != 0 {
		t.Errorf("directory was asked about %v for an undeclared wrapper, want no lookup — "+
			"the contract id carries no G-address the directory could match", dir.asked)
	}
}
