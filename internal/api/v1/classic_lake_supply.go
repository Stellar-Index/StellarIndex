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

// A classic asset's supply sits in trustlines, claimable balances, liquidity-pool reserves and SAC
// balances held by contracts; the trustline-only map ([Server.cachedClassicSupply]) misses the rest
// and `stellar.ledger_entries_current` cannot be widened to fill them. The lake's
// `stellar.supply_flows` (i128) gives Σmint − Σburn − Σclawback over the SAC instead.
//
// The trustline sum is a provable lower bound on issued supply, so [higherClassicSupply] keeps it
// when flows come in lower (incompletely seeded): the lake can only raise the served number.

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

	// classicLakeSupplyBatch caps contracts summed per refresh; read cost scales with flow count (millions for a mature token), so
	// list length is the only handle.
	classicLakeSupplyBatch = 32

	// classicLakeSupplyColdWait bounds how long a request blocks when nothing it asked about is
	// cached yet. A request that times out still leaves the detached refresh running.
	classicLakeSupplyColdWait = 750 * time.Millisecond

	// preciseSupplyMaxAge bounds how old a supply observation may be and still outrank the lake arms: too loose keeps a dead
	// observer's figure, too tight falls to the over-counting lake arm ([classicSupplyReading]).
	preciseSupplyMaxAge = 6 * time.Hour

	// classicLakeSupplyPrewarmGap spaces prewarm batches (ClickHouse spacing only); the prewarm is one serial driver that abandons
	// the pass on the first empty read ([Server.warmClassicLakeSupply]).
	classicLakeSupplyPrewarmGap = 2 * time.Second
)

// classicLakeSupplyReader is the bulk capability type-asserted off [Server.tokenSupply] (not added to [TokenSupplyReader]) so
// existing stubs opt out; production wires *clickhouse.SupplyReader.
type classicLakeSupplyReader interface {
	TokenSupplyForContracts(ctx context.Context, contractIDs []string) (map[string]clickhouse.TokenSupply, error)
}

// lakeSupplyEntry is one asset's cached lake-flows reading. An empty value is a negative cache
// ("asked, and the lake had no usable answer"), so unanswerable assets are not re-queried per request.
type lakeSupplyEntry struct {
	value string
	at    time.Time
}

// classicSACContractID resolves a row's asset_id to its supply_flows contract, CLASSIC assets only: native XLM is excluded (the
// XLM SAC supply is the wrapped amount, not total_coins), and Soroban rows too (their supply uses the RWA arm; admitting them
// would publish caps for every SEP-41 token).
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

// higherClassicSupply returns the larger raw supply (`lake` on a tie) with its basis. A lake figure below the trustline sum is
// incomplete seeding, so the trustline sum wins; otherwise the lake wins (it sees all four holding domains, the trustline sum
// is a lower bound). Unparseable counts as absent, not zero.
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

// classicSupplyReading resolves a row's circulating supply and basis; the one place the preference order lives, so a floor and a four-domain
// total never share a field name. Order: observation, lake flows, trustline sum, never below the trustline floor. Do not invert: the lake
// arm over-counts when replayed mints lack burns ([supply.BasisClassicLakeFlows]). The observation is bounded by [preciseSupplyMaxAge].
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

// stampCirculatingSupply publishes `circ` with its basis; nil-guarded so a supply another branch attached deliberately
// (dust-suppressed, ticker-collision) is not overwritten.
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

// classicLakeSupply returns asset_id to circulating supply from lake mint/burn flows for the rows it can answer. Best-effort: no reader, an
// error or a cold cache gives a smaller map and the caller falls back to the trustline sum. The refresh is detached: a sum outgrowing the
// request timeout must not die on a caller's context before recording the attempt, or the cache latches into failure.
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

// readClassicLakeSupply serves the cache for `wanted`, kicks a detached refresh for the rest, and returns the map, missing ids
// and the in-flight completion channel (nil when none).
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

// liveLakeSupplyLocked reports one asset's cached reading and whether it is live; an empty value is live and negative, hence
// the second boolean. Caller holds lakeSupplyMu. Request path and prewarm share it so the prewarm never refills warm slots or
// skips cold ones.
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

// classicLakeSupplyBatchOf takes the first [classicLakeSupplyBatch] of `missing`; one place, as batch size is the only handle
// on a supply_flows sum's cost.
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

// refreshClassicLakeSupply sums one batch on a detached context and folds it into the cache. Every asset in the batch gets an
// entry: absent, nil total or Incomplete (Σ(burn+clawback) > Σmint, i.e. incompletely seeded flows) is cached empty until the
// TTL lapses. A read error caches nothing: the lake is unavailable, which says nothing about the assets.
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

// rwaClassicPrewarmSet asks the RWA surface which classic assets it serves, so a member on no ranked page is still warmed. It
// goes through [Server.cachedRWAMembership] (stale set served during a single-flighted rebuild; a never-built cache waits,
// bounded by ctx) with the request path's own reduction.
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

// classicLakeSupplyPrewarmSet reduces the listing's answer for `opts` with the request path's own derivation
// ([Server.listAssetsExtAt], [classicLakeSupplyCandidates]), so the prewarm cannot warm a phantom slot while requests pay the
// cold fill. The overfetch asset is warmed too.
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

// warmClassicLakeSupply fills `wanted` one bounded batch at a time, serially (a supply_flows sum scales with flow count). It
// stops on the first batch that made no progress: a successful refresh writes an entry per asset, so a missing set that did not
// shrink means the lake read failed; the next sweep retries.
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

// warmClassicLakeSupplyBatch starts or joins one refresh and waits; returns how many of `wanted` are uncached and whether to
// continue. The retry gap is advanced but not consulted ([classicLakeSupplyPrewarmGap]), so a request-path refresh cannot start
// the instant a batch lands.
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
