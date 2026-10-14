package chops

import (
	"reflect"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	blend_backstop "github.com/Stellar-Index/StellarIndex/internal/sources/blend_backstop"
	"github.com/Stellar-Index/StellarIndex/internal/sources/rozo"
	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	sep41transfers "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// staticOwners builds the contract-to-sources map as computeCompleteness seeds it before the registry fold
// (contractOwners over static contractIDs). rozo and blend_backstop are not gated-registry sources, so
// loadRegistryOwners never names their contracts; this pass is the only path into ownerOf.
func staticOwners(t *testing.T) (map[string][]string, []reconSource) {
	t.Helper()
	return staticOwnersFor(t, config.Config{})
}

func staticOwnersFor(t *testing.T, cfg config.Config) (map[string][]string, []reconSource) {
	t.Helper()
	cat, _, err := buildReconciliationCatalogue(cfg)
	if err != nil {
		t.Fatalf("buildReconciliationCatalogue: %v", err)
	}
	return contractOwners(cat), cat
}

func catalogueSource(t *testing.T, cat []reconSource, name string) reconSource {
	t.Helper()
	for _, src := range cat {
		if src.name == name {
			return src
		}
	}
	t.Fatalf("catalogue has no %q source", name)
	return reconSource{}
}

// Pins contract ownership: without declared contractIDs an unhandled topic on a Rozo payment contract or the Blend
// backstop falls into the system-wide `unattributed` bucket and the source's recognition_ok can never go false. Walks
// every contract each decoder claims, so an unpinned addition fails.
func TestRecognitionAttribution_RozoAndBackstopOwnTheirContracts(t *testing.T) {
	ownerOf, cat := staticOwners(t)

	cases := []struct {
		source    string
		contracts []string
	}{
		{"rozo", rozo.MainnetPaymentContracts},
		{blend_backstop.SourceName, []string{blend_backstop.MainnetBackstopV2, blend_backstop.MainnetBackstopV1}},
	}
	for _, tc := range cases {
		src := catalogueSource(t, cat, tc.source)
		if len(tc.contracts) == 0 {
			t.Fatalf("%s: no contracts to probe — this case is asserting nothing", tc.source)
		}
		for _, contract := range tc.contracts {
			gapLedger := src.genesis + 1_000
			gaps := []completeness.RecognitionGap{{
				ContractID: contract, Topic0Sym: "topic_no_arm_handles",
				MinLedger: gapLedger, MaxLedger: gapLedger, Count: 1,
				Reason: "no decoder matches",
			}}
			recBySource, unattributed := attributeRecognitionGaps(ownerOf, gaps)

			if len(unattributed) != 0 {
				t.Errorf("%s: an unrecognised topic on its own contract %s fell into the system-wide "+
					"bucket (unattributed=%d) — the source's recognition axis cannot see it (F071)",
					tc.source, contract, len(unattributed))
			}
			got := recBySource[tc.source]
			if len(got) != 1 || got[0] != gapLedger {
				t.Errorf("%s: gap on %s attributed as %v, want exactly [%d] on %s",
					tc.source, contract, recBySource, gapLedger, tc.source)
			}
			ok, problems := sourceRecognitionOK(src.genesis, gapLedger, got, false, priorProjection{})
			if ok {
				t.Errorf("%s: recognition_ok stayed TRUE over an unhandled topic on %s (F071)", tc.source, contract)
			}
			if len(problems) != 1 || problems[0] != gapLedger {
				t.Errorf("%s: recognition problem ledgers = %v, want [%d] folded into the watermark",
					tc.source, problems, gapLedger)
			}
		}
	}
}

// With sep41_transfers and sep41_supply both gated on the same watched list, a recognition gap on any pinned contract
// must cap EVERY source pinning it. A single-owner map would give each contract to one source, so the other's
// recognition_ok could not go false.
func TestRecognitionAttribution_EveryPinningSourceCanFail(t *testing.T) {
	sep41Contract, err := strkey.Encode(strkey.VersionByteContract, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	cfg.Supply.WatchedSEP41Contracts = []string{sep41Contract}
	ownerOf, cat := staticOwnersFor(t, cfg)

	pinners := map[string][]string{}
	for _, src := range cat {
		for _, c := range src.contractIDs {
			pinners[c] = append(pinners[c], src.name)
		}
	}
	if len(pinners[sep41Contract]) < 2 {
		t.Fatalf("watched SEP-41 contract pinned by %v, want both sep41 sources — the shared case is not exercised",
			pinners[sep41Contract])
	}
	for c, want := range pinners {
		const gapLedger = 60_000_000
		recBySource, unattributed := attributeRecognitionGaps(ownerOf, []completeness.RecognitionGap{{
			ContractID: c, Topic0Sym: "topic_no_arm_handles", MinLedger: gapLedger, MaxLedger: gapLedger, Count: 1,
		}})
		if len(unattributed) != 0 {
			t.Errorf("gap on pinned contract %s fell into unattributed", c)
		}
		for _, name := range want {
			ok, _ := sourceRecognitionOK(clickhouse.SorobanGenesisLedger, gapLedger, recBySource[name], false, priorProjection{})
			if ok {
				t.Errorf("%s pins %s but recognition_ok stayed true over a gap on it (attributed: %v)",
					name, c, recBySource)
			}
		}
	}
}

// Topic-matched sources have no static contractIDs, so without the registry
// fold a gap on their pool lands in `unattributed` and recognition_ok can never
// fail. Walks mergeRegistryOwners → attributeRecognitionGaps → sourceRecognitionOK.
func TestRecognitionAttribution_TopicMatchedSourceFailsOnItsPoolGap(t *testing.T) {
	const soroswapPool = "CPOOLSOROSWAPxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	const phoenixChild = "CPHOENIXCHILDyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy"
	const foreignContract = "CFOREIGNZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"
	const soroswapGenesis = 50_746_266

	ownerOf := map[string][]string{}
	mergeRegistryOwners(ownerOf,
		map[string][]string{"phoenix": {phoenixChild}},
		[]string{soroswapPool},
	)
	if !reflect.DeepEqual(ownerOf[soroswapPool], []string{"soroswap"}) {
		t.Fatalf("soroswap pool not attributed: ownerOf[%s]=%q, want [soroswap]", soroswapPool, ownerOf[soroswapPool])
	}
	if !reflect.DeepEqual(ownerOf[phoenixChild], []string{"phoenix"}) {
		t.Fatalf("phoenix child not attributed: ownerOf[%s]=%q, want [phoenix]", phoenixChild, ownerOf[phoenixChild])
	}

	gaps := []completeness.RecognitionGap{
		{ContractID: soroswapPool, Topic0Sym: "swap", MinLedger: 61_000_000, Reason: "no decoder matches"},
		{ContractID: foreignContract, Topic0Sym: "mystery", MinLedger: 62_000_000, Reason: "no decoder matches"},
	}
	recBySource, unattributed := attributeRecognitionGaps(ownerOf, gaps)
	if len(recBySource["soroswap"]) != 1 || recBySource["soroswap"][0] != 61_000_000 {
		t.Fatalf("soroswap gap not attributed to soroswap: recBySource[soroswap]=%v", recBySource["soroswap"])
	}
	if len(unattributed) != 1 || unattributed[0].ContractID != foreignContract {
		t.Fatalf("foreign gap mis-attributed; unattributed=%+v", unattributed)
	}

	ok, problems := sourceRecognitionOK(soroswapGenesis, 61_000_000, recBySource["soroswap"], false, priorProjection{})
	if ok {
		t.Fatal("soroswap recognition_ok stayed TRUE over a dropped topic on its own pool")
	}
	if len(problems) != 1 || problems[0] != 61_000_000 {
		t.Fatalf("recognition problem ledger not pinned into the watermark: %v", problems)
	}
}

// TestBuildCensusDispatcher_RecognisesWatchedSEP41 pins that the recognition
// census carries the same watched-set sep41 decoders as the indexer, so the
// CAP-67 shapes the scan keeps for watched contracts are not reported as gaps.
func TestBuildCensusDispatcher_RecognisesWatchedSEP41(t *testing.T) {
	const (
		watched   = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUOZWS4HG3B5UPHHC2QQA"
		unwatched = "CB46LMGJC7SYSH4C7SBNLV635OX5BSNQDGRR32NRXAV7N2AVNZMQUJ3A"
	)
	shapes := map[string][]string{
		"approve": {sep41transfers.TopicSymbolApprove},
		"mint":    {sep41supply.TopicSymbolMint},
	}
	ev := func(contract string, topic []string) events.Event {
		return events.Event{Type: "contract", ContractID: contract, Topic: topic}
	}

	var cfg config.Config
	cfg.Supply.WatchedSEP41Contracts = []string{watched}
	disp, err := buildCensusDispatcher(cfg, nil)
	if err != nil {
		t.Fatalf("buildCensusDispatcher: %v", err)
	}
	for kind, topic := range shapes {
		if _, ok := disp.Recognize(ev(watched, topic)); !ok {
			t.Errorf("%s on a watched contract not recognised by the census dispatcher", kind)
		}
		if name, ok := disp.Recognize(ev(unwatched, topic)); ok {
			t.Errorf("%s on an unwatched contract recognised by %q; the sep41 decoders must stay watched-set gated", kind, name)
		}
	}

	empty, err := buildCensusDispatcher(config.Config{}, nil)
	if err != nil {
		t.Fatalf("buildCensusDispatcher(empty watch list): %v", err)
	}
	for kind, topic := range shapes {
		if name, ok := empty.Recognize(ev(watched, topic)); ok {
			t.Errorf("%s recognised by %q with an empty watch list; the sep41 decoders are opt-in", kind, name)
		}
	}
}
