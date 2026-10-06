// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package pipeline_test

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/projector"
)

type fakeEvent struct{}

func (fakeEvent) EventKind() string { return "fake.event" }
func (fakeEvent) Source() string    { return "fake" }

type fakeDecoder struct{}

func (fakeDecoder) Name() string                                  { return "fake" }
func (fakeDecoder) Matches(events.Event) bool                     { return false }
func (fakeDecoder) Decode(events.Event) ([]consumer.Event, error) { return nil, nil }

// TestSpec_OneEntryProjects: one spec entry is all it takes for the sink
// to cede a source's events and the projector to write them, so the two
// writers cannot disagree about membership (AGENTS.md invariant 7).
func TestSpec_OneEntryProjects(t *testing.T) {
	restore := pipeline.SwapSpecsForTest(append(append([]pipeline.SourceSpec(nil), pipeline.Specs()...), pipeline.SourceSpec{
		Name:       "fake",
		Events:     []consumer.Event{fakeEvent{}},
		NewDecoder: func(pipeline.BuildArgs) (dispatcher.Decoder, error) { return fakeDecoder{}, nil },
		Projector:  &pipeline.ProjectorSpec{},
	}))
	defer restore()

	if !pipeline.IsProjectedEvent(fakeEvent{}) {
		t.Error("IsProjectedEvent(fakeEvent) = false: the sink would double-write a projected source")
	}
	reg, err := projector.BuildRegistry([]string{"fake"}, config.OracleConfig{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Sources) != 1 || reg.Sources[0].Name != "fake" {
		t.Errorf("BuildRegistry(fake) = %+v, want the one fake source: the sink would skip rows no projector writes", reg.Sources)
	}
	if _, err := pipeline.BuildDispatcher([]string{"fake"}, config.OracleConfig{}, nil); err != nil {
		t.Errorf("BuildDispatcher(fake): %v", err)
	}
}

// A watched spec is projected with no enabled_sources entry at all, and
// only RegisterSupplyEventDecoders (never BuildDispatcher) adds it to the
// dispatcher.
func TestSpec_WatchedSourcesNeedNoName(t *testing.T) {
	reg, err := projector.BuildRegistry(nil, config.OracleConfig{}, []string{"CWATCHED"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, s := range reg.Sources {
		got[s.Name] = true
	}
	supply, err := pipeline.RegisterSupplyEventDecoders(dispatcher.New(), config.SupplyConfig{WatchedSEP41Contracts: []string{"CWATCHED"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range pipeline.Specs() {
		if !spec.Watched {
			continue
		}
		if !got[spec.Name] {
			t.Errorf("watched spec %s missing from BuildRegistry(no names)", spec.Name)
		}
		if spec.Projector == nil || !spec.Projector.SoleWriter {
			t.Errorf("watched spec %s must be sole-writer projected: SinkModeSkipSoleWriter is what keeps its dispatcher copy from writing", spec.Name)
		}
		if _, err := pipeline.BuildDispatcher([]string{spec.Name}, config.OracleConfig{}, nil); err == nil {
			t.Errorf("BuildDispatcher accepted watched spec %s by name", spec.Name)
		}
	}
	if len(supply) != len(got) {
		t.Errorf("RegisterSupplyEventDecoders = %v, BuildRegistry(no names) = %v: the watched sets differ", supply, got)
	}
}
