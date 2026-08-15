package v1_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
)

// fakeSchemaReader is a v1.SchemaVersionReader with a scriptable
// applied schema state, so the REC-06 checker can be exercised
// without a live Postgres.
type fakeSchemaReader struct {
	version uint
	dirty   bool
	err     error
}

func (f fakeSchemaReader) SchemaMigrationVersion(context.Context) (uint, bool, error) {
	return f.version, f.dirty, f.err
}

// TestSchemaVersionChecker_Ping pins the REC-06 assertion: the
// checker fails-closed when the applied schema head is BELOW what the
// binary was built against, or when schema_migrations is dirty, and
// passes only when the applied head is >= expected and clean. This is
// the property that lets /readyz drain a migrations-skipped or
// binary-swapped node instead of serving stale data behind a 200.
func TestSchemaVersionChecker_Ping(t *testing.T) {
	t.Parallel()
	exp := v1.ExpectedSchemaVersion

	tests := []struct {
		name       string
		reader     fakeSchemaReader
		wantErr    bool
		wantSubstr string
	}{
		{
			name:       "below expected head is a mismatch",
			reader:     fakeSchemaReader{version: exp - 1},
			wantErr:    true,
			wantSubstr: "schema/binary mismatch",
		},
		{
			name:       "dirty row is rejected even at the right version",
			reader:     fakeSchemaReader{version: exp, dirty: true},
			wantErr:    true,
			wantSubstr: "dirty",
		},
		{
			name:   "exact expected head passes",
			reader: fakeSchemaReader{version: exp},
		},
		{
			name:   "newer schema than the binary passes (rolling deploy / rollback)",
			reader: fakeSchemaReader{version: exp + 5},
		},
		{
			name:       "read error surfaces",
			reader:     fakeSchemaReader{err: errors.New("connection refused")},
			wantErr:    true,
			wantSubstr: "connection refused",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := v1.NewSchemaVersionChecker(tt.reader)
			if !c.Critical() {
				t.Fatal("schema checker must be Critical() so a mismatch drains the backend")
			}
			if got := c.Name(); got != "schema" {
				t.Fatalf("Name() = %q, want \"schema\"", got)
			}
			err := c.Ping(context.Background())
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Ping() = nil, want error containing %q", tt.wantSubstr)
				}
				if !strings.Contains(err.Error(), tt.wantSubstr) {
					t.Fatalf("Ping() error = %q, want substring %q", err.Error(), tt.wantSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Ping() = %v, want nil", err)
			}
		})
	}
}

// TestReadyz_SchemaMismatchDrainsBackend is the end-to-end gate
// demonstration: with the REC-06 critical checker wired into the
// readiness round reporting an applied head one below the binary's
// expectation, /v1/readyz returns 503 (unready) and names the schema
// failure — where before REC-06 there was NO schema check and the
// same node answered 200. Postgres itself is up (its stub passes), so
// the 503 is attributable solely to the schema/binary mismatch.
func TestReadyz_SchemaMismatchDrainsBackend(t *testing.T) {
	stale := fakeSchemaReader{version: v1.ExpectedSchemaVersion - 1}
	ts := newTestServer(t,
		&stubCheck{name: "postgres", critical: true},
		v1.NewSchemaVersionChecker(stale),
	)
	resp, err := http.Get(ts.URL + "/v1/readyz")
	if err != nil {
		t.Fatalf("GET /v1/readyz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (schema mismatch must drain the backend)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"status":"unready"`) {
		t.Errorf("body should report unready on schema mismatch: %s", body)
	}
	if !strings.Contains(body, "schema/binary mismatch") {
		t.Errorf("body should name the schema/binary mismatch: %s", body)
	}
}

// TestExpectedSchemaVersionMatchesMigrationsHead is the registry-parity
// guard: v1.ExpectedSchemaVersion MUST equal the highest-numbered
// migration under migrations/. Adding a migration without bumping the
// constant would silently weaken the REC-06 assertion (the binary would
// accept a schema older than it was built against), so this fails CI.
func TestExpectedSchemaVersionMatchesMigrationsHead(t *testing.T) {
	t.Parallel()

	root := findRepoRoot(t)
	ups, err := filepath.Glob(filepath.Join(root, "migrations", "*.up.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	if len(ups) == 0 {
		t.Fatal("no migrations found — repo root resolution must be broken")
	}

	prefix := regexp.MustCompile(`^(\d+)_`)
	var head uint
	for _, p := range ups {
		m := prefix.FindStringSubmatch(filepath.Base(p))
		if m == nil {
			t.Fatalf("migration file %q does not start with a NNNN_ prefix", filepath.Base(p))
		}
		n, err := strconv.ParseUint(m[1], 10, 64)
		if err != nil {
			t.Fatalf("parse migration number in %q: %v", filepath.Base(p), err)
		}
		if uint(n) > head {
			head = uint(n)
		}
	}

	if v1.ExpectedSchemaVersion != head {
		t.Fatalf("v1.ExpectedSchemaVersion = %d but migrations/ head is %d — bump ExpectedSchemaVersion in internal/api/v1/server.go when adding a migration", v1.ExpectedSchemaVersion, head)
	}
}

// findRepoRoot walks up from the test's working directory until it
// finds the go.mod that anchors the module, so the parity test locates
// migrations/ regardless of where `go test` is invoked.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate go.mod above the test directory")
		}
		dir = parent
	}
}
