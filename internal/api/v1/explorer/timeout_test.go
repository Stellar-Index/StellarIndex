package explorer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Pins that every explorer handler reading the shared ClickHouse pool bounds the read in context.WithTimeout(
// explorerReadTimeout). The server WriteTimeout doesn't cancel an in-flight query, so a raw r.Context() would let a few
// slow unauthenticated requests hold every connection and wedge every lake-backed endpoint.

// deadlineProbe records the deadline on the FIRST reader call (all of a handler's reads share one
// context, and the first sees the full budget). A bounded handler yields a Deadline() about
// explorerReadTimeout out; an unbounded one passes r.Context(), which has none.
type deadlineProbe struct {
	mu      sync.Mutex
	sawCall bool
	hasDL   bool
	budget  time.Duration
}

func (p *deadlineProbe) record(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sawCall {
		return
	}
	p.sawCall = true
	if dl, ok := ctx.Deadline(); ok {
		p.hasDL = true
		p.budget = time.Until(dl)
	}
}

// capReader is a full ExplorerReader that records the context deadline on every
// call and returns harmless zero values, so a handler runs to (or past) its
// first lake read without needing real data.
type capReader struct{ probe *deadlineProbe }

func (r *capReader) Cap67MovementsWatermark(ctx context.Context) (uint32, error) {
	r.probe.record(ctx)
	return 0, nil
}

func (r *capReader) Cap67SupplyCoverage(ctx context.Context) (uint32, uint32, bool, error) {
	r.probe.record(ctx)
	return 0, 0, false, nil
}

func (r *capReader) AccountsStats(ctx context.Context) (clickhouse.AccountsStats, bool, error) {
	r.probe.record(ctx)
	return clickhouse.AccountsStats{}, false, nil
}

func (r *capReader) AccountCreators(ctx context.Context, _ int, _ string) (clickhouse.AccountCreators, bool, error) {
	r.probe.record(ctx)
	return clickhouse.AccountCreators{}, false, nil
}

func (r *capReader) AccountSponsors(ctx context.Context, _ int, _ string) (clickhouse.AccountSponsors, bool, error) {
	r.probe.record(ctx)
	return clickhouse.AccountSponsors{}, false, nil
}

func (r *capReader) AccountGraph(ctx context.Context, _, _ string, _ int, _ string) (clickhouse.AccountGraph, bool, error) {
	r.probe.record(ctx)
	return clickhouse.AccountGraph{}, false, nil
}

func (r *capReader) AccountGraphHistory(ctx context.Context, _ string) (clickhouse.AccountGraphHistory, bool, error) {
	r.probe.record(ctx)
	return clickhouse.AccountGraphHistory{}, false, nil
}

func (r *capReader) AccountCohort(ctx context.Context, _, _ string) (clickhouse.AccountCohort, bool, error) {
	r.probe.record(ctx)
	return clickhouse.AccountCohort{}, false, nil
}

func (r *capReader) ContractActivitySummaryFor(ctx context.Context, _ string, _ int) (clickhouse.ContractActivitySummary, bool, error) {
	r.probe.record(ctx)
	return clickhouse.ContractActivitySummary{}, false, nil
}

func (r *capReader) RecentLedgers(ctx context.Context, _ int, _ uint32) ([]clickhouse.LedgerHeader, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) LedgerBySeq(ctx context.Context, _ uint32) (clickhouse.LedgerHeader, bool, error) {
	r.probe.record(ctx)
	return clickhouse.LedgerHeader{}, false, nil
}

func (r *capReader) LedgerTransactions(ctx context.Context, _ uint32, _ int) ([]clickhouse.TxSummary, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) OperationsByLedger(ctx context.Context, _ uint32, _ int) ([]clickhouse.OpRow, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) RecentOperationsOfType(ctx context.Context, _ int, _ clickhouse.ExplorerCursor, _ []string) (clickhouse.OpTypePage, error) {
	r.probe.record(ctx)
	return clickhouse.OpTypePage{}, nil
}

func (r *capReader) RecentOperations(ctx context.Context, _ int, _ clickhouse.ExplorerCursor) ([]clickhouse.OpRow, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) OperationTypeStats(ctx context.Context, _ uint32) ([]clickhouse.OpTypeCount, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) AccountOperationTypeCounts(ctx context.Context, _ string) ([]clickhouse.OpTypeCount, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) NetworkThroughput(ctx context.Context, _ int) ([]clickhouse.ThroughputBucket, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) BlendPoolReserves(ctx context.Context, _ string, _ blend.PoolVersion, _ []string, _ map[string]blend.ReserveConfig) ([]clickhouse.BlendReserveState, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) TransactionByHash(ctx context.Context, _ string) (clickhouse.TxSummary, bool, error) {
	r.probe.record(ctx)
	return clickhouse.TxSummary{}, false, nil
}

func (r *capReader) OperationsByTx(ctx context.Context, _ uint32, _ string) ([]clickhouse.OpRow, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) OperationResultsByTx(ctx context.Context, _ uint32, _ string) (map[uint32]clickhouse.OpResult, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) TxOutcomesByHash(ctx context.Context, _ []uint32, _ []string) (map[string]clickhouse.TxOutcome, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) EventsByTx(ctx context.Context, _ uint32, _ string) ([]clickhouse.EventSummary, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) ContractEventsRecent(ctx context.Context, _ string, _ int, _ clickhouse.ContractEventsCursor) ([]clickhouse.ContractActivityRow, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) ContractWasm(ctx context.Context, _ string) (clickhouse.ContractWasmInfo, error) {
	r.probe.record(ctx)
	return clickhouse.ContractWasmInfo{}, clickhouse.ErrContractWasmUnresolved
}

func (r *capReader) ContractInstanceState(ctx context.Context, _ string) (clickhouse.ContractInstanceState, error) {
	r.probe.record(ctx)
	return clickhouse.ContractInstanceState{}, nil
}

func (r *capReader) RecentContracts(ctx context.Context, _ int, _ uint32) ([]clickhouse.ContractDirectoryRow, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) ContractInteractions(ctx context.Context, _ string, _ int, since uint32) ([]clickhouse.ContractEdgeRow, uint32, error) {
	r.probe.record(ctx)
	return nil, since, nil
}

func (r *capReader) ContractCodeHistory(ctx context.Context, _ string) ([]clickhouse.ContractCodeVersion, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) AccountTransactions(ctx context.Context, _ string, _ int, _ clickhouse.ExplorerCursor) ([]clickhouse.TxSummary, clickhouse.ExplorerCursor, error) {
	r.probe.record(ctx)
	return nil, clickhouse.ExplorerCursor{}, nil
}

func (r *capReader) AccountOperations(ctx context.Context, _ string, _ int, _ clickhouse.ExplorerCursor) ([]clickhouse.OpRow, clickhouse.ExplorerCursor, error) {
	r.probe.record(ctx)
	return nil, clickhouse.ExplorerCursor{}, nil
}

func (r *capReader) AccountState(ctx context.Context, _ string) (clickhouse.AccountState, error) {
	r.probe.record(ctx)
	return clickhouse.AccountState{}, nil
}

// AccountStateCached records the deadline like its uncached sibling, so
// the timeout-propagation probes still see this call.
func (r *capReader) AccountStateCached(ctx context.Context, _ string) (clickhouse.AccountState, bool, error) {
	r.probe.record(ctx)
	return clickhouse.AccountState{}, false, nil
}

func (r *capReader) AssetHolders(ctx context.Context, _ string, _ int) ([]clickhouse.AssetHolder, int64, error) {
	r.probe.record(ctx)
	return nil, 0, nil
}

func (r *capReader) AccountsByWealth(ctx context.Context, _, _ []string, _ int) ([]clickhouse.AccountWealth, error) {
	r.probe.record(ctx)
	return nil, nil
}

// AccountsByWealthCached records the deadline like its uncached sibling so
// the timeout-propagation probes still observe this call, and reports
// warm-and-fresh so handlers proceed down the normal path.
func (r *capReader) AccountsByWealthCached(ctx context.Context, _, _ []string, _ int) (clickhouse.AccountWealthSnapshot, bool) {
	r.probe.record(ctx)
	return clickhouse.AccountWealthSnapshot{Basis: clickhouse.WealthBasisUSD, AsOf: time.Now()}, true
}

func (r *capReader) SoroswapPairReserves(ctx context.Context, _ []string) (map[string]clickhouse.SoroswapPairState, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) NativeLiquidityPoolReserves(ctx context.Context, _ []string) (map[string]clickhouse.NativeLiquidityPoolState, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) NativeLiquidityPoolsRanked(ctx context.Context, _ int) ([]clickhouse.NativeLiquidityPoolState, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) TokenDisplays(ctx context.Context, _ []string) (map[string]clickhouse.TokenDisplayMeta, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) SACClassicAssetName(ctx context.Context, _ string) (string, bool, error) {
	r.probe.record(ctx)
	return "", false, nil
}

func (r *capReader) SACAssetFromEvents(ctx context.Context, _ string) (string, bool, error) {
	r.probe.record(ctx)
	return "", false, nil
}

func (r *capReader) AccountsUnspendable(ctx context.Context, _ []string) (map[string]bool, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) AccountMovements(ctx context.Context, _ string, _ int, _ clickhouse.AccountMovementCursor, _ clickhouse.AccountMovementFilter) ([]clickhouse.AccountMovementRow, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) AssetMovements(ctx context.Context, _ string, _ int, _ clickhouse.AccountMovementCursor, _ uint32) ([]clickhouse.AssetMovementRow, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) AssetMovementsBackfilledThru(ctx context.Context) (uint32, error) {
	r.probe.record(ctx)
	return 0, nil
}

func (r *capReader) EntryHistoryWatermark(ctx context.Context) (uint32, error) {
	r.probe.record(ctx)
	return 1, nil
}

func (r *capReader) EntryIndexWatermark(ctx context.Context) (uint32, error) {
	r.probe.record(ctx)
	return 1, nil
}

func (r *capReader) AccountEntryLedgers(ctx context.Context, _ string, _ []string, _ uint32, _ int) ([]uint32, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) AssetTrustlineDaily(ctx context.Context, _ string, _ uint32) ([]clickhouse.AssetTrustlineDay, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) LiveClaimableBalancesInLedgers(ctx context.Context, _ string, _ []uint32, _ int) (clickhouse.ClaimableScan, error) {
	r.probe.record(ctx)
	return clickhouse.ClaimableScan{}, nil
}

func (r *capReader) ClaimableIndexWatermark(ctx context.Context) (uint32, error) {
	r.probe.record(ctx)
	return 0, nil
}

func (r *capReader) IndexedClaimableBalances(ctx context.Context, _ string, _ int) (clickhouse.ClaimableScan, error) {
	r.probe.record(ctx)
	return clickhouse.ClaimableScan{}, nil
}

func (r *capReader) SignerIndexWatermark(ctx context.Context) (uint32, error) {
	r.probe.record(ctx)
	return 1, nil
}

func (r *capReader) AccountsSignedBy(ctx context.Context, _ string, _ []uint32, _ int) (clickhouse.SignerScan, error) {
	r.probe.record(ctx)
	return clickhouse.SignerScan{}, nil
}

func (r *capReader) AssetEntryLedgers(ctx context.Context, _ string, _ []string, _ uint32, _ int) ([]uint32, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) StreamAccountEntryChangesForLedgers(ctx context.Context, _ string, ledgers []uint32, fn func(clickhouse.EntryHistorySourceRow) error) error {
	return r.StreamEntryChangesForLedgers(ctx, ledgers, fn)
}

func (r *capReader) StreamEntryChangesForLedgers(ctx context.Context, _ []uint32, _ func(clickhouse.EntryHistorySourceRow) error) error {
	r.probe.record(ctx)
	return nil
}

// capPositions is a PositionsReader that shares the same probe — the positions
// endpoint's lake dependency is Postgres (this seam), so its bounded context
// arrives here rather than at capReader.
type capPositions struct{ probe *deadlineProbe }

func (p *capPositions) BlendPositionsByUser(ctx context.Context, _ string) ([]timescale.BlendPositionFold, error) {
	p.probe.record(ctx)
	return nil, nil
}

func (p *capPositions) BlendBackstopSharesByUser(ctx context.Context, _ string) ([]timescale.BlendBackstopFold, error) {
	p.probe.record(ctx)
	return nil, nil
}

func (p *capPositions) PhoenixStakeByUser(ctx context.Context, _ string) ([]timescale.PhoenixStakeFold, error) {
	p.probe.record(ctx)
	return nil, nil
}

func (p *capPositions) DefindexVaultSharesByUser(ctx context.Context, _ string) ([]timescale.DefindexVaultFold, error) {
	p.probe.record(ctx)
	return nil, nil
}

func (p *capPositions) CreditPositionsByOwner(ctx context.Context, _ string) ([]timescale.CreditPositionFold, error) {
	p.probe.record(ctx)
	return nil, nil
}

func (p *capPositions) AquariusGaugeByUser(ctx context.Context, _ string) ([]timescale.AquariusGaugeFold, error) {
	p.probe.record(ctx)
	return nil, nil
}

// newProbeHandler wires a Handler with the minimal seams the endpoints need,
// mirroring internal/api/v1/explorer.go's construction (ParseLimit returns the
// default, WriteJSON/WriteProblem just set a status, ClientAborted is false).
func newProbeHandler(reader ExplorerReader, positions PositionsReader) *Handler {
	return &Handler{
		Reader:         reader,
		Positions:      positions,
		PricingEnabled: true,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		ParseLimit: func(_ http.ResponseWriter, _ *http.Request, def, _ int) (int, bool) {
			return def, true
		},
		ParseWindowDays: func(_ http.ResponseWriter, _ *http.Request, def int) (int, bool) { return def, true },
		LakeWatermark:   func(_ context.Context) (uint32, bool, bool) { return 0, false, false },
		IsKnownSAC:      func(string) bool { return false },
		ClientAborted:   func(*http.Request, error) bool { return false },
		WriteProblem: func(w http.ResponseWriter, _ *http.Request, _, _ string, status int, _ string) {
			w.WriteHeader(status)
		},
		WriteJSON: func(w http.ResponseWriter, _ any, _ bool) {
			w.WriteHeader(http.StatusOK)
		},
	}
}

// validTestAccount / validTestContract are the same well-formed strkeys the
// package-v1 explorer tests use — they must pass canonical.IsAccountID /
// IsContractID so the handlers reach their lake read rather than 400ing.
const (
	validTestAccount  = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	validTestContract = "CAM7DY53G63XA4AJRS24Z6VFYAFSSF76C3RZ45BE5YU3FQS5255OOABP"
	validTestTxHash   = "88526317d98b1eb5a8040123456789abcdef0123456789abcdef0123456789ab"
)

// Keeps the route table honest: a stub that returns without recording its context makes its route look unreachable, so
// the route is never added and its deadline goes unguarded. Every ExplorerReader / PositionsReader method must record.
func TestCapReader_EveryMethodRecordsDeadline(t *testing.T) {
	probe := &deadlineProbe{}
	stubs := []struct {
		iface reflect.Type
		impl  any
	}{
		{reflect.TypeOf((*ExplorerReader)(nil)).Elem(), &capReader{probe: probe}},
		{reflect.TypeOf((*PositionsReader)(nil)).Elem(), &capPositions{probe: probe}},
	}
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	for _, st := range stubs {
		impl := reflect.ValueOf(st.impl)
		for i := 0; i < st.iface.NumMethod(); i++ {
			name := st.iface.Method(i).Name
			m := impl.MethodByName(name)
			args := make([]reflect.Value, m.Type().NumIn())
			for j := range args {
				args[j] = reflect.Zero(m.Type().In(j))
			}
			if len(args) == 0 || m.Type().In(0) != ctxType {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), explorerReadTimeout)
			args[0] = reflect.ValueOf(ctx)
			*probe = deadlineProbe{}
			m.Call(args)
			cancel()
			if !probe.sawCall || !probe.hasDL {
				t.Errorf("%s.%s does not record its context on the deadline probe", st.iface.Name(), name)
			}
		}
	}
}

// blockingReader is a capReader whose RecentLedgers blocks until its context is
// cancelled (with a generous hard fallback so an un-fixed, deadline-less run
// doesn't leak the goroutine forever).
type blockingReader struct{ *capReader }

func (b *blockingReader) RecentLedgers(ctx context.Context, _ int, _ uint32) ([]clickhouse.LedgerHeader, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(3 * explorerReadTimeout):
		return nil, errors.New("blocking reader hard fallback fired")
	}
}

func (r *capReader) ContractStats(ctx context.Context) (clickhouse.ContractStats, error) {
	r.probe.record(ctx)
	return clickhouse.ContractStats{}, nil
}

func (r *capReader) ContractTypes(ctx context.Context, _ []string) (map[string]bool, error) {
	r.probe.record(ctx)
	return nil, nil
}

type fakeNetTimeout struct{}

func (fakeNetTimeout) Error() string   { return "read tcp 127.0.0.1:34612->127.0.0.1:9300: i/o timeout" }
func (fakeNetTimeout) Timeout() bool   { return true }
func (fakeNetTimeout) Temporary() bool { return true }

// saturationReader is a capReader whose AccountStateCached fails with a
// caller-chosen error, so a test can drive the saturation-vs-genuine-error
// branch of the AccountState handler's status mapping without a live lake.
type saturationReader struct {
	*capReader
	err error
}

func (r *saturationReader) AccountStateCached(context.Context, string) (clickhouse.AccountState, bool, error) {
	return clickhouse.AccountState{}, false, r.err
}

// deadlineDirectoryReader fails the batch lookup the way an expired
// explorerReadTimeout does — the driver's message WRAPPING
// context.DeadlineExceeded, which is how it arrives in production.
type deadlineDirectoryReader struct{ DirectoryReader }

func (deadlineDirectoryReader) DirectoryEntriesByAddresses(
	context.Context, []string,
) (map[string]timescale.DirectoryEntry, error) {
	return nil, fmt.Errorf("read account_directory: %w", context.DeadlineExceeded)
}

// GET /v1/directory must map a blown explorerReadTimeout to a ...-timeout 503 like sibling listings (retryableColdMiss).
// The request context is still alive then (8s budget inside the 15s deadline), so a 500 is never corrected and the sla-
// probe would book an availability failure for something a retry clears.
func TestDirectoryLookup_ReadDeadlineIs503NotInternal500(t *testing.T) {
	const addr = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	h := &Handler{
		Directory:     deadlineDirectoryReader{},
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		ClientAborted: func(*http.Request, error) bool { return false },
		WriteProblem: func(w http.ResponseWriter, _ *http.Request, _, _ string, status int, _ string) {
			w.WriteHeader(status)
		},
		WriteJSON: func(w http.ResponseWriter, _ any, _ bool) {
			w.WriteHeader(http.StatusOK)
		},
	}
	rec := httptest.NewRecorder()
	h.DirectoryLookup(rec, httptest.NewRequest(http.MethodGet, "/v1/directory?addresses="+addr, nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 — a read that blew the explorer budget is retryable "+
			"capacity, not an internal fault", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Errorf("no Retry-After on the retryable 503; every client is left to guess, and the "+
			"guess is `immediately` (headers %v)", rec.Header())
	}
}

func (r *capReader) AccountSponsorships(ctx context.Context, _, _ string, _ bool, _ clickhouse.SponsorshipKey, _ uint32, _ int) ([]clickhouse.SponsorshipArrangement, error) {
	r.probe.record(ctx)
	return nil, nil
}

func (r *capReader) SponsorshipTotals(ctx context.Context, _, _ string, _ uint32) (clickhouse.SponsorshipTotals, error) {
	r.probe.record(ctx)
	return clickhouse.SponsorshipTotals{}, nil
}
