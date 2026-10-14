// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/astguard"
)

// TestWebhookEventTypesListsEveryConstant requires every WebhookEventType constant to appear in WebhookEventTypes(), as Go has no exhaustiveness check over string constants.
func TestWebhookEventTypesListsEveryConstant(t *testing.T) {
	declared := declaredWebhookEventTypes(t)
	if len(declared) == 0 {
		t.Fatal("found no WebhookEventType constants — the parser guard is not looking at the right files")
	}
	listed := map[string]bool{}
	for _, e := range WebhookEventTypes() {
		if listed[string(e)] {
			t.Errorf("WebhookEventTypes() lists %q twice", e)
		}
		listed[string(e)] = true
	}
	for name, value := range declared {
		if !listed[value] {
			t.Errorf("constant %s = %q is not in WebhookEventTypes(): subscription validation and metric seeding cannot see it", name, value)
		}
	}
	if len(listed) != len(declared) {
		t.Errorf("WebhookEventTypes() has %d members, source declares %d constants", len(listed), len(declared))
	}
}

func TestIsWebhookEventType(t *testing.T) {
	for _, e := range WebhookEventTypes() {
		if !IsWebhookEventType(string(e)) {
			t.Errorf("IsWebhookEventType(%q) = false", e)
		}
	}
	for _, s := range []string{"", "incident", "INCIDENT.SEV1", "price.alert "} {
		if IsWebhookEventType(s) {
			t.Errorf("IsWebhookEventType(%q) = true", s)
		}
	}
}

// declaredWebhookEventTypes maps constant name → string value for every
// const spec in this package whose declared type is WebhookEventType.
func declaredWebhookEventTypes(t *testing.T) map[string]string {
	t.Helper()
	return astguard.ByName(astguard.Typed(t, astguard.PackageConsts(t, "."), "WebhookEventType"))
}
