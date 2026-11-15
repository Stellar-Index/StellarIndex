package main

import (
	"reflect"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
)

// The writer list follows the sink mode: a writer left out makes a current table read stale, one too many hides a
// stalled sole writer.
func TestAquariusReserveWriters_FollowTheSinkMode(t *testing.T) {
	dispatcher := v1.TVLWriterCursor{Source: cursorSourceLedgerstream}
	projector := v1.TVLWriterCursor{Source: "projector", Sub: aquarius.SourceName}
	cases := []struct {
		enabled, persist bool
		mode             pipeline.SinkMode
		want             []v1.TVLWriterCursor
	}{
		{false, true, pipeline.SinkModeAll, []v1.TVLWriterCursor{dispatcher}},
		{true, true, pipeline.SinkModeSkipSoleWriter, []v1.TVLWriterCursor{projector, dispatcher}},
		{true, false, pipeline.SinkModeSkipProjected, []v1.TVLWriterCursor{projector}},
	}
	for _, c := range cases {
		if got := pipeline.SinkModeForProjector(c.enabled, c.persist); got != c.mode {
			t.Fatalf("enabled=%v persist=%v: sink mode %v, want %v: the cases no longer match the pipeline", c.enabled, c.persist, got, c.mode)
		}
		got := aquariusReserveWriters(config.ProjectorConfig{Enabled: c.enabled, PersistPerSource: c.persist})
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("enabled=%v persist=%v: writers = %v, want %v", c.enabled, c.persist, got, c.want)
		}
	}
	spec, ok := pipeline.SpecByName(aquarius.SourceName)
	if !ok || spec.Projector == nil {
		t.Fatal("aquarius is no longer a projected source: aquariusReserveWriters is wrong")
	}
	if spec.Projector.SoleWriter {
		t.Error("aquarius became a sole-writer source: in parallel mode the dispatcher no longer writes aquarius_reserves, drop it from the list")
	}
}

// cursorSourceLedgerstream is the indexer's dispatcher cursor source (cmd/stellarindex-indexer cursorSource).
const cursorSourceLedgerstream = "ledgerstream"
