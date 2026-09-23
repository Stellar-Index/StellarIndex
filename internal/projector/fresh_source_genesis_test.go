package projector

import (
	"context"
	"slices"
	"sort"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sourcenet"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorocredit"
	sushiswap_v3 "github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
	"github.com/Stellar-Index/StellarIndex/internal/sources/upshift"
)

// TestFreshSourceStartsAtGenesis: a source with no cursor row starts at its
// declared Genesis, not at the lake floor (GH-567: upshift / sushiswap_v3
// crawled ~85 h of ledgers that cannot hold their events), and the lake
// floor still wins when it is the later of the two.
func TestFreshSourceStartsAtGenesis(t *testing.T) {
	for _, tc := range []struct {
		name     string
		genesis  uint32
		lakeMin  uint32
		wantFrom uint32
	}{
		{"genesis above the lake floor starts at genesis", upshift.GenesisLedger, 2, upshift.GenesisLedger},
		{"lake floor above genesis starts at the floor", sushiswap_v3.FactoryGenesisLedger, 63_000_000, 63_000_000},
		{"undeclared genesis keeps the lake floor", 0, 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newLakeProjector(&fakeStore{tipLedger: 70_000_000})
			// Watermark from-1: the start ledger is not yet complete, so the
			// cycle goes idle before the lake event scan.
			fake := &fakeLake{lakeMin: tc.lakeMin, wm: tc.wantFrom - 1}
			lake := &sourceLake{open: func(context.Context) (lakeReader, error) { return fake, nil }}
			src := Source{Name: "fresh-genesis", Decoder: &ledgerEchoDecoder{}, Genesis: tc.genesis}
			window := uint32(BatchLimit)
			var tracker poisonTracker
			var wedge wedgeTracker

			p.cycleOneSource(context.Background(), src, &window, &tracker, &wedge, lake)

			// The first-event seek and the cycle both read from the floor.
			if len(fake.wmFrom) == 0 || slices.ContainsFunc(fake.wmFrom, func(f uint32) bool { return f != tc.wantFrom }) {
				t.Fatalf("fresh source scanned from %v, want every scan from %d", fake.wmFrom, tc.wantFrom)
			}
		})
	}
}

// TestFreshSourceStartsAtGenesisWithoutLake: the non-CH path honours Genesis
// too: an event below genesis must not become the seed, or the first commit
// lands one window above it instead of at or above genesis.
func TestFreshSourceStartsAtGenesisWithoutLake(t *testing.T) {
	store := &fakeStore{tipLedger: 70_000_000, rows: []sorobanevents.Row{lakeRow(100, 1), lakeRow(upshift.GenesisLedger+5, 2)}}
	p := &Projector{store: store, logger: discardLog(), sink: func(context.Context, consumer.Event) error { return nil }}
	src := Source{Name: "fresh-genesis", Decoder: &ledgerEchoDecoder{}, Genesis: upshift.GenesisLedger}
	window := uint32(BatchLimit)
	var tracker poisonTracker
	var wedge wedgeTracker

	p.cycleOneSource(context.Background(), src, &window, &tracker, &wedge, nil)

	if !store.haveCursor {
		t.Fatal("first cycle committed no cursor")
	}
	if store.projectorCursor < upshift.GenesisLedger {
		t.Fatalf("first cycle committed cursor %d, below genesis %d: the seed ignored Genesis",
			store.projectorCursor, upshift.GenesisLedger)
	}
}

// crawlsFromLakeFloor lists the projected sources with no verified
// first-event constant: a fresh one starts at the lake floor. Adding a
// projected source means either declaring its Genesis in buildSource or
// listing it here — never silently inheriting the crawl.
var crawlsFromLakeFloor = map[string]string{
	"soroswap":        "no package genesis constant",
	"aquarius":        "no package genesis constant",
	"phoenix":         "no package genesis constant",
	"comet":           "no package genesis constant",
	"blend_backstop":  "BackstopGenesisLedger postdates the V1 backstop it also gates",
	"blend_emitter":   "no package genesis constant",
	"cctp":            "no package genesis constant",
	"rozo":            "no package genesis constant",
	"defindex":        "no package genesis constant",
	"sep41_transfers": "watched set is operator-chosen; runs on every network",
	"sep41_supply":    "watched set is operator-chosen; runs on every network",
	"reflector-dex":   "oracle contract is operator-configured",
	"reflector-cex":   "oracle contract is operator-configured",
	"reflector-fx":    "oracle contract is operator-configured",
	"redstone":        "oracle contract is operator-configured",
}

// TestProjectedSourcesDeclareGenesis is the guard for GH-567's class: every
// source buildSource projects either carries the expected Genesis or is an
// acknowledged lake-floor crawler. A Genesis is a PUBNET ledger, so only a
// pubnet-only source may carry one.
func TestProjectedSourcesDeclareGenesis(t *testing.T) {
	wantGenesis := map[string]uint32{
		upshift.SourceName:      upshift.GenesisLedger,
		sushiswap_v3.SourceName: sushiswap_v3.FactoryGenesisLedger,
		sorocredit.SourceName:   sorocredit.GenesisLedger,
		blend.SourceName:        blend.FactoryGenesisLedger,
	}
	contractID := sorocredit.MainnetContract
	oracle := config.OracleConfig{}
	oracle.Reflector.DEXContract = contractID
	oracle.Reflector.CEXContract = contractID
	oracle.Reflector.FXContract = contractID
	oracle.Redstone.AdapterContract = contractID

	names := make([]string, 0, len(config.KnownSources))
	for n := range config.KnownSources {
		names = append(names, n)
	}
	sort.Strings(names)
	projected := 0
	for _, name := range names {
		src, ok, err := buildSource(name, oracle, []string{contractID}, nil)
		if err != nil {
			t.Fatalf("buildSource(%q): %v", name, err)
		}
		if !ok {
			continue
		}
		projected++
		want, declared := wantGenesis[name]
		_, exempt := crawlsFromLakeFloor[name]
		switch {
		case declared && src.Genesis != want:
			t.Errorf("%s: Genesis = %d, want %d", name, src.Genesis, want)
		case declared && exempt:
			t.Errorf("%s: both declares a Genesis and is listed in crawlsFromLakeFloor", name)
		case !declared && src.Genesis != 0:
			t.Errorf("%s: carries Genesis %d the test does not pin", name, src.Genesis)
		case !declared && !exempt:
			t.Errorf("%s: projected source declares no Genesis — a fresh deploy crawls from the lake floor; set Source.Genesis or list it in crawlsFromLakeFloor", name)
		}
		if src.Genesis != 0 {
			if ok, _ := sourcenet.Applicable(name, sourcenet.Testnet); ok {
				t.Errorf("%s: runs on every network but carries pubnet Genesis %d", name, src.Genesis)
			}
		}
	}
	if projected < len(wantGenesis) {
		t.Fatalf("only %d projected sources built from config.KnownSources; the guard enumerated nothing useful", projected)
	}
}
