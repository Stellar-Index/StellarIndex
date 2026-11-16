package blnt_test

import (
	"path/filepath"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/wasmaudit"
)

// Keep fixture directories welded to the replay gate without introducing an
// import cycle into the in-package decoder tests.
func TestRealFixtureHashesAreAudited(t *testing.T) {
	t.Parallel()
	manifest, err := wasmaudit.Load()
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := filepath.Glob(filepath.Join("../../../test/fixtures/blnt", "*"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("fixture hash directories = %d (err %v)", len(dirs), err)
	}
	for _, dir := range dirs {
		hash := filepath.Base(dir)
		if entry, ok := manifest[hash]; !ok || !entry.Covers(blnt.SourceName) {
			t.Errorf("fixture WASM %s is not audited for %s", hash, blnt.SourceName)
		}
	}
}
