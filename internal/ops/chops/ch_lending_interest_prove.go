package chops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// lendingInterestProveJob names the proof's heartbeat textfile, which also carries its verdict gauges.
const lendingInterestProveJob = "ch-entry-history-lending-interest-prove"

// Seams for the proof's reads of the table it proves.
var (
	lendingInterestStored = clickhouse.LendingInterestStoredRows
	lendingInterestBeyond = clickhouse.LendingInterestRowsBeyond
)

const (
	metricInterestProofFailed = "stellarindex_lending_interest_proof_failed"
	metricInterestUnexplained = "stellarindex_lending_interest_unexplained_versions"
)

// interestProofCounts is one reserve's tally over the proved range.
type interestProofCounts struct{ proved, mismatched, incomplete int }

// interestProofList is a capped list of report lines with its full count.
type interestProofList struct {
	n     int
	lines []string
}

func (l *interestProofList) add(format string, a ...any) {
	l.n++
	if len(l.lines) < lendingProofListMax {
		l.lines = append(l.lines, fmt.Sprintf(format, a...))
	}
}

// interestProof proves stellar.lending_interest_hourly, read only. Per window of the proved range it re-reads the
// lake's versions with the writer's own coverage-proved read and folds them again (the same Fold: this catches a row
// written from other versions or by older arithmetic, not a defect in Fold itself), then holds the stored rows under
// FINAL against:
//   - the row set: every stored (pool, asset, hour) has a fresh row and the reverse; a stored row with none is stale;
//   - every stored column, byte-exact against the fresh fold, the event-reconciliation columns included;
//   - versions against ReserveVersionCounts: the lake's non-removed versions after Fold's own duplicate collapse,
//     independent of the pairing, so a version lost or doubled shows even where telescoping is blind;
//   - the writer's in-line identities (chain endpoints, hour set, telescoped gross, BLNT fee carry) over the fresh
//     rows and, where the row sets agree, over the stored values. TelescopedGross shares the ceil liability formula
//     with the accrual; what is independent is the pairing and the start value, which come from lending_state.
//
// A row of another derive revision is INCOMPLETE, not compared, as are the hours under a pending redo and a group whose
// seed the lake does not prove; rows past the watermark or a pool's coverage row are
// listed as unmarked, neither proved nor failed.
type interestProof struct {
	wm, start, last uint32
	uninitialised   bool
	keys            map[lendinginterest.ReserveKey]*interestProofCounts
	mismatches      interestProofList
	incomplete      interestProofList
	unmarked        interestProofList
	unexplained     int64
	checked         interestChecked // the identities over the stored values
	notChecked      int             // stored rows whose window's row set or revisions differ, so not re-checked
}

const (
	helpInterestProofFailed = "1 when the last lending interest proof found a stored row that disagrees with the lake, 0 otherwise."
	helpInterestUnexplained = "Σ unreconciled_versions − explained_versions over the rows the last lending interest proof read; attributed versions count."
)

// proveLendingInterest runs the proof holding -lock-file and the interest lock shared (proveLocks) and publishes its
// verdict gauges in its heartbeat textfile. Against a writer holding either past the wait the unit's fire skips,
// writing no heartbeat so the last finished proof's age still reaches proof_stale; a run by hand refuses.
func (r lendingRun) proveLendingInterest(ctx context.Context, out, log io.Writer) error {
	unlock, err := r.proveLocks(ctx)
	if err != nil {
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		if launchedByUnit() {
			_, _ = fmt.Fprintf(log, "%s: skipped, a lending_state or interest write run held its lock past %s; the next fire proves: %v\n",
				lendingInterestProveJob, proveLockWait, err)
			return nil
		}
		return fmt.Errorf("%s: a lending_state or interest write run held its lock past %s, so what the proof reads may change; "+
			"prove once it finishes: %w", lendingInterestProveJob, proveLockWait, err)
	}
	defer unlock()
	job := r.proveJob()
	hb := opsutil.NewJobHeartbeat(job, r.siblingHeartbeat(job), nil)
	// Until this run reaches a verdict the last one stands: a run that errors or reads no verdict clears no alert.
	failed, hasFailed := hb.LastGauge(metricInterestProofFailed)
	if hasFailed {
		hb.Gauge(metricInterestProofFailed, helpInterestProofFailed, failed)
	}
	if v, ok := hb.LastGauge(metricInterestUnexplained); ok {
		hb.Gauge(metricInterestUnexplained, helpInterestUnexplained, v)
	}
	hb.Start()
	pr := &interestProof{keys: map[lendinginterest.ReserveKey]*interestProofCounts{}}
	err = r.collectInterestProof(ctx, pr, hb, log)
	if err == nil {
		switch {
		case pr.mismatches.n > 0:
			hb.Gauge(metricInterestProofFailed, helpInterestProofFailed, 1)
		case !hasFailed || (pr.incomplete.n == 0 && !r.partialProof()):
			// Only the whole range compared in full clears a mismatch; INCOMPLETE hours or a -from, -to or
			// -interest-pool spot check may raise the verdict, never clear it.
			hb.Gauge(metricInterestProofFailed, helpInterestProofFailed, 0)
		}
		if !r.partialProof() {
			hb.Gauge(metricInterestUnexplained, helpInterestUnexplained, pr.unexplained)
		}
		err = pr.report(out)
	}
	hb.Stop(err == nil)
	return err
}

// proveJob names the proof's heartbeat: the daily -prove-recent run's, which proof_stale ages, or the full range's.
// Each holds its own verdict, so a recent range that proves never clears a mismatch only the full range reaches.
func (r lendingRun) proveJob() string {
	if r.interest.recent != 0 {
		return lendingInterestProveJob
	}
	return lendingInterestProveJob + "-full"
}

// partialProof is a proof narrowed by hand to part of its range.
func (r lendingRun) partialProof() bool { return r.from != 0 || r.to != 0 || r.interest.pool != "" }

// The proof's wait for a writer's lock: state passes and the timer's interest windows are short, so a fire waits one
// out rather than skipping the day.
const (
	proveLockRetry = 10 * time.Second
	proveLockWait  = 15 * time.Minute
)

// proveLockSleep is the wait between lock tries; a seam so tests never sleep.
var proveLockSleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// proveLocks holds -lock-file shared, so no lending_state run (exclusive) changes the state the proof folds, then the
// interest lock shared, so no interest write (exclusive) changes the rows it compares. The timer's interest pass meets
// the shared hold as it meets another interest run: a quiet skip.
func (r lendingRun) proveLocks(ctx context.Context) (func(), error) {
	unlockState, err := waitSharedLock(ctx, lendingJob, r.lockFile)
	if err != nil {
		return nil, err
	}
	unlockInterest, err := waitSharedLock(ctx, lendingInterestJob, filepath.Join(filepath.Dir(r.lockFile), interestLock))
	if err != nil {
		unlockState()
		return nil, err
	}
	return func() { unlockInterest(); unlockState() }, nil
}

// waitSharedLock takes path shared, trying again every proveLockRetry for up to proveLockWait while it is held
// exclusive; past that it returns the EWOULDBLOCK.
func waitSharedLock(ctx context.Context, job, path string) (func(), error) {
	for waited := time.Duration(0); ; waited += proveLockRetry {
		unlock, err := acquireSharedLock(job, path)
		if err == nil || !errors.Is(err, syscall.EWOULDBLOCK) || waited >= proveLockWait {
			return unlock, err
		}
		if err := proveLockSleep(ctx, proveLockRetry); err != nil {
			return nil, err
		}
	}
}

// launchedByUnit reports a run systemd started (the timer's unit): systemd sets INVOCATION_ID for every unit, as
// run-heavy-job.sh also reads it.
func launchedByUnit() bool { return os.Getenv("INVOCATION_ID") != "" }

// interestProofGroup is the pools proved through one last ledger.
type interestProofGroup struct {
	last  uint32
	pools []clickhouse.LendingContract
}

// collectInterestProof resolves the range and pools, then folds and compares each pool group's windows.
func (r lendingRun) collectInterestProof(ctx context.Context, pr *interestProof, hb *opsutil.JobHeartbeat, log io.Writer) error {
	wm, err := lendingInterestWatermark(ctx, r.chAddr)
	if err != nil {
		return err
	}
	if wm == 0 {
		pr.uninitialised = true
		return nil
	}
	pr.wm = wm
	if pr.start, pr.last, err = r.interestProofRange(ctx, wm); err != nil {
		return err
	}
	if err := r.clipToRedo(ctx, pr); err != nil {
		return err
	}
	reg, err := lendingInterestRegistry(ctx, r.chAddr, wm)
	if err != nil {
		return err
	}
	all := lendingPools(reg)
	groups, cuts, err := r.interestProofGroups(ctx, pr, all, wm)
	if err != nil {
		return err
	}
	for _, g := range groups {
		if g.last < pr.start {
			continue
		}
		if err := r.proveGroup(ctx, pr, g, hb, log); err != nil {
			return err
		}
	}
	return pr.beyond(ctx, r.chAddr, all, cuts, r.interest.pool)
}

// clipToRedo ends the proof before the hour a pending redo starts in: a lending_state replay rewrote the state from
// there, so those rows await a re-derive and are INCOMPLETE, not compared. Hours below it are still proved.
func (r lendingRun) clipToRedo(ctx context.Context, pr *interestProof) error {
	redo, pending, err := lendingInterestRedo(ctx, r.chAddr)
	if err != nil || !pending || redo.Next() > pr.last {
		return err
	}
	cut, err := hourStart(ctx, r.chAddr, max(redo.Next(), pr.start))
	if err != nil {
		return err
	}
	pr.incomplete.add("INCOMPLETE redo pending from ledger %d (re-derived through %d): ledgers [%d,%d] not compared; "+
		"re-derive them (-interest-only -from %d -write, through the tip), then prove again", redo.From, redo.Thru, cut, pr.last, redo.Next())
	pr.last = cut - 1
	return nil
}

// proveGroup folds and compares one pool group's windows. A seed the lake does not prove (or whose read timed out) leaves the group INCOMPLETE
// with the reason; any other refusal fails the run.
func (r lendingRun) proveGroup(ctx context.Context, pr *interestProof, g interestProofGroup, hb *opsutil.JobHeartbeat, log io.Writer) error {
	p := &interestPass{
		addr: r.chAddr, pools: g.pools, dryRun: true, log: log, hb: hb, proof: pr, maxVersions: r.interest.maxVersions,
		// The stored rows carry whatever unordered count the writer accepted; the proof compares it, never refuses it.
		opts: lendinginterest.Options{Protocols: map[string]lendinginterest.Protocol{}, MaxUnordered: math.MaxInt},
	}
	ids := make([]string, 0, len(g.pools))
	for _, c := range g.pools {
		p.opts.Protocols[c.ID] = lendinginterest.Protocol(c.Protocol)
		ids = append(ids, c.ID)
	}
	var err error
	if p.code, err = readLendingCode(ctx, r.chAddr, g.pools); err != nil {
		return err
	}
	err = p.run(ctx, pr.start, g.last)
	if errors.Is(err, clickhouse.ErrLendingInterestSeedUnproved) || errors.Is(err, clickhouse.ErrLendingInterestSeedProofTimeout) {
		pr.incomplete.add("INCOMPLETE pools %s [%d,%d]: not compared, the seed is not proved: %v", strings.Join(ids, ","), pr.start, g.last, err)
		return nil
	}
	return err
}

// interestProofRange is [-from, -to] snapped to whole hours, by default the floor to the watermark; -prove-recent N
// starts at the hour of watermark − N instead, never below the floor.
func (r lendingRun) interestProofRange(ctx context.Context, wm uint32) (uint32, uint32, error) {
	start, err := lendingInterestFloor(ctx, r.chAddr)
	if err != nil {
		return 0, 0, err
	}
	if n := uint64(r.interest.recent); n != 0 && uint64(wm) > n && uint64(wm)-n > uint64(start) {
		h, err := hourStart(ctx, r.chAddr, wm-uint32(n)) //nolint:gosec // n < wm, a uint32
		if err != nil {
			return 0, 0, err
		}
		start = max(start, h)
	}
	if r.from != 0 {
		if r.from < start || r.from > wm {
			return 0, 0, fmt.Errorf("lending interest: -from %d is outside the rows' range [%d (the floor), %d (the watermark)]", r.from, start, wm)
		}
		h, err := hourStart(ctx, r.chAddr, r.from)
		if err != nil {
			return 0, 0, err
		}
		start = max(start, h)
	}
	last := wm
	if r.to != 0 {
		if r.to < start {
			return 0, 0, fmt.Errorf("lending interest: -to %d is below the proof's first ledger %d", r.to, start)
		}
		h, err := lendingInterestHour(ctx, r.chAddr, r.to)
		if err != nil {
			return 0, 0, err
		}
		last = min(last, h.Last)
	}
	return start, last, nil
}

// interestProofGroups splits the proved pools by the ledger each is proved through: the range's end, clipped to a
// pool's coverage row when that stops below the watermark (which is INCOMPLETE). cuts is each covered pool's last
// proved hour; a stored row past it is unmarked.
func (r lendingRun) interestProofGroups(ctx context.Context, pr *interestProof, all []clickhouse.LendingContract, wm uint32,
) ([]interestProofGroup, map[string]time.Time, error) {
	pools := all
	if r.interest.pool != "" {
		pools = nil
		for _, c := range all {
			if c.ID == r.interest.pool {
				pools = append(pools, c)
			}
		}
		if len(pools) == 0 {
			return nil, nil, fmt.Errorf("lending interest: %s is not a pool in the registry through %d", r.interest.pool, wm)
		}
	}
	thru, cuts, err := r.interestCoverage(ctx, all, wm)
	if err != nil {
		return nil, nil, err
	}
	byLast := map[uint32][]clickhouse.LendingContract{}
	for _, c := range pools {
		if t := thru[c.ID]; t < wm {
			pr.incomplete.add("INCOMPLETE pool %s: its coverage row stops at %d (0 = none), below the watermark %d; rows past it are not proved",
				c.ID, t, wm)
			if t == 0 {
				continue
			}
		}
		last := min(pr.last, thru[c.ID])
		byLast[last] = append(byLast[last], c)
	}
	groups := make([]interestProofGroup, 0, len(byLast))
	for last, ps := range byLast {
		groups = append(groups, interestProofGroup{last: last, pools: ps})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].last < groups[j].last })
	return groups, cuts, nil
}

// interestCoverage is each pool's coverage (the watermark, or its own row when that stops below) and, for a pool
// with any, the hour of it: the last hour the proof reaches.
func (r lendingRun) interestCoverage(ctx context.Context, all []clickhouse.LendingContract, wm uint32,
) (map[string]uint32, map[string]time.Time, error) {
	gaps, err := lendingInterestUncovered(ctx, r.chAddr, all)
	if err != nil {
		return nil, nil, err
	}
	thru := map[string]uint32{}
	for _, c := range all {
		thru[c.ID] = wm
	}
	for _, g := range gaps {
		thru[g.Pool] = g.Thru
	}
	cuts := map[string]time.Time{}
	hours := map[uint32]time.Time{}
	for id, t := range thru {
		if t == 0 {
			continue
		}
		if _, ok := hours[t]; !ok {
			h, err := lendingInterestHour(ctx, r.chAddr, t)
			if err != nil {
				return nil, nil, err
			}
			hours[t] = h.Hour
		}
		cuts[id] = hours[t]
	}
	return thru, cuts, nil
}

// interestRowKey names a row by reserve and hour.
type interestRowKey struct {
	lendinginterest.ReserveKey
	hour int64
}

func interestKeyOf(r lendinginterest.HourRow) interestRowKey {
	return interestRowKey{lendinginterest.ReserveKey{Pool: r.Pool, Asset: r.Asset}, r.Hour.Unix()}
}

func (pr *interestProof) counts(k lendinginterest.ReserveKey) *interestProofCounts {
	c := pr.keys[k]
	if c == nil {
		c = &interestProofCounts{}
		pr.keys[k] = c
	}
	return c
}

// window compares one folded window's stored rows with the fresh fold.
func (pr *interestProof) window(ctx context.Context, p *interestPass, w foldedWindow) error {
	ids := make([]string, 0, len(p.pools))
	for _, c := range p.pools {
		ids = append(ids, c.ID)
	}
	stored, err := lendingInterestStored(ctx, p.addr, ids, w.from, w.to)
	if err != nil {
		return err
	}
	span := fmt.Sprintf("hours [%s, %s)", w.from.Format(time.RFC3339), w.to.Format(time.RFC3339))
	if _, err := checkLendingInterest(p.opts.Protocols, w.rows, w.state, w.versions, w.from, w.to); err != nil {
		pr.mismatches.add("mismatch %s: the lake's versions fail the in-line identities: %v", span, err)
	}
	fresh := make(map[interestRowKey]lendinginterest.HourRow, len(w.rows))
	for _, r := range w.rows {
		fresh[interestKeyOf(r)] = r
	}
	counts := lendinginterest.ReserveVersionCounts(w.versions)
	seen := map[interestRowKey]bool{}
	merged, whole := make([]lendinginterest.HourRow, 0, len(stored)), true
	for _, s := range stored {
		k := interestKeyOf(s.Row)
		seen[k] = true
		pr.unexplained += int64(s.Row.UnreconciledVersions) - int64(s.ExplainedVersions)
		f, ok := fresh[k]
		switch {
		case !ok:
			pr.rowMismatch(s.Row, "present", "none: no version of the reserve in the hour (a stale row)")
			whole = false
		case s.DeriveRev != lendingInterestRev:
			pr.counts(k.ReserveKey).incomplete++
			pr.incomplete.add("INCOMPLETE %s %s %s: derive_rev %d, this binary derives %d; re-derive the hour", s.Row.Pool, s.Row.Asset,
				s.Row.Hour.Format(time.RFC3339), s.DeriveRev, lendingInterestRev)
			whole = false
		default:
			if pr.compareRow(s, f, counts[k.ReserveKey][k.hour]) {
				pr.counts(k.ReserveKey).proved++
			}
			m := s.Row
			m.Baselines, m.ExcludedCreditDelta, m.Excluded = f.Baselines, f.ExcludedCreditDelta, f.Excluded
			m.FeeOnlyCreditDelta, m.FeeOnlyCarryDelta, m.FeeOnlyVersions, m.FeeOnlyUnknown = f.FeeOnlyCreditDelta, f.FeeOnlyCarryDelta, f.FeeOnlyVersions, f.FeeOnlyUnknown
			m.ResidualExplained, m.ResidualAttributed, m.ExplainedVersions, m.AttributedVersions = s.ResidualExplained, s.ResidualAttributed, s.ExplainedVersions, s.AttributedVersions
			m.ResidualUnexplained, m.UnexplainedVersions, m.ResidualLedgers = f.ResidualUnexplained, f.UnexplainedVersions, f.ResidualLedgers
			merged = append(merged, m)
		}
	}
	whole = pr.missingRows(w, counts, seen) && whole
	if !whole {
		pr.notChecked += len(merged)
		return nil
	}
	c, err := checkLendingInterest(p.opts.Protocols, merged, w.state, w.versions, w.from, w.to)
	if err != nil {
		pr.mismatches.add("mismatch %s: the stored values fail the in-line identities: %v", span, err)
	}
	pr.checked.telescoped += c.telescoped
	pr.checked.telescopeSplit += c.telescopeSplit
	pr.checked.excludedPairs += c.excludedPairs
	pr.checked.feeChecked += c.feeChecked
	pr.checked.feeSkipped += c.feeSkipped
	return nil
}

// missingRows reports every fresh row, and every hour the lake holds a version in, with no stored row; false when
// there was one.
func (pr *interestProof) missingRows(w foldedWindow, counts map[lendinginterest.ReserveKey]map[int64]uint32,
	seen map[interestRowKey]bool,
) bool {
	ok := true
	for _, r := range w.rows {
		if k := interestKeyOf(r); !seen[k] {
			seen[k] = true
			pr.rowMismatch(r, "none", fmt.Sprintf("present (versions %d)", r.Versions))
			ok = false
		}
	}
	for rk, hours := range counts {
		for h, n := range hours {
			k := interestRowKey{rk, h}
			t := time.Unix(h, 0).UTC()
			if seen[k] || t.Before(w.from) || !t.Before(w.to) {
				continue
			}
			pr.rowMismatch(lendinginterest.HourRow{Pool: rk.Pool, Asset: rk.Asset, Hour: t}, "none",
				fmt.Sprintf("none, though the lake holds %d versions", n))
			ok = false
		}
	}
	return ok
}

// rowMismatch records a row present on one side only.
func (pr *interestProof) rowMismatch(r lendinginterest.HourRow, stored, fresh string) {
	pr.counts(lendinginterest.ReserveKey{Pool: r.Pool, Asset: r.Asset}).mismatched++
	pr.mismatches.add("mismatch %s %s %s row: stored %s, recomputed %s", r.Pool, r.Asset, r.Hour.Format(time.RFC3339), stored, fresh)
}

// compareRow holds every stored column against the fresh fold and versions against the lake count; true when all agree.
func (pr *interestProof) compareRow(s clickhouse.LendingInterestStoredRow, f lendinginterest.HourRow, lakeVersions uint32) bool {
	got, want := interestColumns(s.Row), interestColumns(f)
	got = append(got, s.ResidualExplained.String(), s.ResidualAttributed.String(), fmt.Sprint(s.ExplainedVersions), fmt.Sprint(s.AttributedVersions))
	want = append(want, f.ResidualExplained.String(), f.ResidualAttributed.String(), fmt.Sprint(f.ExplainedVersions), fmt.Sprint(f.AttributedVersions))
	got, want = append(got, fmt.Sprint(s.Row.Versions)), append(want, fmt.Sprint(lakeVersions))
	ok := true
	for i, name := range interestColumnNames {
		if got[i] != want[i] {
			ok = false
			pr.mismatches.add("mismatch %s %s %s %s: stored %s, recomputed %s", s.Row.Pool, s.Row.Asset, s.Row.Hour.Format(time.RFC3339),
				name, got[i], want[i])
		}
	}
	if !ok {
		pr.counts(lendinginterest.ReserveKey{Pool: s.Row.Pool, Asset: s.Row.Asset}).mismatched++
	}
	return ok
}

// interestColumnNames are the compared columns, in interestColumns' order, then the event-reconciliation columns and
// the lake's version count.
var interestColumnNames = []string{
	"protocol", "first_ledger", "last_ledger", "versions", "accrued_versions", "gross", "protocol_fee", "backstop",
	"supplier_intended", "supplier_realised", "backstop_credit_delta", "unreconciled_versions", "brate_mismatches",
	"backstop_residual", "supply_residual", "fee_mismatches", "fee_credit_residual", "unordered_versions",
	"accrual_errors", "error_samples",
	"residual_explained", "residual_attributed", "explained_versions", "attributed_versions",
	"versions (the lake's count)",
}

// interestColumns renders a row's stored columns exactly.
func interestColumns(r lendinginterest.HourRow) []string {
	n := func(v uint32) string { return fmt.Sprint(v) }
	return []string{
		string(r.Protocol), n(r.FirstLedger), n(r.LastLedger), n(r.Versions), n(r.AccruedVersions), r.Gross.String(),
		r.ProtocolFee.String(), r.Backstop.String(), r.SupplierIntended.String(), r.SupplierRealised.String(),
		r.BackstopCreditDelta.String(), n(r.UnreconciledVersions), n(r.BRateMismatches), r.BackstopResidual.String(),
		r.SupplyResidual.String(), n(r.FeeMismatches), r.FeeCreditResidual.String(), n(r.UnorderedVersions),
		n(r.AccrualErrors), fmt.Sprintf("%q", r.ErrorSamples),
	}
}

// beyond lists the stored rows the proof did not reach: past the watermark or a pool's coverage row (unmarked), and
// any row of a contract the registry does not list as a pool (a mismatch at or below the watermark's hour).
func (pr *interestProof) beyond(ctx context.Context, addr string, all []clickhouse.LendingContract, cuts map[string]time.Time, only string) error {
	ids := make([]string, 0, len(all))
	registered := map[string]bool{}
	var after time.Time // the earliest cut
	for _, c := range all {
		ids = append(ids, c.ID)
		registered[c.ID] = true
		if cut, ok := cuts[c.ID]; ok && (after.IsZero() || cut.Before(after)) {
			after = cut
		}
	}
	// The watermark's own hour, not the latest cut: with no pool covered there is none.
	wmHour, err := lendingInterestHour(ctx, addr, pr.wm)
	if err != nil {
		return err
	}
	rows, err := lendingInterestBeyond(ctx, addr, ids, after)
	if err != nil {
		return err
	}
	for _, r := range rows {
		cut, covered := cuts[r.Pool]
		switch {
		case only != "" && r.Pool != only:
		case covered && !r.Hour.After(cut):
		case !registered[r.Pool] && !r.Hour.After(wmHour.Hour):
			pr.rowMismatch(lendinginterest.HourRow{Pool: r.Pool, Asset: r.Asset, Hour: r.Hour}, "present",
				"none: the contract is not a pool in the registry")
		default:
			pr.unmarked.add("unmarked %s %s %s (derive_rev %d): past the watermark or the pool's coverage row", r.Pool, r.Asset,
				r.Hour.Format(time.RFC3339), r.DeriveRev)
		}
	}
	return nil
}

// report prints one line per reserve, the listed mismatches, INCOMPLETE and unmarked rows, and the verdict; the
// error is the verdict's.
func (pr *interestProof) report(out io.Writer) error {
	if pr.uninitialised {
		_, err := fmt.Fprintln(out, "lending interest: not initialised")
		return err
	}
	keys := make([]lendinginterest.ReserveKey, 0, len(pr.keys))
	for k := range pr.keys {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Pool+keys[i].Asset < keys[j].Pool+keys[j].Asset })
	proved := 0
	for _, k := range keys {
		c := pr.keys[k]
		proved += c.proved
		if _, err := fmt.Fprintf(out, "lending_interest %s %s: %d hours proved, %d mismatched, %d INCOMPLETE\n", k.Pool, k.Asset,
			c.proved, c.mismatched, c.incomplete); err != nil {
			return err
		}
	}
	for _, l := range []interestProofList{pr.mismatches, pr.incomplete, pr.unmarked} {
		for _, s := range l.lines {
			if _, err := fmt.Fprintln(out, s); err != nil {
				return err
			}
		}
		if more := l.n - len(l.lines); more > 0 && len(l.lines) > 0 {
			if _, err := fmt.Fprintf(out, "… %d more\n", more); err != nil {
				return err
			}
		}
	}
	verdict := "PROVED"
	switch {
	case pr.mismatches.n > 0:
		verdict = "MISMATCH"
	case pr.incomplete.n > 0:
		verdict = "INCOMPLETE"
	}
	if _, err := fmt.Fprintf(out, "lending_interest [%d,%d] @%d: %s: %d hours proved, %d mismatches, %d INCOMPLETE, %d unmarked; "+
		"identities over the stored values: gross telescoped %d (%d around %d excluded pairs), fee carry %d (%d not), %d rows not re-checked; "+
		"unexplained versions %d\n", pr.start, pr.last, pr.wm, verdict, proved, pr.mismatches.n, pr.incomplete.n, pr.unmarked.n,
		pr.checked.telescoped, pr.checked.telescopeSplit, pr.checked.excludedPairs, pr.checked.feeChecked, pr.checked.feeSkipped, pr.notChecked, pr.unexplained); err != nil {
		return err
	}
	switch verdict {
	case "MISMATCH":
		return fmt.Errorf("%s: %d stored rows or windows disagree with the lake (listed above)", lendingInterestProveJob, pr.mismatches.n)
	case "INCOMPLETE":
		return fmt.Errorf("%s: INCOMPLETE: %d hours or pools not compared (listed above); the rest passed", lendingInterestProveJob, pr.incomplete.n)
	}
	return nil
}
