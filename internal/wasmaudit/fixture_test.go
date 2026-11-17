// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package wasmaudit

import (
	"path/filepath"
	"testing"
)

// hashesWithoutFixture is the count of attested hashes with no captured-event fixture; it may only fall.
const hashesWithoutFixture = 127

// A newly attested hash must ship a captured event under test/fixtures/<source>/<hash>/ (wasm-audits README §3).
func TestManifest_NewHashNeedsFixture(t *testing.T) {
	var missing int
	for h := range mustLoad(t) {
		files, err := filepath.Glob(filepath.Join("..", "..", "test", "fixtures", "*", h, "*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 0 {
			missing++
		}
	}
	switch {
	case missing > hashesWithoutFixture:
		t.Errorf("%d attested hashes have no test/fixtures/<source>/<hash>/ fixture, %d allowed: capture an event from each new hash", missing, hashesWithoutFixture)
	case missing < hashesWithoutFixture:
		t.Errorf("only %d attested hashes lack a fixture: lower hashesWithoutFixture to %d", missing, missing)
	}
}
