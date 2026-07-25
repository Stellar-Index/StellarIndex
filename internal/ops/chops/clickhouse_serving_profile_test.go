// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ─── C6-015: the api_serving profile's `readonly` value ────────────
//
// ADR-0048 D4 provisions a dedicated ClickHouse user + settings profile
// for the API's public serving reads
// (configs/ansible/roles/archival-node/tasks/20-clickhouse-serving-profile.yml).
// That file shipped with <readonly>1</readonly>, which would have taken
// out every explorer/supply endpoint the moment
// stellarindex_clickhouse_serving_enabled flipped on:
//
//	readonly=1 — read-data queries only AND changing settings prohibited.
//	readonly=2 — read data AND change settings; INSERT/ALTER/DROP/CREATE
//	             still blocked, and `readonly` itself cannot be raised.
//
// The serving readers change settings on every query. Both constructors
// the API binary hands the serving credentials to open the connection
// with clickhouse.Settings{"max_execution_time": 30}, and clickhouse-go
// merges the connection's Settings into the settings packet of every
// query. Under readonly=1 ClickHouse answers "Cannot modify
// 'max_execution_time' setting in readonly mode. (READONLY)". The
// native Ping carries no settings, so this would not have failed at
// startup — it would have failed on the first served request.
//
// This lives in a Go test rather than a rule lint because the two facts
// that have to stay in agreement are one ansible file and one Go file,
// and neither `ansible --syntax-check` nor `go vet` can see across that
// seam. The Go half is asserted here too, so the pairing can't rot by
// someone deleting the connection Settings and leaving readonly=2
// unexplained, or (worse) re-tightening readonly=1 later because the
// Settings "looked unused".

func repoRoot(t *testing.T) string {
	t.Helper()
	// internal/ops/chops -> repo root
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	return root
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

const servingProfileTask = "configs/ansible/roles/archival-node/tasks/20-clickhouse-serving-profile.yml"

// TestServingProfileReadonlyAllowsQuerySettings is the assertion that
// matters: the profile must NOT be readonly=1 while the serving readers
// send per-query settings, and readonly=2 is the value that permits
// exactly that and nothing more.
func TestServingProfileReadonlyAllowsQuerySettings(t *testing.T) {
	task := readRepoFile(t, servingProfileTask)

	re := regexp.MustCompile(`<readonly>\s*([0-9]+)\s*</readonly>`)
	m := re.FindStringSubmatch(task)
	if m == nil {
		t.Fatalf("%s no longer sets <readonly> in the api_serving profile — "+
			"an unset readonly is readonly=0, i.e. this serving user may write", servingProfileTask)
	}
	switch m[1] {
	case "2":
		// correct
	case "1":
		t.Fatalf("api_serving profile has <readonly>1</readonly>: read-data-only AND "+
			"settings changes prohibited. The serving readers set max_execution_time on "+
			"every query, so ClickHouse rejects all of them with READONLY and every "+
			"explorer/supply endpoint 500s once serving is enabled. Want 2 (read + "+
			"change settings; INSERT/ALTER/DROP still blocked). See %s.", servingProfileTask)
	default:
		t.Fatalf("api_serving profile has <readonly>%s</readonly>; want 2 "+
			"(0 lets this serving user write)", m[1])
	}
}

// TestServingReadersStillSendQuerySettings pins the other half of the
// pairing. If both serving constructors ever stop sending connection
// Settings, the readonly=2 justification above weakens and this test
// says so at the point of change — rather than the profile silently
// keeping a looser permission than it needs.
func TestServingReadersStillSendQuerySettings(t *testing.T) {
	for _, tc := range []struct{ file, ctor string }{
		{"internal/storage/clickhouse/explorer_reader.go", "func NewExplorerReaderAuth("},
		{"internal/storage/clickhouse/supply_flows.go", "func NewSupplyReaderAuth("},
	} {
		src := readRepoFile(t, tc.file)
		i := strings.Index(src, tc.ctor)
		if i < 0 {
			t.Errorf("%s: %s not found — the ADR-0048 D4 serving constructor was renamed or removed; "+
				"re-derive the api_serving profile's readonly value against its replacement (%s)",
				tc.file, tc.ctor, servingProfileTask)
			continue
		}
		body := src[i:]
		if end := strings.Index(body, "\n}\n"); end > 0 {
			body = body[:end]
		}
		if !strings.Contains(body, "clickhouse.Settings{") {
			t.Errorf("%s: %s no longer passes clickhouse.Settings — if the serving path "+
				"genuinely stopped changing per-query settings, the api_serving profile in %s "+
				"can be tightened back to readonly=1; do that deliberately, in the same change",
				tc.file, tc.ctor, servingProfileTask)
		}
	}
}
