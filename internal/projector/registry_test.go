package projector

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
)

// TestBuildRegistry_FoldsWhitespaceAndCaseInSourceNames pins Q054: the
// registry's own name lookup must normalise ingestion.enabled_sources
// entries the SAME way internal/config/validate.go's KnownSources check
// (which trims + lowercases) and internal/pipeline.BuildDispatcher already
// do. Before this fix BuildRegistry only lowercased (no TrimSpace), so a
// name with leading/trailing whitespace — accepted by config.Validate —
// silently missed its projector entry (buildSource's default case returns
// ok=false with no error) instead of registering comet.
func TestBuildRegistry_FoldsWhitespaceAndCaseInSourceNames(t *testing.T) {
	reg, err := BuildRegistry([]string{"  Comet  "}, config.OracleConfig{}, nil, nil)
	if err != nil {
		t.Fatalf("BuildRegistry(%q): %v", "  Comet  ", err)
	}
	for _, s := range reg.Sources {
		if s.Name == comet.SourceName {
			return
		}
	}
	t.Fatalf("BuildRegistry(%q) did not register the comet source; got %d sources", "  Comet  ", len(reg.Sources))
}
