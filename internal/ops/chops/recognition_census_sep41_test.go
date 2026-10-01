package chops

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	sep41transfers "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
)

// TestBuildCensusDispatcher_RecognisesWatchedSEP41 pins that the recognition
// census carries the same watched-set sep41 decoders as the indexer, so the
// CAP-67 shapes watchedSep41RecognitionShapes re-adds are not reported as gaps.
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
