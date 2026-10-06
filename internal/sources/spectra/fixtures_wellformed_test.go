package spectra_test

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
	"github.com/Stellar-Index/StellarIndex/internal/sources/spectra"
	"github.com/Stellar-Index/StellarIndex/internal/wasmaudit"
)

// Every fixture sits under an audited WASM hash, its file name carries the
// topic[0] symbol it holds, and its bytes are valid base64.
func TestFixtures_WellFormed(t *testing.T) {
	t.Parallel()
	manifest, err := wasmaudit.Load()
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	files, err := filepath.Glob(filepath.Join("../../../test/fixtures/spectra", "*", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures under %s (err %v)", "../../../test/fixtures/spectra", err)
	}
	for _, path := range files {
		hash := filepath.Base(filepath.Dir(path))
		name := filepath.Base(path)
		if e, ok := manifest[hash]; !ok || !e.Covers(spectra.SourceName) {
			t.Errorf("%s: hash %s is not audited for %s", name, hash, spectra.SourceName)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var f struct {
			Topics    []string `json:"topics"`
			Value     string   `json:"value"`
			WasmHash  string   `json:"wasm_hash"`
			EventName string   `json:"event_name"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if f.WasmHash != hash || !strings.HasPrefix(name, f.EventName+"_") || len(f.Topics) == 0 {
			t.Errorf("%s: wasm_hash/event_name/topics inconsistent with its path", name)
			continue
		}
		if f.Topics[0] != scval.MustEncodeSymbol(f.EventName) {
			t.Errorf("%s: topic[0] is not the %q symbol", name, f.EventName)
		}
		for _, s := range append([]string{f.Value}, f.Topics...) {
			if _, err := base64.StdEncoding.DecodeString(s); err != nil {
				t.Errorf("%s: bad base64: %v", name, err)
			}
		}
	}
}
