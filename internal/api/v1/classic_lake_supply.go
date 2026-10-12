// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"math/big"
	"sort"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// The classic supply map ([Server.cachedClassicSupply]) sums trustline balances, but a classic
// asset's supply sits in four places: trustlines, claimable balances, liquidity-pool reserves and
// SAC balances held by contracts. `stellar.ledger_entries_current` fills `asset` for trustlines
// only, and a pool row holds two assets while a SAC balance entry names its contract, so widening
// the state query cannot work. The lake reads `stellar.supply_flows` instead (i128): Σmint −
// Σburn − Σclawback over the asset's SAC, as GET /v1/assets/{id}/supply does.
//
// The trustline sum is a provable lower bound on issued supply. A flows total below it means
// incompletely seeded flows, so [higherClassicSupply] keeps the trustline figure and the lake can
// only ever raise the served number.

const (
	// classicLakeSupplyTTL bounds how long one asset's lake-flows reading is reused. Supply moves
	// on mints and burns, not transfers, so it is stable; the read is slow, so the TTL is long.
	classicLakeSupplyTTL = 30 * time.Minute

	// classicLakeSupplyRetryGap rate-limits refresh attempts, not refreshes: once a heavy lake read
	// starts failing, retrying per request turns one slow query into a sustained outage.
	classicLakeSupplyRetryGap = 60 * time.Second

	// classicLakeSupplyBudget is the detached refresh's own deadline; it exceeds the API request
	// timeout because it does not run on a request's context.
	classicLakeSupplyBudget = 2 * time.Minute

	// classicLakeSupplyBatch caps how many contracts one refresh sums. Read cost scales with the
	// flow count (millions for a mature token), so list length is the only handle on it. A listing
	// page of up to 500 assets is served a batch at a time across requests.
	classicLakeSupplyBatch = 32

	// classicLakeSupplyColdWait bounds how long a request blocks when nothing it asked about is
	// cached yet. A request that times out still leaves the detached refresh running.
	classicLakeSupplyColdWait = 750 * time.Millisecond

	// preciseSupplyMaxAge bounds how old a supply observation may be and still outrank the lake
	// arms. Too loose and a dead observer keeps publishing a figure nobody is computing; too tight
	// and a healthy asset falls to the lake arm, which over-counts (see [classicSupplyReading]).
	preciseSupplyMaxAge = 6 * time.Hour

	// classicLakeSupplyPrewarmGap spaces the background prewarm's batches. It is not the retry gap,
	// which stops a per-request retry storm: the prewarm is one serial driver that abandons the
	// pass on the first empty read (see [Server.warmClassicLakeSupply]), so it only owes
	// ClickHouse spacing between batches.
	classicLakeSupplyPrewarmGap = 2 * time.Second
)

// classicLakeSupplyReader is the narrow bulk capability this file needs from the token-supply
// reader. It is type-asserted off [Server.tokenSupply] rather than added to [TokenSupplyReader],
// so existing stubs keep compiling and opt out. Production wiring is *clickhouse.SupplyReader.
type classicLakeSupplyReader interface {
	TokenSupplyForContracts(ctx context.Context, contractIDs []string) (map[string]clickhouse.TokenSupply, error)
}

// lakeSupplyEntry is one asset's cached lake-flows reading. An empty value is a negative cache
// ("asked, and the lake had no usable answer"), so unanswerable assets are not re-queried per request.
type lakeSupplyEntry struct {
	value string
	at    time.Time
}

// classicSACContractID resolves a listing row's asset_id to the contract stellar.supply_flows is
// keyed by, for CLASSIC assets only.
//
// Native XLM is excluded: the XLM SAC's supply is the amount currently wrapped, a different
// quantity from the ledger header's total_coins. Soroban contract rows are excluded because their
// supply already reaches the RWA contract arm; admitting them would newly publish market caps for
// every SEP-41 token on the listing.
func classicSACContractID(assetID string) (string, bool) {
	asset, err := canonical.ParseAsset(assetID)
	if err != nil || asset.Type != canonical.AssetClassic {
		return "", false
	}
	sac, err := asset.SacContractID()
	if err != nil {
		return "", false
	}
	return sac, true
}

// higherClassicSupply returns the larger of two raw decimal supply readings (`lake` on a tie) with
// the basis that produced it.
//
// A lake figure below the trustline sum is incomplete seeding, so the trustline sum wins; at or
// above it the lake wins because it includes supply the trustline sum cannot see. An unparseable
// reading is treated as absent, not zero. The basis is returned because the lake sum covers all
// four holding domains while the trustline sum is only a lower bound.
func higherClassicSupply(lake, trustline string) (string, supply.Basis) {
	l, lok := new(big.Int).SetString(lake, 10)
	t, tok := new(big.Int).SetString(trustline, 10)
	switch {
	case !lok && !tok:
		return "", ""
	case !lok:
		return trustline, supply.BasisClassicTrustlineSum
	case !tok:
		return lake, supply.BasisClassicLakeFlows
	case l.Cmp(t) < 0:
		return trustline, supply.BasisClassicTrustlineSum
	default:
		return lake, supply.BasisClassicLakeFlows
	}
}

// classicSupplyReading resolves one listing row's circulating supply and its basis.
//
// It is the one place the preference order lives; two copies could publish a floor and a
// four-domain total under the same field name.
//
// Order: the supply observation, then the lake-flows total, then the trustline sum, never below
// the trustline floor. Do not invert it: the lake arm over-counts when its flow history carries
// replayed mints whose burns are missing, which no completeness check can see (see
// [supply.BasisClassicLakeFlows]). The observation is bounded by [preciseSupplyMaxAge] so a stale
// reading cannot outrank a live lake figure that disagrees with it.
func classicSupplyReading(
	assetID string, precise map[string]timescale.SupplyObservation, lake, broad map[string]string,
) (string, supply.Basis) {
	if obs, ok := precise[assetID]; ok && obs.CirculatingSupply != "" {
		// Publishes the basis the observer recorded, as /v1/assets/{asset_id} does, so one number
		// never carries two labels.
		return obs.CirculatingSupply, supply.Basis(obs.Basis)
	}
	return higherClassicSupply(lake[assetID], broad[assetID])
}

// stampCirculatingSupply publishes `circ` on a listing row with its basis. It is nil-guarded so a
// supply another branch attached deliberately (dust-suppressed, ticker-collision) is not
// overwritten; the basis travels with the value, so a row never mixes arms.
func stampCirculatingSupply(row *AssetDetail, circ string, basis supply.Basis) {
	if row.CirculatingSupply != nil || circ == "" {
		return
	}
	c := circ
	row.CirculatingSupply = &c
	if basis != "" {
		b := basis.String()
		row.SupplyBasis = &b
	}
}

// classicLakeSupply returns asset_id → circulating supply from the lake's mint/burn flows, for
// whichever of `rows` it can answer for.
//
// Best-effort: no reader, a read error or a cold cache yield a smaller map, never a failed
// response; the caller falls back to the trustline sum. The refresh is detached: a lake sum that
// outgrows the request timeout must not die on a caller's context before it records the attempt,
// or the cache would latch into permanent failure.
func (s *Server) classicLakeSupply(ctx context.Context, rows []AssetDetail) map[string]string {
	if s.TokenSupply == nil {
		return nil
	}
	rd, ok := s.TokenSupply.(classicLakeSupplyReader)
	if !ok {
		return nil
	}
	wanted := classicLakeSupplyCandidates(rows)
	if len(wanted) == 0 {
		return nil
	}

	// readClassicLakeSupply takes no ctx: it may launch the detached refresh, which must outlive
	// this request. The waiting below honours ctx.
	out, missing, flight := s.readClassicLakeSupply(rd, wanted) //nolint:contextcheck // the detachment is the point; see this function's doc.
	// Wait only on a fully cold answer; a partially warm one already beats the fallback.
	if len(out) > 0 || len(missing) == 0 || flight == nil {
		return out
	}
	timer := time.NewTimer(classicLakeSupplyColdWait)
	defer timer.Stop()
	select {
	case <-flight:
		warmed, _, _ := s.readClassicLakeSupply(rd, wanted) //nolint:contextcheck // same detachment as above; the flight it could start is deliberate.
		return warmed
	case <-timer.C:
		return out
	case <-ctx.Done():
		return out
	}
}

// classicLakeSupplyCandidates reduces a page of listing rows to the asset_id → SAC contract id map
// this file can act on.
func classicLakeSupplyCandidates(rows []AssetDetail) map[string]string {
	wanted := make(map[string]string, len(rows))
	for i := range rows {
		if _, dup := wanted[rows[i].AssetID]; dup {
			continue
		}
		if contractID, ok := classicSACContractID(rows[i].AssetID); ok {
			wanted[rows[i].AssetID] = contractID
		}
	}
	return wanted
}

// readClassicLakeSupply serves what the cache holds for `wanted` and kicks a detached refresh for
// the rest. It returns the served map, the missing asset ids, and the in-flight refresh's
// completion channel (nil when none is running).
func (s *Server) readClassicLakeSupply(
	rd classicLakeSupplyReader, wanted map[string]string,
) (map[string]string, []string, chan struct{}) {
	s.lakeSupplyMu.Lock()
	defer s.lakeSupplyMu.Unlock()

	out := make(map[string]string, len(wanted))
	for assetID := range wanted {
		if value, live := s.liveLakeSupplyLocked(assetID); live && value != "" {
			out[assetID] = value
		}
	}
	missing := s.missingLakeSupplyLocked(wanted) // sorted: deterministic batching across concurrent requests
	if len(missing) == 0 {
		return out, nil, nil
	}

	if s.lakeSupplyFlight != nil {
		return out, missing, s.lakeSupplyFlight
	}
	if time.Since(s.lakeSupplyAttemptAt) < classicLakeSupplyRetryGap {
		return out, missing, nil
	}
	s.lakeSupplyAttemptAt = time.Now() // advances on failure too
	flight := make(chan struct{})
	s.lakeSupplyFlight = flight
	// G118 is intended: detaching keeps a slow lake sum from being killed at the request deadline
	// and retried, unbounded, by the next caller.
	go s.refreshClassicLakeSupply(rd, classicLakeSupplyBatchOf(missing, wanted), flight) //nolint:gosec,contextcheck // G118 + contextcheck: the detachment is deliberate — see this function's doc.
	return out, missing, flight
}

// liveLakeSupplyLocked reports one asset's cached reading and whether that entry is still live.
// An empty value is live and negative, hence the second boolean instead of "" meaning absent.
// Caller holds lakeSupplyMu. The request path and the prewarm share this notion of liveness, so
// the prewarm cannot refill slots the handler considers warm or skip ones it considers cold.
func (s *Server) liveLakeSupplyLocked(assetID string) (string, bool) {
	entry, cached := s.lakeSupply[assetID]
	if !cached || time.Since(entry.at) >= classicLakeSupplyTTL {
		return "", false
	}
	return entry.value, true
}

// missingLakeSupplyLocked returns the asset ids in `wanted` with no live cache entry, sorted so
// concurrent callers cut identical batches. Caller holds lakeSupplyMu.
func (s *Server) missingLakeSupplyLocked(wanted map[string]string) []string {
	missing := make([]string, 0, len(wanted))
	for assetID := range wanted {
		if _, live := s.liveLakeSupplyLocked(assetID); !live {
			missing = append(missing, assetID)
		}
	}
	sort.Strings(missing)
	return missing
}

// classicLakeSupplyBatchOf takes the first [classicLakeSupplyBatch] entries of `missing` as an
// asset_id → contract batch. One place, because batch size is the only handle on a supply_flows
// sum's cost.
func classicLakeSupplyBatchOf(missing []string, wanted map[string]string) map[string]string {
	batch := make(map[string]string, classicLakeSupplyBatch)
	for _, assetID := range missing {
		if len(batch) >= classicLakeSupplyBatch {
			break
		}
		batch[assetID] = wanted[assetID]
	}
	return batch
}

// refreshClassicLakeSupply sums one batch of contracts' lake flows on a detached context and folds
// the result into the cache.
//
// Every asset in the batch gets an entry: one that is absent, has a nil total, or is Incomplete
// (Σ(burn+clawback) > Σmint, meaning incompletely seeded flows, not negative supply) is cached
// empty until the TTL lapses. A read error caches nothing: it says the lake is unavailable, not
// anything about the assets.
func (s *Server) refreshClassicLakeSupply(rd classicLakeSupplyReader, batch map[string]string, done chan struct{}) {
	// Deferred so a panic cannot leave the single-flight marker set, which would freeze the cache
	// for the life of the process.
	defer s.endClassicLakeSupplyFlight(done)
	defer worker.Recover(s.logger, "api-classic-lake-supply-refresh")

	ctx, cancel := context.WithTimeout(context.Background(), classicLakeSupplyBudget)
	defer cancel()

	byContract := make(map[string]string, len(batch))
	ids := make([]string, 0, len(batch))
	for assetID, contractID := range batch {
		byContract[contractID] = assetID
		ids = append(ids, contractID)
	}
	sort.Strings(ids) // deterministic SQL for logs and tests

	sums, err := rd.TokenSupplyForContracts(ctx, ids)
	if err != nil {
		s.logger.Warn("classic lake-supply refresh failed (serving trustline sums)",
			"contracts", len(ids), "err", err)
		return
	}

	now := time.Now()
	s.lakeSupplyMu.Lock()
	defer s.lakeSupplyMu.Unlock()
	if s.lakeSupply == nil {
		s.lakeSupply = make(map[string]lakeSupplyEntry, len(batch))
	}
	for assetID, contractID := range batch {
		entry := lakeSupplyEntry{at: now}
		if sup, found := sums[contractID]; found && sup.Total != nil && !sup.Incomplete && sup.Total.Sign() > 0 {
			entry.value = sup.Total.String()
		}
		s.lakeSupply[assetID] = entry
	}
}

// endClassicLakeSupplyFlight releases the single-flight marker and wakes the cold-start waiters.
// It is deferred so it runs on EVERY exit, panic included.
func (s *Server) endClassicLakeSupplyFlight(done chan struct{}) {
	s.lakeSupplyMu.Lock()
	s.lakeSupplyFlight = nil
	s.lakeSupplyMu.Unlock()
	close(done)
}

// PrewarmClassicLakeSupply fills the lake-flows supply cache out of band in the API process (per-process memory) for the named pages plus /v1/rwa/assets members; best-effort.
func (s *Server) PrewarmClassicLakeSupply(ctx context.Context, opts []timescale.ListAssetsOptions) {
	if s.TokenSupply == nil || s.AssetsReader == nil {
		return
	}
	rd, ok := s.TokenSupply.(classicLakeSupplyReader)
	if !ok {
		return
	}
	wanted := s.classicLakeSupplyPrewarmSet(ctx, opts)
	for assetID, contractID := range s.rwaClassicPrewarmSet(ctx) {
		wanted[assetID] = contractID
	}
	for assetID, contractID := range s.stablecoinPrewarmSet() {
		wanted[assetID] = contractID
	}
	if len(wanted) == 0 {
		return
	}
	s.warmClassicLakeSupply(ctx, rd, wanted)
}

// rwaClassicPrewarmSet asks the RWA surface which classic assets it serves, so a member absent from
// every ranked listing page is warmed anyway.
//
// It goes through [Server.cachedRWAMembership] and so behaves like a request: a stale set is served
// during a detached, single-flighted rebuild, and a never-built cache waits for the first build
// (bounded by ctx). The reduction is the request path's own, so it cannot warm an asset the read
// path would not look up.
func (s *Server) rwaClassicPrewarmSet(ctx context.Context) map[string]string {
	m := s.cachedRWAMembership(ctx)
	if len(m.members) == 0 {
		return nil
	}
	// The reduction reads only AssetID (it derives the SAC from the id), so nothing else is set.
	details := make([]AssetDetail, 0, len(m.members))
	for _, mem := range m.members {
		details = append(details, AssetDetail{AssetID: mem.code + "-" + mem.issuer})
	}
	return classicLakeSupplyCandidates(details)
}

// classicLakeSupplyPrewarmSet asks the listing which assets it serves for `opts` and reduces the
// answer with the request path's own derivation ([Server.listAssetsExtAt],
// [classicLakeSupplyCandidates]), so the prewarm cannot warm a phantom slot while real requests
// still pay the cold fill. The (Limit+1)th overfetch asset is warmed too: it rides a batch that
// ran anyway.
func (s *Server) classicLakeSupplyPrewarmSet(
	ctx context.Context, opts []timescale.ListAssetsOptions,
) map[string]string {
	wanted := make(map[string]string)
	for _, o := range opts {
		// Checked per shape: a cold boot pass reads a dozen listing pages before its first batch,
		// and this goroutine is tracked by the shutdown WaitGroup.
		if ctx.Err() != nil {
			return wanted
		}
		rows, _, _, err := s.listAssetsExtAt(ctx, o)
		if err != nil {
			s.logger.Debug("classic lake-supply prewarm: listing read failed",
				"limit", o.Limit, "order", o.Order, "err", err)
			continue
		}
		details := make([]AssetDetail, 0, len(rows))
		for _, row := range rows {
			details = append(details, assetDetailFromAssetRow(row))
		}
		for assetID, contractID := range classicLakeSupplyCandidates(details) {
			wanted[assetID] = contractID
		}
	}
	return wanted
}

// warmClassicLakeSupply fills `wanted` one bounded batch at a time until every asset has an entry.
//
// Serial (one batch in flight, then a pause) because a supply_flows sum scales with the flow count
// of its contracts. It stops on the first batch that made no progress: the refresh writes an entry
// for every asset on success and nothing on a read error, so "the missing set did not shrink"
// means the lake read failed. The next sweep retries rather than walking into the same failure.
func (s *Server) warmClassicLakeSupply(
	ctx context.Context, rd classicLakeSupplyReader, wanted map[string]string,
) {
	// One iteration per asset is a ceiling no healthy pass nears (ceil(len/batch) is); it stops a
	// cache expiring under a long pass from turning this into a spin.
	for range len(wanted) {
		remaining, keepGoing := s.warmClassicLakeSupplyBatch(ctx, rd, wanted)
		if remaining == 0 || !keepGoing {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(classicLakeSupplyPrewarmGap):
		}
	}
}

// warmClassicLakeSupplyBatch starts (or joins) one refresh and waits for it. It returns how many of
// `wanted` are still uncached, and whether the pass should continue.
//
// The retry gap is not consulted (see [classicLakeSupplyPrewarmGap]) but is advanced, so a
// request-path refresh cannot start the instant a prewarm batch lands on the same table.
func (s *Server) warmClassicLakeSupplyBatch(
	ctx context.Context, rd classicLakeSupplyReader, wanted map[string]string,
) (int, bool) {
	s.lakeSupplyMu.Lock()
	missing := s.missingLakeSupplyLocked(wanted)
	if len(missing) == 0 {
		s.lakeSupplyMu.Unlock()
		return 0, false
	}
	before := len(missing)
	flight, mine := s.lakeSupplyFlight, false
	if flight == nil {
		flight = make(chan struct{})
		mine = true
		s.lakeSupplyAttemptAt = time.Now()
		s.lakeSupplyFlight = flight
		// Detached like the request path, and STARTED rather than called inline so the wait
		// below can honour ctx: shutdown returns at once and the refresh still records its result.
		go s.refreshClassicLakeSupply(rd, classicLakeSupplyBatchOf(missing, wanted), flight) //nolint:gosec,contextcheck // G118 + contextcheck: the detachment is deliberate — see refreshClassicLakeSupply's doc.
	}
	s.lakeSupplyMu.Unlock()

	select {
	case <-flight:
	case <-ctx.Done():
		return before, false
	}

	s.lakeSupplyMu.Lock()
	after := len(s.missingLakeSupplyLocked(wanted))
	s.lakeSupplyMu.Unlock()
	// A flight this pass merely joined filled its own caller's batch, which may not overlap
	// `wanted`; only our own batch coming back with nothing cached is evidence of a failed read.
	return after, after < before || !mine
}
