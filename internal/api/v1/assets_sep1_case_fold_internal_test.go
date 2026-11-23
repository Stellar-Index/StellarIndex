package v1

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const foldIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// foldChainStub serves one issuer's stellar.toml and logo declarations and the codes on chain under it.
type foldChainStub struct {
	stubSep1ImagesReader
	declared []string
	chain    []string
	readErr  error
	probes   int
}

func (f *foldChainStub) GetIssuerSep1Cached(context.Context, string) (*timescale.IssuerSep1Cached, error) {
	sep := &timescale.IssuerSep1Cached{}
	for _, c := range f.declared {
		sep.Currencies = append(sep.Currencies, timescale.IssuerSep1Currency{Code: c, Issuer: foldIssuer, Name: "decl " + c})
	}
	return sep, nil
}

func (f *foldChainStub) ClassicCodesFoldingTo(_ context.Context, keys []timescale.ClassicCodeKey) (map[timescale.ClassicCodeKey][]string, error) {
	f.probes++
	if f.readErr != nil {
		return nil, f.readErr
	}
	out := map[timescale.ClassicCodeKey][]string{}
	for _, k := range keys {
		for _, c := range f.chain {
			if k.Issuer == foldIssuer && len(out[k]) < 3 && strings.EqualFold(c, k.Code) {
				out[k] = append(out[k], c)
			}
		}
	}
	return out, nil
}

func newFoldStub(declared, chain []string) *foldChainStub {
	f := &foldChainStub{declared: declared, chain: chain}
	for _, c := range declared {
		f.imgs = append(f.imgs, timescale.Sep1Image{Code: c, Issuer: foldIssuer, Image: "https://issuer.example/" + c + ".png"})
	}
	return f
}

// foldOutcome runs the detail overlay and the listing logo overlay for every on-chain code.
func foldOutcome(t *testing.T, f *foldChainStub) (status map[string]string, logo map[string]string, unread map[string]bool) {
	t.Helper()
	s := discardServer(f)
	s.PrewarmSep1Images(context.Background())
	status, logo, unread = map[string]string{}, map[string]string{}, map[string]bool{}
	rows := make([]AssetDetail, 0, len(f.chain))
	for _, code := range f.chain {
		a, err := canonical.NewClassicAsset(code, foldIssuer)
		if err != nil {
			t.Fatalf("NewClassicAsset(%s): %v", code, err)
		}
		var d AssetDetail
		s.applySep1Overlay(context.Background(), &d, a)
		status[code], unread[code] = d.Sep1Status, d.sep1Unread
		rows = append(rows, AssetDetail{AssetID: code + "-" + foldIssuer, Type: "classic", Code: code, Issuer: ptr(foldIssuer)})
	}
	s.fillImagesFromSep1(context.Background(), rows)
	for _, r := range rows {
		if r.Image != nil {
			logo[r.Code] = *r.Image
		}
	}
	return status, logo, unread
}

// (i) A trustline-only `usdc` beside the declared `USDC` takes nothing; `USDC` keeps everything.
func TestSep1CaseFold_variantBesideExactTakesNothing(t *testing.T) {
	status, logo, _ := foldOutcome(t, newFoldStub([]string{"USDC"}, []string{"USDC", "usdc"}))
	if status["USDC"] != "verified" || logo["USDC"] != "https://issuer.example/USDC.png" {
		t.Errorf("USDC: status %q logo %q, want verified with its logo", status["USDC"], logo["USDC"])
	}
	if status["usdc"] == "verified" || logo["usdc"] != "" {
		t.Errorf("usdc inherited USDC: status %q logo %q", status["usdc"], logo["usdc"])
	}
}

// (ii) A mis-cased declaration reaches the one on-chain asset it can mean.
func TestSep1CaseFold_misCasedDeclarationReachesTheOneAsset(t *testing.T) {
	status, logo, _ := foldOutcome(t, newFoldStub([]string{"Ustry"}, []string{"USTRY"}))
	if status["USTRY"] != "verified" || logo["USTRY"] != "https://issuer.example/Ustry.png" {
		t.Errorf("USTRY: status %q logo %q, want verified with the Ustry logo", status["USTRY"], logo["USTRY"])
	}
}

// (iii) Two on-chain spellings make a mis-cased declaration ambiguous: neither is verified, neither wears the logo.
func TestSep1CaseFold_ambiguousFoldMatchesNone(t *testing.T) {
	status, logo, _ := foldOutcome(t, newFoldStub([]string{"Ustry"}, []string{"USTRY", "ustry"}))
	for _, c := range []string{"USTRY", "ustry"} {
		if status[c] != "no_match" || logo[c] != "" {
			t.Errorf("%s: status %q logo %q, want no_match and no logo", c, status[c], logo[c])
		}
	}
}

// (iv) A failed chain read folds nothing and the detail says its read failed.
func TestSep1CaseFold_readErrorFailsClosedAndFlags(t *testing.T) {
	f := newFoldStub([]string{"Ustry"}, []string{"USTRY"})
	f.readErr = errors.New("pg down")
	status, logo, unread := foldOutcome(t, f)
	if status["USTRY"] == "verified" || logo["USTRY"] != "" || !unread["USTRY"] {
		t.Errorf("USTRY on read error: status %q logo %q unread %v, want no fold and unread", status["USTRY"], logo["USTRY"], unread["USTRY"])
	}
}

// The common case costs nothing: an exact declaration never probes the chain.
func TestSep1CaseFold_exactMatchDoesNotProbe(t *testing.T) {
	f := newFoldStub([]string{"USDC"}, []string{"USDC"})
	foldOutcome(t, f)
	if f.probes != 0 {
		t.Errorf("exact match probed the chain %d times, want 0", f.probes)
	}
}

func rwaFoldRows(codes ...string) []timescale.AssetRow {
	rows := make([]timescale.AssetRow, len(codes))
	for i, c := range codes {
		rows[i] = timescale.AssetRow{AssetID: c + "-" + foldIssuer, Code: c, IssuerGStrkey: foldIssuer}
	}
	return rows
}

// The RWA join folds only over a complete page with one candidate, and an exact row always wins.
func TestRWAMatchMemberRows_caseFoldRule(t *testing.T) {
	full := make([]string, 0, rwaAssetsPerIssuer)
	full = append(full, "ustry")
	for i := 1; i < rwaAssetsPerIssuer; i++ {
		full = append(full, fmt.Sprintf("X%d", i))
	}
	for name, tc := range map[string]struct {
		declared []string
		chain    []string
		want     []string
	}{
		"variant beside exact":         {[]string{"USDC"}, []string{"usdc", "USDC"}, []string{"USDC"}},
		"mis-cased, one candidate":     {[]string{"Ustry"}, []string{"USTRY"}, []string{"USTRY"}},
		"mis-cased, two candidates":    {[]string{"Ustry"}, []string{"USTRY", "ustry"}, nil},
		"full page, exact may be past": {[]string{"USTRY"}, full, nil},
	} {
		t.Run(name, func(t *testing.T) {
			keep, _ := rwaMatchMemberRows(tc.declared, rwaFoldRows(tc.chain...))
			got := make([]string, 0, len(keep))
			for _, r := range keep {
				got = append(got, r.Code)
			}
			if fmt.Sprint(got) != fmt.Sprint(append([]string{}, tc.want...)) {
				t.Errorf("kept %v, want %v", got, tc.want)
			}
		})
	}
}
