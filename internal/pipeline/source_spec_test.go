// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
)

func TestSpec_NamesUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range specs {
		if seen[s.Name] {
			t.Errorf("two specs named %q: SpecByName returns only the first", s.Name)
		}
		seen[s.Name] = true
	}
}

// reflector.UpdateEvent is shared by three specs; one of them claiming a
// different writer role must fail loudly rather than pick a winner.
func TestIndexEventRoles_ConflictingRolesPanic(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("indexEventRoles accepted one event type with two writer roles")
		}
	}()
	indexEventRoles([]SourceSpec{
		{Name: "a", Events: []consumer.Event{reflector.UpdateEvent{}}, Projector: &ProjectorSpec{}},
		{Name: "b", Events: []consumer.Event{reflector.UpdateEvent{}}},
	})
}
