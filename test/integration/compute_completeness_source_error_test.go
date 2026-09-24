//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestComputeCompleteness_OneSourceErrorDoesNotWithholdTheRest drives the real
// compute-completeness subcommand on real TimescaleDB (#805, #1202 item 5).
//
// soroswap is the FIRST catalogue source. A trigger makes its verdict write
// fail, standing in for any per-source error (an RPC seed gap, a lake
// deadline, a served-floor read). The loop used to return at that first
// error, so no later source got a verdict: /v1/coverage kept serving every
// source's prior verdict while the run looked like one failed source. Now
// every other source is evaluated and published, soroswap publishes nothing,
// and the run still fails. -skip-recognition only because an empty lake is
// (rightly) refused as a vacuous recognition scan before the loop is reached.
func TestComputeCompleteness_OneSourceErrorDoesNotWithholdTheRest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfgPath := filepath.Join(t.TempDir(), "stellarindex.toml")
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf("[storage]\npostgres_dsn = %q\n", dsn)), 0o600); err != nil {
		t.Fatal(err)
	}

	const failSoroswap = `
CREATE FUNCTION fail_soroswap_verdict() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.source = 'soroswap' THEN
        RAISE EXCEPTION 'injected soroswap verdict failure';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER fail_soroswap_verdict BEFORE INSERT OR UPDATE ON completeness_snapshots
    FOR EACH ROW EXECUTE FUNCTION fail_soroswap_verdict();`
	if _, err := store.DB().ExecContext(ctx, failSoroswap); err != nil {
		t.Fatalf("install fault trigger: %v", err)
	}

	runErr := chops.Run([]string{"compute-completeness", "-config", cfgPath, "-to", "70000000", "-skip-recognition"})
	if runErr == nil || !strings.Contains(runErr.Error(), "soroswap") {
		t.Fatalf("run err = %v, want a non-nil error naming soroswap (a failed source must fail the run)", runErr)
	}

	published := map[string]bool{}
	rows, err := store.DB().QueryContext(ctx, `SELECT source FROM completeness_snapshots`)
	if err != nil {
		t.Fatalf("read snapshots: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		published[s] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if published["soroswap"] {
		t.Error("soroswap has a verdict row despite its write failing")
	}
	for _, src := range []string{"aquarius", "phoenix", "blend"} {
		if !published[src] {
			t.Errorf("%s has no verdict: soroswap's error withheld every later source's verdict (published: %v)", src, published)
		}
	}
}
