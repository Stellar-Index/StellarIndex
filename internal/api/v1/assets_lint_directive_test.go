package v1

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// TestSep1GocycloNolintPlacement guards RWC-OBS-3: the //nolint:gocyclo
// comment describing a "linear field-overlay sequence" must sit directly
// above applySep1VerifiedFields (the actual linear if-copy sequence it
// documents), not above sep1StatusForNoPayload (an unrelated short
// type-assert-and-branch helper).
func TestSep1GocycloNolintPlacement(t *testing.T) {
	f, err := os.Open("assets.go")
	if err != nil {
		t.Fatalf("open assets.go: %v", err)
	}
	defer f.Close()

	var prevNonBlank string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)

		switch {
		case strings.HasPrefix(trimmed, "func (s *Server) sep1StatusForNoPayload("):
			if strings.Contains(prevNonBlank, "nolint:gocyclo") {
				t.Errorf("sep1StatusForNoPayload should not carry the gocyclo nolint directive; " +
					"it belongs on applySep1VerifiedFields, the actual linear field-overlay sequence")
			}
		case strings.HasPrefix(trimmed, "func applySep1VerifiedFields("):
			if !strings.Contains(prevNonBlank, "nolint:gocyclo") {
				t.Errorf("applySep1VerifiedFields (the linear field-overlay sequence) must carry "+
					"the //nolint:gocyclo directive directly above it; got prior line: %q", prevNonBlank)
			}
		}

		if trimmed != "" {
			prevNonBlank = trimmed
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan assets.go: %v", err)
	}
}
