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
// asset's supply sits in FOUR places: trustlines, claimable balances, liquidity-pool reserves and
// SAC balances held by CONTRACT holders. `stellar.ledger_entries_current` fills `asset` for
// trustlines ONLY.
//
// A pool row holds two assets and a SAC balance entry names its CONTRACT, never its asset, so
// widening the state query cannot work. The lake reads `stellar.supply_flows` instead (i128):
// Σmint − Σburn − Σclawback over the asset's SAC, as GET /v1/assets/{id}/supply does.
//
// The trustline sum is a PROVABLE LOWER BOUND on issued supply. A flows total BELOW it means
// incompletely seeded flows, so [higherClassicSupply] keeps the trustline figure and the lake
// can only ever raise the served number. cachedClassicSupply stops serving the map past
// classicSupplyMaxAge.

const (
	// classicLakeSupplyTTL bounds how long one asset's lake-flows reading is
	// reused. Supply moves on mints and burns, not on transfers, so it is
	// far more stable than a price; the TTL is long because the read is not.
	classicLakeSupplyTTL = 30 * time.Minute

	// classicLakeSupplyRetryGap rate-limits refresh ATTEMPTS, not refreshes.
	// Same lesson as classicSupplyRetryGap: once a heavy lake read starts
	// failing, retrying it per request is what turns one slow query into a
	// sustained outage.
	classicLakeSupplyRetryGap = 60 * time.Second

	// classicLakeSupplyBudget is the DETACHED refresh's own deadline. It
	// must exceed the API request timeout precisely because it does not run
	// on a request's context — that is the whole point of detaching.
	classicLakeSupplyBudget = 2 * time.Minute

	// classicLakeSupplyBatch caps how many contracts one refresh sums. Read cost scales with
	// the FLOW COUNT of the contracts, and a mature token carries millions of flows, so list
	// length is the only handle on it. A listing page of up to 500 assets is served a batch
	// at a time across requests.
	classicLakeSupplyBatch = 32

	// classicLakeSupplyColdWait bounds how long a request blocks when NOTHING it asked about
	// is cached yet. It is short and applies only to a fully cold answer; a request that times
	// out still leaves the detached refresh running, so the next one is served from cache.
	classicLakeSupplyColdWait = 750 * time.Millisecond

	// preciseSupplyMaxAge bounds how old a supply observation may be and still outrank the lake
	// arms. The widest observed gap between observations of any asset was 40 minutes, so six
	// hours survives restarts and slow passes while a stopped observer hands over within a
	// working morning.
	//
	// Too loose and a dead observer keeps publishing a figure nobody is computing; too tight and
	// a healthy asset falls to the lake arm, which over-counts (see [classicSupplyReading]).
	preciseSupplyMaxAge = 6 * time.Hour

	// classicLakeSupplyPrewarmGap spaces the background prewarm's batches. It is deliberately
	// NOT classicLakeSupplyRetryGap, which stops a per-REQUEST retry storm and is sized for
	// unbounded concurrent callers. The prewarm is one serial driver that abandons the pass on
	// the first empty read (see [Server.warmClassicLakeSupply]), so it cannot storm; it only
	// owes ClickHouse spacing between batches.
	classicLakeSupplyPrewarmGap = 2 * time.Second
)

// classicLakeSupplyReader is the narrow bulk capability this file needs from the token-supply
// reader. It is type-asserted off [Server.tokenSupply] rather than added to [TokenSupplyReader],
// so existing stubs keep compiling and opt out (the optional-seam idiom of
// [Server.latestPreciseSupply]). Production wiring is *clickhouse.SupplyReader.
type classicLakeSupplyReader interface {
	TokenSupplyForContracts(ctx context.Context, contractIDs []string) (map[string]clickhouse.TokenSupply, error)
}

// lakeSupplyEntry is one asset's cached lake-flows reading. An entry with an
// EMPTY value is a negative cache — "asked, and the lake had no usable answer"
// — and is as load-bearing as a positive one: without it, every asset the lake
// cannot answer for would be re-queried on every request forever.
type lakeSupplyEntry struct {
	value string
	at    time.Time
}

// classicSACContractID resolves a listing row's asset_id to the contract
// stellar.supply_flows is keyed by, for CLASSIC assets only.
//
// Native XLM is excluded deliberately: the XLM SAC's supply is the amount currently WRAPPED, a
// different quantity from the ledger header's total_coins (asset_supply.go excludes it too).
// Soroban contract rows are excluded because their supply already reaches the RWA contract arm
// via fillContractMarketCaps; admitting them would newly publish market caps for every SEP-41
// token on the listing.
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

// higherClassicSupply returns the larger of two raw decimal supply readings,
// preferring `lake` on a tie or when only one is present, together with the
// basis that produced the winner.
//
// A lake figure below the trustline sum is incomplete seeding, so the trustline sum wins; at or
// above it the lake wins because it includes supply the trustline sum cannot see. An
// unparseable reading is treated as absent, not zero.
//
// The basis is returned because the arms differ in KIND: the lake sum covers all four holding
// domains, the trustline sum is a LOWER BOUND. An empty basis means no reading at all.
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

// classicSupplyReading resolves one listing row's circulating supply and names
// the basis that produced it.
//
// It is the ONE place the listing path's preference order lives ([Server.fillRowMarketCap],
// [Server.rwaFillMissingSupply] call it); two copies could publish a floor and a four-domain
// total under the same field name.
//
// Order: the supply observation, then the lake-flows total, then the trustline sum, never below
// the trustline floor. Do not invert it: the lake arm over-counts when its flow history carries
// replayed mints whose burns are missing, which no completeness check can see (see
// [supply.BasisClassicLakeFlows]), while the observation matched Horizon.
//
// The observation's weakness is VINTAGE, so it is bounded by [preciseSupplyMaxAge]; a stale
// reading is not offered and so cannot outrank a live lake figure that disagrees with it.
func classicSupplyReading(
	assetID string, precise map[string]timescale.SupplyObservation, lake, broad map[string]string,
) (string, supply.Basis) {
	if obs, ok := precise[assetID]; ok && obs.CirculatingSupply != "" {
		// The observation publishes the basis the OBSERVER recorded, the same one
		// /v1/assets/{asset_id} publishes; a listing-only name would put two labels on one number.
		return obs.CirculatingSupply, supply.Basis(obs.Basis)
	}
	return higherClassicSupply(lake[assetID], broad[assetID])
}

// stampCirculatingSupply publishes `circ` on a listing row together with the
// basis that produced it.
//
// It is nil-guarded on CirculatingSupply: a row whose supply another branch attached
// deliberately (the dust-suppressed and ticker-collision paths) must not be overwritten. The
// basis travels WITH the value, so a row never carries one arm's figure and another's basis.
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

// classicLakeSupply returns asset_id → circulating supply derived from the
// lake's mint/burn flows, for whichever of `rows` it can answer for.
//
// Best-effort at every step: no reader, no bulk capability, a read error or a cold cache all
// yield a smaller map (or none), never a failed response. The caller falls back to the
// trustline sum.
//
// The refresh is DETACHED, as in refreshClassicSupply: a lake sum that outgrows the request
// timeout must not die on a caller's context before it records the attempt, or the cache would
// latch into permanent failure.
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

	// contextcheck: readClassicLakeSupply deliberately does NOT take the
	// caller's ctx — it may launch the detached refresh, which must outlive
	// this request. The waiting below is what honours ctx here.
	out, missing, flight := s.readClassicLakeSupply(rd, wanted) //nolint:contextcheck // the detachment is the point; see this function's doc.
	// Wait only on a FULLY cold answer — a warm cache never blocks a request,
	// and a partially warm one is already better than the fallback.
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

// classicLakeSupplyCandidates reduces a page of listing rows to the asset_id →
// SAC contract id map this file can act on, in a deterministic order.
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

// readClassicLakeSupply serves what the cache holds for `wanted`, and kicks a
// detached refresh for what it does not. Returns the served map, the asset ids
// still missing, and the in-flight refresh's completion channel (nil when no
// refresh is running).
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
	// G118 is the intended behaviour, not a defect: detaching from the
	// request context is what keeps a slow lake sum from being killed at the
	// request deadline and retried, unbounded, by the next caller.
	go s.refreshClassicLakeSupply(rd, classicLakeSupplyBatchOf(missing, wanted), flight) //nolint:gosec,contextcheck // G118 + contextcheck: the detachment is deliberate — see this function's doc.
	return out, missing, flight
}

// liveLakeSupplyLocked reports one asset's cached reading and whether that entry is still live.
// An EMPTY value is live and NEGATIVE ("asked, and the lake had no usable answer"), hence the
// second boolean instead of "" standing for absent. Caller holds lakeSupplyMu.
//
// The request path and the prewarm share this ONE notion of liveness, so the prewarm cannot
// refill slots the handler considers warm or skip ones it considers cold.
func (s *Server) liveLakeSupplyLocked(assetID string) (string, bool) {
	entry, cached := s.lakeSupply[assetID]
	if !cached || time.Since(entry.at) >= classicLakeSupplyTTL {
		return "", false
	}
	return entry.value, true
}

// missingLakeSupplyLocked returns the asset ids in `wanted` with no live cache
// entry, sorted so that concurrent callers cut identical batches out of it.
// Caller holds lakeSupplyMu.
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

// classicLakeSupplyBatchOf takes the first [classicLakeSupplyBatch] entries of
// `missing` as an asset_id → contract batch.
//
// One place, deliberately: batch size is the only handle on what a supply_flows sum costs, and a
// background driver cutting bigger batches than the request path would be a second load profile
// against the same table.
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

// refreshClassicLakeSupply sums one batch of contracts' lake flows on a
// DETACHED context and folds the result into the cache.
//
// Every asset in the batch gets an entry, including those with no usable answer: an asset that
// is absent, has a nil total, or is Incomplete (Σ(burn+clawback) > Σmint, meaning the flows are
// incompletely seeded, not that supply is negative) is cached empty so it is not re-queried
// until the TTL lapses. A read ERROR caches nothing, since it says the lake is unavailable, not
// anything about the assets.
func (s *Server) refreshClassicLakeSupply(rd classicLakeSupplyReader, batch map[string]string, done chan struct{}) {
	// Deferred so a panic in the lake sum cannot leave the single-flight
	// marker set — which would freeze the cache for the life of the process,
	// since a non-nil flight is never replaced.
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

// endClassicLakeSupplyFlight releases the single-flight marker and wakes the
// cold-start waiters. Deferred by refreshClassicLakeSupply so it runs on EVERY
// exit, panic included.
func (s *Server) endClassicLakeSupplyFlight(done chan struct{}) {
	s.lakeSupplyMu.Lock()
	s.lakeSupplyFlight = nil
	s.lakeSupplyMu.Unlock()
	close(done)
}

// PrewarmClassicLakeSupply fills the lake-flows supply cache out of band, so the
// supply /v1/assets and /v1/rwa/assets publish does not depend on how recently
// somebody looked.
//
// Request-path warming converges only under traffic; otherwise entries expire at
// [classicLakeSupplyTTL] and the listing falls back to the trustline-only sum. It runs in the
// API process because s.lakeSupply is per-process memory.
//
// Coverage is the pages `opts` names plus the /v1/rwa/assets membership, so it cannot drift from
// what each surface serves; "every classic asset" would be 450k+ SAC lookups. RWA members are
// chosen by attestation, not rank, so listing pages alone leave them cold.
//
// Best-effort: a missing reader or capability, or a listing or lake error, leaves the cache as
// it was.
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

// rwaClassicPrewarmSet asks the RWA surface which classic assets it serves, so
// a member that never appears on a ranked listing page is warmed anyway.
//
// It goes through [Server.cachedRWAMembership], so it behaves like a request: a stale set is
// served while a detached, single-flighted rebuild runs, and a never-built cache waits for the
// in-flight first build (bounded by ctx). Only a gapped-out build or an expired ctx yields no
// members, and then the sweep covers the listing alone.
//
// The reduction is the request path's own ([classicLakeSupplyCandidates]), so this cannot warm
// an asset the read path would not look up: native and contract-issued members drop out, and
// the contract arm has its own reader.
func (s *Server) rwaClassicPrewarmSet(ctx context.Context) map[string]string {
	m := s.cachedRWAMembership(ctx)
	if len(m.members) == 0 {
		return nil
	}
	// Only AssetID is read by the reduction — it derives the SAC from the
	// id itself — so nothing else is filled in, rather than filling fields
	// that would look load-bearing and are not.
	details := make([]AssetDetail, 0, len(m.members))
	for _, mem := range m.members {
		details = append(details, AssetDetail{AssetID: mem.code + "-" + mem.issuer})
	}
	return classicLakeSupplyCandidates(details)
}

// classicLakeSupplyPrewarmSet asks the listing which assets it serves for
// `opts` and reduces the answer with the REQUEST PATH's own candidate
// derivation.
//
// Reusing the request path's own pieces is the drift guard: rows from [Server.listAssetsExtAt],
// the asset_id → SAC map from [classicLakeSupplyCandidates], and the projection
// [assetDetailFromAssetRow]. Nothing about the population or cache key is restated, so the
// prewarm cannot warm a phantom slot while real requests still pay the cold fill.
//
// The (Limit+1)th overfetch asset is warmed too: it is the first row of the next page and rides
// a batch that ran anyway.
func (s *Server) classicLakeSupplyPrewarmSet(
	ctx context.Context, opts []timescale.ListAssetsOptions,
) map[string]string {
	wanted := make(map[string]string)
	for _, o := range opts {
		// Checked per shape, not just per batch: a cold boot pass reads a
		// dozen listing pages before it cuts its first batch, and this
		// goroutine is tracked by the shutdown WaitGroup.
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

// warmClassicLakeSupply fills `wanted` one bounded batch at a time until every
// asset in it has a cache entry.
//
// Serial by construction (one batch in flight, then a pause) because a supply_flows sum scales
// with the FLOW COUNT of the contracts in it and a mature token carries millions of rows.
//
// It stops on the first batch that made no progress. That test is exact:
// [Server.refreshClassicLakeSupply] writes an entry for EVERY asset in its batch on success
// (empty where the lake had no usable answer) and nothing on a read error, so "the missing set
// did not shrink" means "the lake read failed". Stop and let the next sweep retry rather than
// walk the rest of the population into the same failure (see classicLakeSupplyRetryGap).
func (s *Server) warmClassicLakeSupply(
	ctx context.Context, rd classicLakeSupplyReader, wanted map[string]string,
) {
	// One iteration per asset is a ceiling no healthy pass comes near
	// (ceil(len/classicLakeSupplyBatch) is), and it is what stops a cache
	// expiring underneath a long pass from turning this into a spin.
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

// warmClassicLakeSupplyBatch starts (or joins) one refresh and waits for it.
// Returns how many of `wanted` are still uncached afterwards, and whether the
// pass should continue.
//
// The retry gap is not consulted (see [classicLakeSupplyPrewarmGap]) but is ADVANCED: a prewarm
// batch is an attempt on the same table, and letting the request path kick a competing refresh
// the instant it lands is the stampede the gap prevents.
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
		// Detached for the same reason the request path detaches, and
		// STARTED rather than called inline so the wait below can honour
		// ctx: a shutdown returns this goroutine at once and still lets the
		// refresh finish and record what it learned.
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
	// A flight this pass merely JOINED was filling whatever batch its own
	// caller chose, which may not overlap `wanted` at all — so no progress
	// there says nothing about the lake's health. Only a batch of our own
	// that came back with nothing cached is evidence of a failed read.
	return after, after < before || !mine
}
