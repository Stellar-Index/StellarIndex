//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestXLMBaseRestampChunks_RestampsInsideACompressedChunk is the DB-backed
// proof for `usd-volume-restamp -tier xlm-base -chunks`, driven through
// the real subcommand (flags, config, live-tail guard, generation) on real
// TimescaleDB, against a `trades` chunk that is COMPRESSED the way every
// chunk older than the 7-day policy is on production:
//
//  1. the DRY RUN (the default) prints the chunk plan and leaves the chunk
//     compressed and every row byte-identical;
//  2. -write restamps the rows through the same anchor the day walk uses
//     (10 XLM x $0.50 = $5.00), stamps them with the run's generation, and
//     leaves the chunk COMPRESSED again afterwards — with the trades
//     compression policy job (migration 0001) observed PAUSED while the
//     run is in flight and scheduled again after it;
//  3. a second -write run changes nothing: the chunk is probed read-only,
//     reported as skipped, never decompressed, and every row keeps its
//     value and generation;
//  4. with the run lock held from another session — the way a live run
//     holds it — -write refuses to start and touches nothing;
//  5. with the policy already unscheduled, -write refuses without
//     -resume-paused-policy and leaves the policy as it found it; with
//     the flag it restamps and re-enables the policy at exit;
//  6. with the policy removed, -write refuses to start and touches
//     nothing; a -generation in the future is refused the same way.
//
// The harness runs timescaledb.max_background_workers = 0, so the policy
// can never fire here; what this pins is that the real alter_job
// round-trips on the job the tool resolves, in both directions.
func TestXLMBaseRestampChunks_RestampsInsideACompressedChunk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const usdcID = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	token, err := c.NewSorobanAsset("CAFJZQWSED6YAWZU3GWRTOCNPPCGBN32L7QV43XX5LZLFTK6JLN34DLN")
	if err != nil {
		t.Fatal(err)
	}
	xlm := c.NativeAsset()
	xlmUSDC, err := c.NewPair(xlm, usdc)
	if err != nil {
		t.Fatal(err)
	}
	xlmToken, err := c.NewPair(xlm, token)
	if err != nil {
		t.Fatal(err)
	}
	// The insert path's wiring, so the seeded rows are what production
	// rows are.
	if err := timescale.InstallUSDVolumeResolution(store, []string{usdcID}, nil); err != nil {
		t.Fatalf("InstallUSDVolumeResolution: %v", err)
	}

	day := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	anchorTS := day.Add(10 * time.Hour)

	// XLM/USD anchor: 100 XLM for 50 USDC -> $0.50.
	anchor := mkIntegrationTrade("sdex", 1, anchorTS, xlmUSDC, 1_000_000_000, 500_000_000)
	if err := store.InsertTrade(ctx, anchor); err != nil {
		t.Fatalf("InsertTrade anchor: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`); err != nil {
		t.Fatalf("refresh prices_1m: %v", err)
	}

	// The XLM-base population: 10 XLM against a token with no USD market,
	// so the anchor's answer is exactly $5.00.
	const wantAnchored = "5.00000000"
	fixtures := []struct {
		name  string
		nonce int
		ts    time.Time
	}{
		{"quote-side wrong", 10, anchorTS.Add(5 * time.Minute)},
		{"stored NULL", 11, anchorTS.Add(6 * time.Minute)},
		{"already correct", 12, anchorTS.Add(7 * time.Minute)},
	}
	ledger := map[string]uint32{}
	var topLedger uint32
	for _, f := range fixtures {
		tr := mkIntegrationTrade("sdex", f.nonce, f.ts, xlmToken, 100_000_000, 300)
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade %s: %v", f.name, err)
		}
		ledger[f.name] = tr.Ledger
		if tr.Ledger > topLedger {
			topLedger = tr.Ledger
		}
	}
	exec := func(t *testing.T, q string, args ...any) {
		t.Helper()
		if _, err := store.DB().ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%q: %v", q, err)
		}
	}
	// The pre-fd1860bd state, imposed by hand.
	exec(t, `UPDATE trades SET usd_volume = 0.00372265 WHERE source='sdex' AND ledger=$1`, ledger["quote-side wrong"])
	exec(t, `UPDATE trades SET usd_volume = NULL       WHERE source='sdex' AND ledger=$1`, ledger["stored NULL"])

	type row struct {
		usd *string
		gen int64
	}
	readRow := func(t *testing.T, l uint32) row {
		t.Helper()
		var (
			usd sql.NullString
			gen int64
		)
		if err := store.DB().QueryRowContext(ctx,
			`SELECT usd_volume::text, derive_generation FROM trades WHERE source = 'sdex' AND ledger = $1`, l,
		).Scan(&usd, &gen); err != nil {
			t.Fatalf("read ledger %d: %v", l, err)
		}
		if !usd.Valid {
			return row{nil, gen}
		}
		return row{&usd.String, gen}
	}
	snapshot := func(t *testing.T) map[uint32]row {
		t.Helper()
		out := map[uint32]row{}
		for _, l := range ledger {
			out[l] = readRow(t, l)
		}
		out[anchor.Ledger] = readRow(t, anchor.Ledger)
		return out
	}
	sameSnapshot := func(t *testing.T, before, after map[uint32]row, what string) {
		t.Helper()
		for l, b := range before {
			a := after[l]
			switch {
			case (b.usd == nil) != (a.usd == nil):
				t.Errorf("%s: ledger %d usd_volume nullness changed (%v -> %v)", what, l, b.usd, a.usd)
			case b.usd != nil && *b.usd != *a.usd:
				t.Errorf("%s: ledger %d usd_volume %s -> %s", what, l, *b.usd, *a.usd)
			case b.gen != a.gen:
				t.Errorf("%s: ledger %d derive_generation %d -> %d", what, l, b.gen, a.gen)
			}
		}
	}
	chunkCompressed := func(t *testing.T) bool {
		t.Helper()
		var compressed bool
		if err := store.DB().QueryRowContext(ctx, `
			SELECT is_compressed FROM timescaledb_information.chunks
			 WHERE hypertable_name = 'trades' AND range_start <= $1 AND range_end > $1`, anchorTS,
		).Scan(&compressed); err != nil {
			t.Fatalf("read chunk state: %v", err)
		}
		return compressed
	}

	// ── compress the chunk, as the 7-day policy would have ────────────
	exec(t, `SELECT compress_chunk(c, true) FROM show_chunks('trades') c`)
	if !chunkCompressed(t) {
		t.Fatal("fixture: the day's chunk did not compress")
	}
	const policySQL = `SELECT scheduled FROM timescaledb_information.jobs WHERE proc_name = 'policy_compression' AND hypertable_name = 'trades'`
	policyScheduled := func(t *testing.T) bool {
		t.Helper()
		var scheduled bool
		if err := store.DB().QueryRowContext(ctx, policySQL).Scan(&scheduled); err != nil {
			t.Fatalf("read the trades compression policy: %v", err)
		}
		return scheduled
	}
	if !policyScheduled(t) {
		t.Fatal("fixture: the trades compression policy (migration 0001) is not scheduled before the run")
	}
	// The live tail is past the window, so the one-writer guard admits it.
	if err := store.UpsertCursor(ctx, "ledgerstream", "", topLedger+1_000); err != nil {
		t.Fatalf("UpsertCursor: %v", err)
	}

	cfgPath := filepath.Join(t.TempDir(), "stellarindex.toml")
	cfg := fmt.Sprintf("[storage]\npostgres_dsn = %q\n\n[trades]\nusd_pegged_classic_assets = [%q]\n", dsn, usdcID)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	const gen = "1756800000"
	args := []string{
		"usd-volume-restamp", "-config", cfgPath, "-tier", "xlm-base", "-chunks",
		"-from", "2026-06-10", "-to", "2026-06-10", "-fill-null",
		"-generation", gen,
		// The database's data directory is inside the container, so the
		// host cannot statfs it: the operator-override path.
		"-min-free-bytes", fmt.Sprint(int64(1) << 40),
	}

	// ── 1. dry run: plan printed, nothing decompressed, nothing written ─
	before := snapshot(t)
	out, err := captureStdout(t, func() error { return chops.Run(args) })
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	for _, want := range []string{
		"chunk plan: 1 trades chunk(s) intersect [2026-06-10, 2026-06-10] — 1 compressed, 0 not",
		"WARNING: trusting -min-free-bytes",
		"DRY RUN: would take session advisory lock hashtext('usd-volume-restamp:trades')",
		"then pause compression policy job ",
		"DRY RUN: nothing is decompressed",
		"would change 2 row(s)",
		"would restamp 2 row(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, out)
		}
	}
	sameSnapshot(t, before, snapshot(t), "dry run")
	if !chunkCompressed(t) {
		t.Fatal("the dry run decompressed the chunk")
	}
	if !policyScheduled(t) {
		t.Fatal("the dry run paused the compression policy")
	}

	// ── 2. -write: restamped through the anchor, chunk compressed again ─
	// A second session watches the policy job for the whole run: it must
	// be seen unscheduled while the run is in flight.
	stop := make(chan struct{})
	sawPaused := make(chan bool, 1)
	go func() {
		paused := false
		for {
			select {
			case <-stop:
				sawPaused <- paused
				return
			default:
			}
			var scheduled bool
			if err := store.DB().QueryRowContext(ctx, policySQL).Scan(&scheduled); err == nil && !scheduled {
				paused = true
			}
			time.Sleep(time.Millisecond)
		}
	}()
	out, err = captureStdout(t, func() error { return chops.Run(append(args, "-write")) })
	close(stop)
	if !<-sawPaused {
		t.Error("the trades compression policy was never observed PAUSED while the write run was in flight")
	}
	if !policyScheduled(t) {
		t.Error("the trades compression policy was not re-enabled after the write run")
	}
	if err != nil {
		t.Fatalf("write run: %v\n%s", err, out)
	}
	for _, want := range []string{
		"changed 2 row(s) (planned 2)",
		"bytes ",
		"restamped 2 row(s) in [2026-06-10, 2026-06-10] (tier xlm-base)",
		"CALL refresh_continuous_aggregate('prices_1m'",
		"CALL refresh_continuous_aggregate('twap_1d'",
		"acceptance: stellarindex-ops verify-usd-volume -config " + cfgPath + " -day 2026-06-10 -days 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("write-run output lacks %q:\n%s", want, out)
		}
	}
	if !chunkCompressed(t) {
		t.Fatal("the chunk was left DECOMPRESSED after a successful write run")
	}
	after := snapshot(t)
	for _, name := range []string{"quote-side wrong", "stored NULL"} {
		got := after[ledger[name]]
		if got.usd == nil || *got.usd != wantAnchored {
			t.Errorf("%s: usd_volume = %v, want %s", name, got.usd, wantAnchored)
		}
		if fmt.Sprint(got.gen) != gen {
			t.Errorf("%s: derive_generation = %d, want the run's %s", name, got.gen, gen)
		}
	}
	// The already-correct row and the exact-tier anchor row are untouched.
	for _, l := range []uint32{ledger["already correct"], anchor.Ledger} {
		if b, a := before[l], after[l]; b.gen != a.gen || *b.usd != *a.usd {
			t.Errorf("ledger %d moved: %+v -> %+v", l, b, a)
		}
	}

	// ── 3. the rerun: probed, skipped, nothing moves ──────────────────
	before = snapshot(t)
	out, err = captureStdout(t, func() error { return chops.Run(append(args, "-write")) })
	if err != nil {
		t.Fatalf("rerun: %v\n%s", err, out)
	}
	for _, want := range []string{
		"nothing to change — skipped, chunk left compressed",
		"restamped 0 row(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rerun output lacks %q:\n%s", want, out)
		}
	}
	sameSnapshot(t, before, snapshot(t), "rerun")
	if !chunkCompressed(t) {
		t.Fatal("the rerun left the chunk decompressed")
	}
	if !policyScheduled(t) {
		t.Fatal("the rerun left the compression policy paused")
	}

	// ── 4. a second attempt beside a live one: the run lock ───────────
	// run-heavy-job.sh's lock is per job NAME, so the wrapper lets a
	// second -write start; the session advisory lock does not. Held here
	// from a second connection, the way a live run holds it.
	exec(t, `UPDATE trades SET usd_volume = 0.00372265, derive_generation = 0 WHERE source='sdex' AND ledger=$1`, ledger["quote-side wrong"])
	exec(t, `SELECT compress_chunk(c, true) FROM show_chunks('trades') c`)
	holder, err := store.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var locked bool
	if err := holder.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtext($1::text))`, timescale.USDVolumeRestampLockName).Scan(&locked); err != nil || !locked {
		t.Fatalf("fixture: take the run lock from a second connection: locked=%v err=%v", locked, err)
	}
	before = snapshot(t)
	out, err = captureStdout(t, func() error { return chops.Run(append(args, "-write")) })
	if err == nil || !errors.Is(err, timescale.ErrUSDVolumeRestampLockHeld) || !strings.Contains(err.Error(), "refuses to start") {
		t.Fatalf("with the run lock held elsewhere: err = %v, want the held-lock refusal\n%s", err, out)
	}
	sameSnapshot(t, before, snapshot(t), "refused run (lock held)")
	if !chunkCompressed(t) {
		t.Fatal("the refused run decompressed the chunk")
	}
	if !policyScheduled(t) {
		t.Fatal("the refused run paused the compression policy")
	}
	var released bool
	if err := holder.QueryRowContext(ctx, `SELECT pg_advisory_unlock(hashtext($1::text))`, timescale.USDVolumeRestampLockName).Scan(&released); err != nil || !released {
		t.Fatalf("fixture: release the run lock: released=%v err=%v", released, err)
	}
	_ = holder.Close()

	// ── 5. a policy already unscheduled: refused without the flag ─────
	exec(t, `SELECT alter_job(job_id, scheduled => false) FROM timescaledb_information.jobs WHERE proc_name = 'policy_compression' AND hypertable_name = 'trades'`)
	if policyScheduled(t) {
		t.Fatal("fixture: the policy did not unschedule")
	}
	out, err = captureStdout(t, func() error { return chops.Run(append(args, "-write")) })
	if err == nil || !strings.Contains(err.Error(), "-resume-paused-policy") || !strings.Contains(err.Error(), "ALREADY unscheduled") {
		t.Fatalf("against an unscheduled policy: err = %v, want the refusal naming -resume-paused-policy\n%s", err, out)
	}
	sameSnapshot(t, before, snapshot(t), "refused run (policy unscheduled)")
	if !chunkCompressed(t) {
		t.Fatal("the refused run decompressed the chunk")
	}
	if policyScheduled(t) {
		t.Fatal("the refused run re-enabled the policy; that is the operator's call, or -resume-paused-policy's")
	}
	// With the flag: the run takes the paused policy over, restamps, and
	// re-enables it at exit.
	out, err = captureStdout(t, func() error { return chops.Run(append(args, "-resume-paused-policy", "-write")) })
	if err != nil {
		t.Fatalf("with -resume-paused-policy: %v\n%s", err, out)
	}
	if !strings.Contains(out, "changed 1 row(s) (planned 1)") {
		t.Errorf("-resume-paused-policy run output lacks the restamp:\n%s", out)
	}
	if !policyScheduled(t) {
		t.Fatal("-resume-paused-policy did not re-enable the policy at exit")
	}
	if !chunkCompressed(t) {
		t.Fatal("the -resume-paused-policy run left the chunk decompressed")
	}
	if got := readRow(t, ledger["quote-side wrong"]); got.usd == nil || *got.usd != wantAnchored || fmt.Sprint(got.gen) != gen {
		t.Errorf("quote-side wrong after the -resume-paused-policy run: usd=%v gen=%d", got.usd, got.gen)
	}

	// ── 6. refusals: no policy to pause; a generation in the future ───
	exec(t, `UPDATE trades SET usd_volume = 0.00372265, derive_generation = 0 WHERE source='sdex' AND ledger=$1`, ledger["quote-side wrong"])
	exec(t, `SELECT compress_chunk(c, true) FROM show_chunks('trades') c`)
	before = snapshot(t)
	exec(t, `SELECT remove_compression_policy('trades')`)
	out, err = captureStdout(t, func() error { return chops.Run(append(args, "-write")) })
	if err == nil || !strings.Contains(err.Error(), "no compression policy job on trades") || !strings.Contains(err.Error(), "refuses to start") {
		t.Fatalf("without a compression policy: err = %v, want a refusal naming it\n%s", err, out)
	}
	sameSnapshot(t, before, snapshot(t), "refused run (no policy)")
	if !chunkCompressed(t) {
		t.Fatal("the refused run decompressed the chunk")
	}
	exec(t, `SELECT add_compression_policy('trades', INTERVAL '7 days')`)
	if !policyScheduled(t) {
		t.Fatal("fixture: the re-added compression policy is not scheduled")
	}

	future := fmt.Sprint(time.Now().Add(24 * time.Hour).Unix())
	futureArgs := append([]string{}, args[:len(args)-4]...) // drop -generation and -min-free-bytes
	futureArgs = append(futureArgs, "-generation", future, "-min-free-bytes", fmt.Sprint(int64(1)<<40), "-write")
	out, err = captureStdout(t, func() error { return chops.Run(futureArgs) })
	if err == nil || !strings.Contains(err.Error(), "in the future") {
		t.Fatalf("-generation %s: err = %v, want the future refusal\n%s", future, err, out)
	}
	sameSnapshot(t, before, snapshot(t), "refused run (future generation)")
}

// captureStdout runs fn with os.Stdout redirected into a buffer. The ops
// subcommands print their plans and reports with fmt.Print*, which reads
// os.Stdout at call time.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()
	ferr := fn()
	os.Stdout = old
	_ = w.Close()
	<-done
	_ = r.Close()
	return buf.String(), ferr
}
