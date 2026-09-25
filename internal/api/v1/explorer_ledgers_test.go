package v1_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

type stubExplorerReader struct {
	ledgers        []clickhouse.LedgerHeader
	txs            []clickhouse.TxSummary
	ops            []clickhouse.OpRow
	opTypeStats    []clickhouse.OpTypeCount
	throughput     []clickhouse.ThroughputBucket
	reserves       []clickhouse.BlendReserveState
	reserveCalls   []blend.PoolVersion // the pool version each BlendPoolReserves call was made with
	opResults      map[uint32]clickhouse.OpResult
	events         []clickhouse.EventSummary
	contractEvents []clickhouse.ContractActivityRow
	wasm           clickhouse.ContractWasmInfo
	wasmErr        error
	instance       clickhouse.ContractInstanceState
	instanceErr    error
	directory      []clickhouse.ContractDirectoryRow
	interactions   []clickhouse.ContractEdgeRow
	codeHistory    []clickhouse.ContractCodeVersion
	accountState   clickhouse.AccountState
	cap67WM        uint32
	accountsStats  clickhouse.AccountsStats
	// accountCreators backs GET /v1/accounts/creators; creatorsLimit
	// records the limit the handler asked for, so a test can pin the
	// handler's clamping rather than trusting it.
	// creatorsAccount / sponsorsAccount record the ?account= filter the
	// handler passed through, so a test can pin that a malformed value
	// never reaches the reader and a well-formed one always does.
	accountCreators clickhouse.AccountCreators
	creatorsLimit   int
	creatorsAccount string
	accountSponsors clickhouse.AccountSponsors
	cohort          clickhouse.AccountCohort
	cohortOK        bool
	cohortRelation  string
	sponsorsLimit   int
	sponsorsAccount string
	// accountGraph backs GET /v1/accounts/{g}/graph. graphCreatedEdges /
	// graphSponsoredEdges are the FULL outbound edge sets; the stub
	// applies the keyset cursor and the limit the way the ClickHouse
	// statement does, so a pagination test exercises the handler's real
	// contract instead of a pre-sliced fixture. graphRelation /
	// graphLimit / graphCursor record what the handler asked for, so a
	// test can pin the parsing rather than trust it.
	accountGraph        clickhouse.AccountGraph
	graphCreatedEdges   []clickhouse.AccountGraphEdge
	graphSponsoredEdges []clickhouse.AccountGraphEdge
	graphRelation       string
	graphLimit          int
	graphCursor         string
	// graphHistory backs GET /v1/accounts/{g}/graph/history. Like the
	// graph above, the creation arm's coverage span is the stub's "a
	// cycle has run" signal.
	graphHistory     clickhouse.AccountGraphHistory
	contractActivity clickhouse.ContractActivitySummary
	holders          []clickhouse.AssetHolder
	holderCount      int64
	wealth           []clickhouse.AccountWealth
	wealthLedger     uint32 // the cached ranking snapshot's AsOfLedger
	pairStates       map[string]clickhouse.SoroswapPairState
	tokenDisplays    map[string]clickhouse.TokenDisplayMeta
	// tokenDisplaysErr fails ONLY TokenDisplays, so tests can exercise a
	// display-lookup outage while the reserve read itself succeeds.
	tokenDisplaysErr error
	nativeLPStates   map[string]clickhouse.NativeLiquidityPoolState
	nativeLPRanked   []clickhouse.NativeLiquidityPoolState
	movements        []clickhouse.AccountMovementRow
	err              error
}

func (s *stubExplorerReader) RecentLedgers(_ context.Context, _ int, _ uint32) ([]clickhouse.LedgerHeader, error) {
	return s.ledgers, s.err
}

func (s *stubExplorerReader) LedgerBySeq(_ context.Context, seq uint32) (clickhouse.LedgerHeader, bool, error) {
	if s.err != nil {
		return clickhouse.LedgerHeader{}, false, s.err
	}
	for _, l := range s.ledgers {
		if l.Seq == seq {
			return l, true, nil
		}
	}
	// The stub only ever lists the tip entry explicitly, but
	// windowFloorLedger's close_time binary search needs an answer for
	// every intermediate sequence too. Synthesize one from the newest
	// known entry at the theoretical 5s cadence (17,280/day) — the exact
	// rate these tests' expected since_ledger values were computed against.
	if len(s.ledgers) == 0 {
		return clickhouse.LedgerHeader{}, false, nil
	}
	tip := s.ledgers[0]
	for _, l := range s.ledgers {
		if l.Seq > tip.Seq {
			tip = l
		}
	}
	if seq == 0 || seq > tip.Seq {
		return clickhouse.LedgerHeader{}, false, nil
	}
	delta := time.Duration(tip.Seq-seq) * (5 * time.Second)
	return clickhouse.LedgerHeader{Seq: seq, CloseTime: tip.CloseTime.Add(-delta)}, true, nil
}

func (s *stubExplorerReader) LedgerTransactions(_ context.Context, _ uint32, _ int) ([]clickhouse.TxSummary, error) {
	return s.txs, s.err
}

func (s *stubExplorerReader) OperationsByLedger(_ context.Context, _ uint32, _ int) ([]clickhouse.OpRow, error) {
	return s.ops, s.err
}

func (s *stubExplorerReader) RecentOperations(_ context.Context, _ int, _ clickhouse.ExplorerCursor) ([]clickhouse.OpRow, error) {
	return s.ops, s.err
}

func (s *stubExplorerReader) OperationTypeStats(_ context.Context, _ uint32) ([]clickhouse.OpTypeCount, error) {
	return s.opTypeStats, s.err
}

func (s *stubExplorerReader) AccountOperationTypeCounts(_ context.Context, _ string) ([]clickhouse.OpTypeCount, error) {
	return s.opTypeStats, s.err
}

func (s *stubExplorerReader) NetworkThroughput(_ context.Context, _ int) ([]clickhouse.ThroughputBucket, error) {
	return s.throughput, s.err
}

func (s *stubExplorerReader) BlendPoolReserves(_ context.Context, _ string, version blend.PoolVersion, _ []string, _ map[string]blend.ReserveConfig) ([]clickhouse.BlendReserveState, error) {
	s.reserveCalls = append(s.reserveCalls, version)
	return s.reserves, s.err
}

func (s *stubExplorerReader) TransactionByHash(_ context.Context, hash string) (clickhouse.TxSummary, bool, error) {
	if s.err != nil {
		return clickhouse.TxSummary{}, false, s.err
	}
	for _, t := range s.txs {
		if t.TxHash == hash {
			return t, true, nil
		}
	}
	return clickhouse.TxSummary{}, false, nil
}

func (s *stubExplorerReader) OperationsByTx(_ context.Context, _ uint32, _ string) ([]clickhouse.OpRow, error) {
	return s.ops, s.err
}

func (s *stubExplorerReader) OperationResultsByTx(_ context.Context, _ uint32, _ string) (map[uint32]clickhouse.OpResult, error) {
	return s.opResults, s.err
}

func (s *stubExplorerReader) TxOutcomesByHash(_ context.Context, _ []uint32, _ []string) (map[string]clickhouse.TxOutcome, error) {
	return nil, nil
}

func (s *stubExplorerReader) EventsByTx(_ context.Context, _ uint32, _ string) ([]clickhouse.EventSummary, error) {
	return s.events, s.err
}

func (s *stubExplorerReader) ContractEventsRecent(_ context.Context, _ string, _ int, _ clickhouse.ContractEventsCursor) ([]clickhouse.ContractActivityRow, error) {
	return s.contractEvents, s.err
}

func (s *stubExplorerReader) ContractWasm(_ context.Context, _ string) (clickhouse.ContractWasmInfo, error) {
	return s.wasm, s.wasmErr
}

func (s *stubExplorerReader) ContractInstanceState(_ context.Context, _ string) (clickhouse.ContractInstanceState, error) {
	return s.instance, s.instanceErr
}

func (s *stubExplorerReader) RecentContracts(_ context.Context, _ int, _ uint32) ([]clickhouse.ContractDirectoryRow, error) {
	return s.directory, s.err
}

func (s *stubExplorerReader) ContractInteractions(_ context.Context, _ string, _ int, since uint32) ([]clickhouse.ContractEdgeRow, uint32, error) {
	return s.interactions, since, s.err
}

func (s *stubExplorerReader) ContractCodeHistory(_ context.Context, _ string) ([]clickhouse.ContractCodeVersion, error) {
	return s.codeHistory, s.err
}

func (s *stubExplorerReader) AccountState(_ context.Context, _ string) (clickhouse.AccountState, error) {
	return s.accountState, s.err
}

func (s *stubExplorerReader) Cap67MovementsWatermark(_ context.Context) (uint32, error) {
	return s.cap67WM, nil
}

func (s *stubExplorerReader) AccountsStats(_ context.Context) (clickhouse.AccountsStats, bool, error) {
	return s.accountsStats, s.accountsStats.TotalAccounts > 0, nil
}

// AccountCreators mirrors the real reader's contract: ok=false unless
// the snapshot carries a covered span, and the board is truncated to
// the limit the handler asked for.
func (s *stubExplorerReader) AccountCreators(_ context.Context, limit int, account string) (clickhouse.AccountCreators, bool, error) {
	s.creatorsLimit, s.creatorsAccount = limit, account
	if s.err != nil {
		return clickhouse.AccountCreators{}, false, s.err
	}
	if s.accountCreators.ThruLedger == 0 {
		return clickhouse.AccountCreators{}, false, nil
	}
	out := s.accountCreators
	// Mirrors the keyed read: the filter selects, the limit does not
	// apply, the row keeps the rank the rollup gave it, and totals and
	// coverage are untouched.
	if account != "" {
		kept := make([]clickhouse.AccountCreatorRow, 0, 1)
		for _, row := range out.Board {
			if row.Creator == account {
				kept = append(kept, row)
			}
		}
		out.Board = kept
		return out, true, nil
	}
	if limit < len(out.Board) {
		out.Board = out.Board[:limit]
	}
	return out, true, nil
}

func (s *stubExplorerReader) ContractActivitySummaryFor(_ context.Context, _ string, _ int) (clickhouse.ContractActivitySummary, bool, error) {
	return s.contractActivity, s.contractActivity.ActiveLedgersTotal > 0, nil
}

func (s *stubExplorerReader) AccountStateCached(_ context.Context, _ string) (clickhouse.AccountState, bool, error) {
	return s.accountState, false, s.err
}

func (s *stubExplorerReader) AssetHolders(_ context.Context, _ string, _ int) ([]clickhouse.AssetHolder, int64, error) {
	return s.holders, s.holderCount, s.err
}

// sacName lets SAC-resolution tests inject a wrapped-asset name;
// empty = not a SAC (the common case for explorer stubs).
func (s *stubExplorerReader) SACClassicAssetName(_ context.Context, _ string) (string, bool, error) {
	return "", false, nil
}

func (s *stubExplorerReader) SACAssetFromEvents(_ context.Context, _ string) (string, bool, error) {
	return "", false, nil
}

func (s *stubExplorerReader) AccountsUnspendable(_ context.Context, _ []string) (map[string]bool, error) {
	return nil, nil
}

func (s *stubExplorerReader) SoroswapPairReserves(_ context.Context, _ []string) (map[string]clickhouse.SoroswapPairState, error) {
	return s.pairStates, s.err
}

func (s *stubExplorerReader) TokenDisplays(_ context.Context, _ []string) (map[string]clickhouse.TokenDisplayMeta, error) {
	if s.tokenDisplaysErr != nil {
		return nil, s.tokenDisplaysErr
	}
	return s.tokenDisplays, s.err
}

func (s *stubExplorerReader) NativeLiquidityPoolReserves(_ context.Context, _ []string) (map[string]clickhouse.NativeLiquidityPoolState, error) {
	return s.nativeLPStates, s.err
}

func (s *stubExplorerReader) NativeLiquidityPoolsRanked(_ context.Context, limit int) ([]clickhouse.NativeLiquidityPoolState, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := s.nativeLPRanked
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *stubExplorerReader) AccountsByWealth(_ context.Context, _ []string, _ []float64, _ int) ([]clickhouse.AccountWealth, error) {
	return s.wealth, s.err
}

// AccountsByWealthCached mirrors the uncached stub: warm-and-fresh whenever
// the stub isn't configured to fail, so existing expectations are
// unchanged. An error case reports cold, which is how the real cache
// signals "nothing ever computed" (site-audit S3).
func (s *stubExplorerReader) AccountsByWealthCached(_ context.Context, _ []string, _ []float64, _ int) (clickhouse.AccountWealthSnapshot, bool) {
	if s.err != nil {
		return clickhouse.AccountWealthSnapshot{}, false
	}
	return clickhouse.AccountWealthSnapshot{
		Rows: s.wealth, Basis: clickhouse.WealthBasisUSD, AsOf: time.Now(), AsOfLedger: s.wealthLedger,
	}, true
}

func (s *stubExplorerReader) AccountTransactions(_ context.Context, _ string, _ int, _ clickhouse.ExplorerCursor) ([]clickhouse.TxSummary, error) {
	return s.txs, s.err
}

func (s *stubExplorerReader) AccountOperations(_ context.Context, _ string, _ int, _ clickhouse.ExplorerCursor) ([]clickhouse.OpRow, error) {
	return s.ops, s.err
}

func (s *stubExplorerReader) AccountMovements(_ context.Context, _ string, _ int, _ clickhouse.AccountMovementCursor, _ clickhouse.AccountMovementFilter) ([]clickhouse.AccountMovementRow, error) {
	return s.movements, s.err
}

// AccountSponsors mirrors the real reader's contract: a snapshot with no
// covered span is not servable, however many board rows it carries.
func (s *stubExplorerReader) AccountSponsors(_ context.Context, limit int, account string) (clickhouse.AccountSponsors, bool, error) {
	s.sponsorsLimit, s.sponsorsAccount = limit, account
	if s.err != nil {
		return clickhouse.AccountSponsors{}, false, s.err
	}
	if s.accountSponsors.ThruLedger == 0 {
		return clickhouse.AccountSponsors{}, false, nil
	}
	out := s.accountSponsors
	if account != "" {
		kept := make([]clickhouse.AccountSponsorRow, 0, 1)
		for _, row := range out.Board {
			if row.Sponsor == account {
				kept = append(kept, row)
			}
		}
		out.Board = kept
		return out, true, nil
	}
	if limit < len(out.Board) {
		out.Board = out.Board[:limit]
	}
	return out, true, nil
}

func (s *stubExplorerReader) AccountGraph(_ context.Context, _, relation string, limit int, cursor string) (clickhouse.AccountGraph, bool, error) {
	s.graphRelation, s.graphLimit, s.graphCursor = relation, limit, cursor
	if s.err != nil {
		return clickhouse.AccountGraph{}, false, s.err
	}
	// The creation arm's span is the stub's "a cycle has run" signal, the
	// same guard the real reader applies to both arms.
	if s.accountGraph.CreationCoverage.ThruLedger == 0 {
		return clickhouse.AccountGraph{}, false, nil
	}
	out := s.accountGraph
	out.Page = nil
	if relation == "" {
		return out, true, nil
	}
	full := s.graphSponsoredEdges
	if relation == clickhouse.GraphRelationCreated {
		full = s.graphCreatedEdges
	}
	// Keyset semantics, exactly as the statement's `counterparty > ?
	// ORDER BY counterparty LIMIT ?` behaves.
	for _, e := range full {
		if e.Account <= cursor {
			continue
		}
		if len(out.Page) >= limit {
			break
		}
		out.Page = append(out.Page, e)
	}
	return out, true, nil
}

// AccountGraphHistory mirrors the real reader's contract: a graph whose
// creation arm has no covered span has not completed a cycle, and a
// series over it would claim "this account has never created anything".
// AccountCohort serves the stub's cohort when a cycle "completed"
// (cohortOK) and records the relation asked for.
func (s *stubExplorerReader) AccountCohort(_ context.Context, _ string, relation string) (clickhouse.AccountCohort, bool, error) {
	if s.err != nil {
		return clickhouse.AccountCohort{}, false, s.err
	}
	s.cohortRelation = relation
	if !s.cohortOK {
		return clickhouse.AccountCohort{}, false, nil
	}
	out := s.cohort
	out.Relation = relation
	return out, true, nil
}

func (s *stubExplorerReader) AccountGraphHistory(_ context.Context, _ string) (clickhouse.AccountGraphHistory, bool, error) {
	if s.err != nil {
		return clickhouse.AccountGraphHistory{}, false, s.err
	}
	if s.graphHistory.Created.Coverage.ThruLedger == 0 {
		return clickhouse.AccountGraphHistory{}, false, nil
	}
	return s.graphHistory, true, nil
}

func explorerTestServer(t *testing.T, r v1.ExplorerReader) string {
	t.Helper()
	srv := v1.New(v1.Options{Explorer: r})
	return httpTestServer(t, srv).URL
}

func TestExplorer_LedgersList(t *testing.T) {
	now := time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC)
	reader := &stubExplorerReader{ledgers: []clickhouse.LedgerHeader{
		{Seq: 100, CloseTime: now, LedgerHash: "ab", PrevHash: "cd", ProtocolVersion: 22, TxCount: 3, OpCount: 5, TotalCoins: 5000000000000000000, FeePool: 12345, BaseFee: 100, BaseReserve: 5000000},
		{Seq: 99, CloseTime: now, LedgerHash: "ef", PrevHash: "gh", TotalCoins: 1, FeePool: 0},
	}}
	base := explorerTestServer(t, reader)

	resp := mustGet(t, base+"/v1/ledgers?limit=10")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data v1.LedgersListView `json:"data"`
	}
	mustDecode(t, resp, &body)
	if len(body.Data.Ledgers) != 2 {
		t.Fatalf("want 2 ledgers, got %d", len(body.Data.Ledgers))
	}
	// total_coins must be a STRING (ADR-0003 — exceeds 2^53).
	if body.Data.Ledgers[0].TotalCoins != "5000000000000000000" {
		t.Errorf("total_coins = %q, want exact string", body.Data.Ledgers[0].TotalCoins)
	}
	// next_before = last (oldest) ledger's seq for keyset paging.
	if body.Data.NextBefore != 99 {
		t.Errorf("next_before = %d, want 99", body.Data.NextBefore)
	}
}

func TestExplorer_LedgerDetail_FoundAndNotFound(t *testing.T) {
	reader := &stubExplorerReader{ledgers: []clickhouse.LedgerHeader{{Seq: 42, LedgerHash: "deadbeef"}}}
	base := explorerTestServer(t, reader)

	resp := mustGet(t, base+"/v1/ledgers/42")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("found: status = %d", resp.StatusCode)
	}
	var body struct {
		Data v1.LedgerView `json:"data"`
	}
	mustDecode(t, resp, &body)
	if body.Data.Sequence != 42 || body.Data.Hash != "deadbeef" {
		t.Errorf("ledger view = %+v", body.Data)
	}

	resp = mustGet(t, base+"/v1/ledgers/999")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing ledger: status = %d, want 404", resp.StatusCode)
	}
}

// Stroop fees and reserves are money: on the wire they are decimal
// strings like total_coins beside them, never JSON numbers (ADR-0003).
func TestExplorer_LedgerAndTxFeesAreWireStrings(t *testing.T) {
	reader := &stubExplorerReader{
		ledgers: []clickhouse.LedgerHeader{{Seq: 42, TxCount: 1, BaseFee: 100, BaseReserve: 5_000_000}},
		txs: []clickhouse.TxSummary{
			{Seq: 42, TxHash: "tx1", SourceAccount: "GABC", FeeCharged: 9_007_199_254_740_993, MaxFee: 120_000, OperationCount: 1, Successful: true},
		},
	}
	base := explorerTestServer(t, reader)

	var ledger struct {
		Data map[string]any `json:"data"`
	}
	mustDecode(t, mustGet(t, base+"/v1/ledgers/42"), &ledger)
	for field, want := range map[string]string{"base_fee": "100", "base_reserve": "5000000"} {
		if got, ok := ledger.Data[field].(string); !ok || got != want {
			t.Errorf("ledger %s = %#v, want the string %q", field, ledger.Data[field], want)
		}
	}

	var txs struct {
		Data struct {
			Transactions []map[string]any `json:"transactions"`
		} `json:"data"`
	}
	mustDecode(t, mustGet(t, base+"/v1/ledgers/42/transactions"), &txs)
	if len(txs.Data.Transactions) != 1 {
		t.Fatalf("transactions = %+v", txs.Data.Transactions)
	}
	tx := txs.Data.Transactions[0]
	for field, want := range map[string]string{"fee_charged": "9007199254740993", "max_fee": "120000"} {
		if got, ok := tx[field].(string); !ok || got != want {
			t.Errorf("tx %s = %#v, want the string %q", field, tx[field], want)
		}
	}
}

func TestExplorer_LedgerDetail_InvalidSeq(t *testing.T) {
	base := explorerTestServer(t, &stubExplorerReader{})
	resp := mustGet(t, base+"/v1/ledgers/notanumber")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestExplorer_LedgerTransactions(t *testing.T) {
	reader := &stubExplorerReader{
		ledgers: []clickhouse.LedgerHeader{{Seq: 42, TxCount: 1}},
		txs: []clickhouse.TxSummary{
			{Seq: 42, TxHash: "tx1", TxIndex: 0, SourceAccount: "GABC", FeeCharged: 100, OperationCount: 2, Successful: true, MemoType: "text", Memo: "hi"},
		},
	}
	base := explorerTestServer(t, reader)
	resp := mustGet(t, base+"/v1/ledgers/42/transactions")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data v1.LedgerTransactionsView `json:"data"`
	}
	mustDecode(t, resp, &body)
	if len(body.Data.Transactions) != 1 || body.Data.Transactions[0].Hash != "tx1" {
		t.Errorf("txs = %+v", body.Data.Transactions)
	}
	if !body.Data.Transactions[0].Successful || body.Data.Transactions[0].Memo != "hi" {
		t.Errorf("tx fields = %+v", body.Data.Transactions[0])
	}
	if body.Data.Total != 1 || body.Data.Truncated {
		t.Errorf("total/truncated = %d/%v, want 1/false", body.Data.Total, body.Data.Truncated)
	}
}

// TestExplorer_LedgerTransactions_Truncated pins the T172 fix: when a
// ledger's header reports more transactions than the page-size cap
// returned, the response must say so — the caller previously had no way
// to distinguish a genuinely short ledger from a silently truncated one.
func TestExplorer_LedgerTransactions_Truncated(t *testing.T) {
	reader := &stubExplorerReader{
		ledgers: []clickhouse.LedgerHeader{{Seq: 42, TxCount: 5}},
		txs: []clickhouse.TxSummary{
			{Seq: 42, TxHash: "tx1", TxIndex: 0, SourceAccount: "GABC", FeeCharged: 100, OperationCount: 1, Successful: true},
			{Seq: 42, TxHash: "tx2", TxIndex: 1, SourceAccount: "GABD", FeeCharged: 100, OperationCount: 1, Successful: true},
		},
	}
	base := explorerTestServer(t, reader)
	resp := mustGet(t, base+"/v1/ledgers/42/transactions?limit=2")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data v1.LedgerTransactionsView `json:"data"`
	}
	mustDecode(t, resp, &body)
	if body.Data.Total != 5 {
		t.Errorf("total = %d, want 5 (ledger header tx_count)", body.Data.Total)
	}
	if !body.Data.Truncated {
		t.Errorf("truncated = false, want true (2 of 5 returned)")
	}
}

const testTxHash = "88526317d98b1eb5a8040123456789abcdef0123456789abcdef0123456789ab"

func TestExplorer_TxDetail(t *testing.T) {
	reader := &stubExplorerReader{
		txs:       []clickhouse.TxSummary{{Seq: 42, TxHash: testTxHash, SourceAccount: "GABC", FeeCharged: 300, OperationCount: 1, Successful: true, MemoType: "MemoTypeMemoText", Memo: "hello"}},
		ops:       []clickhouse.OpRow{{Seq: 42, TxHash: testTxHash, OpIndex: 0, OpType: "OperationTypePayment", BodyXDR: "not-valid-xdr"}},
		opResults: map[uint32]clickhouse.OpResult{0: {Code: 0}},
		events:    []clickhouse.EventSummary{{OpIndex: 0, EventIndex: 1, ContractID: "CABC", EventType: "contract", Topic0Sym: "transfer"}},
	}
	base := explorerTestServer(t, reader)
	resp := mustGet(t, base+"/v1/tx/"+testTxHash)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data v1.TxDetailView `json:"data"`
	}
	mustDecode(t, resp, &body)
	if body.Data.Hash != testTxHash {
		t.Errorf("hash = %q", body.Data.Hash)
	}
	if body.Data.MemoType != "text" { // normalized from MemoTypeMemoText
		t.Errorf("memo_type = %q, want text", body.Data.MemoType)
	}
	if len(body.Data.Operations) != 1 {
		t.Fatalf("ops = %d, want 1", len(body.Data.Operations))
	}
	// op had invalid XDR -> raw_xdr fallback, result_code populated from map.
	if body.Data.Operations[0].ResultCode == nil || *body.Data.Operations[0].ResultCode != 0 {
		t.Errorf("result_code = %v, want 0", body.Data.Operations[0].ResultCode)
	}
	if len(body.Data.Events) != 1 || body.Data.Events[0].Topic0 != "transfer" {
		t.Errorf("events = %+v", body.Data.Events)
	}
}

func TestExplorer_TxDetail_InvalidHashAndNotFound(t *testing.T) {
	base := explorerTestServer(t, &stubExplorerReader{})
	if resp := mustGet(t, base+"/v1/tx/xyz"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("short hash: status = %d, want 400", resp.StatusCode)
	}
	if resp := mustGet(t, base+"/v1/tx/"+testTxHash); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown tx: status = %d, want 404", resp.StatusCode)
	}
}

func TestExplorer_ContractDetail(t *testing.T) {
	const cid = "CAM7DY53G63XA4AJRS24Z6VFYAFSSF76C3RZ45BE5YU3FQS5255OOABP"
	reader := &stubExplorerReader{contractEvents: []clickhouse.ContractActivityRow{
		{Seq: 63000000, TxHash: "abc", OpIndex: 0, EventIndex: 1, EventType: "contract", Topic0Sym: "transfer"},
		{Seq: 62999000, TxHash: "def", OpIndex: 0, EventIndex: 0, EventType: "contract", Topic0Sym: "mint"},
	}}
	base := explorerTestServer(t, reader)
	// limit=2 makes this a FULL page (n==limit) so a next_cursor is emitted —
	// the full row-identity composite (ledger, tx_hash, op_index, event_index)
	// of the last (oldest) row.
	resp := mustGet(t, base+"/v1/contracts/"+cid+"?limit=2")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data v1.ContractDetailView `json:"data"`
	}
	mustDecode(t, resp, &body)
	if body.Data.ContractID != cid || len(body.Data.Events) != 2 {
		t.Fatalf("detail = %+v", body.Data)
	}
	if body.Data.Events[0].Topic0 != "transfer" || body.Data.NextCursor != "62999000.def.0.0" {
		t.Errorf("events/cursor = %+v next=%q", body.Data.Events, body.Data.NextCursor)
	}
}

func TestExplorer_ContractDetail_InvalidID(t *testing.T) {
	base := explorerTestServer(t, &stubExplorerReader{})
	if resp := mustGet(t, base+"/v1/contracts/notacontract"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// The contract-activity cursor must carry the tx_hash discriminator: the
// legacy 3-part (ledger, op_index, event_index) tuple is not unique
// (single-op txs all tie at 0.0) and paging on it permanently skipped tied
// rows (cold audit 2026-08-03). A 4-part cursor round-trips; the legacy
// 3-part form is rejected as invalid rather than silently mis-paged.
func TestExplorer_ContractDetail_CursorRequiresTxHash(t *testing.T) {
	const cid = "CAM7DY53G63XA4AJRS24Z6VFYAFSSF76C3RZ45BE5YU3FQS5255OOABP"
	base := explorerTestServer(t, &stubExplorerReader{})
	if resp := mustGet(t, base+"/v1/contracts/"+cid+"?cursor=62999000.def.0.0"); resp.StatusCode != http.StatusOK {
		t.Errorf("4-part cursor: status = %d, want 200", resp.StatusCode)
	}
	if resp := mustGet(t, base+"/v1/contracts/"+cid+"?cursor=62999000.0.0"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("legacy 3-part cursor: status = %d, want 400", resp.StatusCode)
	}
}

func TestExplorer_AccountActivity(t *testing.T) {
	const g = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	reader := &stubExplorerReader{
		txs: []clickhouse.TxSummary{{Seq: 100, TxHash: "h1", SourceAccount: g, Successful: true}},
		ops: []clickhouse.OpRow{{Seq: 100, TxHash: "h1", OpIndex: 0, OpType: "OperationTypePayment", BodyXDR: "x"}},
	}
	base := explorerTestServer(t, reader)

	resp := mustGet(t, base+"/v1/accounts/"+g+"/transactions")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("txs status = %d", resp.StatusCode)
	}
	var tb struct {
		Data v1.AccountTransactionsView `json:"data"`
	}
	mustDecode(t, resp, &tb)
	if tb.Data.Account != g || len(tb.Data.Transactions) != 1 || tb.Data.Scope != "all" {
		t.Errorf("account txs = %+v", tb.Data)
	}

	resp = mustGet(t, base+"/v1/accounts/"+g+"/operations")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ops status = %d", resp.StatusCode)
	}
	var ob struct {
		Data v1.AccountOperationsView `json:"data"`
	}
	mustDecode(t, resp, &ob)
	if len(ob.Data.Operations) != 1 {
		t.Errorf("account ops = %+v", ob.Data)
	}

	// invalid strkey -> 400
	if r := mustGet(t, base+"/v1/accounts/notanaccount/transactions"); r.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid account: status = %d, want 400", r.StatusCode)
	}
}

func TestExplorer_Unavailable503(t *testing.T) {
	base := explorerTestServer(t, nil)
	for _, path := range []string{"/v1/ledgers", "/v1/ledgers/1", "/v1/ledgers/1/transactions", "/v1/operations", "/v1/tx/" + testTxHash} {
		resp := mustGet(t, base+path)
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503", path, resp.StatusCode)
		}
	}
}
