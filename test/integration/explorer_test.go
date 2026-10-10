//go:build integration

package integration_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stellar/go-stellar-sdk/historyarchive"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/explorer"
	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/ops/chops"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
	"github.com/Stellar-Index/StellarIndex/test/harness"
)

// opKey orders like the stellar.operations sort key.
type opKey struct{ L, T, O uint32 }

func (a opKey) less(b opKey) bool {
	if a.L != b.L {
		return a.L < b.L
	}
	if a.T != b.T {
		return a.T < b.T
	}
	return a.O < b.O
}

// TestOperationsTypeFilter_PagesSparseTypeWithoutGapOrDup walks
// /v1/operations?type= through the real handler and reader over a sparse
// type: matches sit on and beside the 5,000-ledger scan floors, several share
// a ledger and a transaction, one is an un-merged duplicate part, and payments
// are interleaved as non-matches. Every limit must return each match exactly
// once, newest first, with every cursor strictly below the last.
func TestOperationsTypeFilter_PagesSparseTypeWithoutGapOrDup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")
	if err := raw.Exec(ctx, "SYSTEM STOP MERGES stellar.operations"); err != nil {
		t.Fatalf("SYSTEM STOP MERGES: %v", err)
	}
	t.Cleanup(func() { _ = raw.Exec(context.Background(), "SYSTEM START MERGES stellar.operations") })

	// A range no other test seeds; the walk starts above it and stops below it.
	const top = uint32(3_500_000_000)
	const window = 5000
	start := top + 1
	floor := func(k uint32) uint32 { return start - k*window } // short-page scan floors
	bottom := floor(4) - 1

	matches := []opKey{
		{top, 2, 0},
		{top, 2, 1},
		{top, 2, 2},
		{top, 5, 0},
		{floor(1), 0, 0},
		{floor(1) - 1, 0, 0},
		{floor(1) - 1, 1, 0},
		{floor(2), 3, 1},
		{floor(2) - 1, 0, 0},
		{floor(3), 0, 0},
		{floor(4) + 9, 0, 0},
	}
	nonMatches := []opKey{{top, 2, 3}, {floor(1), 0, 1}, {floor(2), 3, 0}, {floor(3) + 1, 0, 0}, {bottom, 0, 0}}
	insert := func(k opKey, opType, ingestedAt string) { insertLakeOp(ctx, t, raw, k, opType, ingestedAt) }
	for _, k := range matches {
		insert(k, "OperationTypeInflation", "2026-09-01 00:00:00")
	}
	for _, k := range nonMatches {
		insert(k, "OperationTypePayment", "2026-09-01 00:00:00")
	}
	dup := matches[4]
	insert(dup, "OperationTypeInflation", "2026-09-02 00:00:00") // a second, un-merged part
	// Physical rows on purpose: the walk must collapse exactly these two.
	var copies []struct {
		IngestedAt time.Time `ch:"ingested_at"`
	}
	if err := raw.Select(ctx, &copies, `SELECT ingested_at FROM stellar.operations WHERE ledger_seq = ? AND tx_index = 0 AND op_index = 0`, dup.L); err != nil || len(copies) != 2 {
		t.Fatalf("duplicate rows = %d (err %v), want 2 un-merged copies", len(copies), err)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	for _, limit := range []int{1, 2, 3, 50} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			assertServedExactly(t, walkTypedOperations(t, er, limit, opKey{start, 0, 0}, bottom), matches)
		})
	}
}

func insertLakeOp(ctx context.Context, t *testing.T, raw driver.Conn, k opKey, opType, ingestedAt string) {
	t.Helper()
	q := fmt.Sprintf(`INSERT INTO stellar.operations
		(ledger_seq, close_time, tx_hash, tx_index, op_index, op_type, source_account, body_xdr, ingested_at)
		VALUES (%d, toDateTime('2030-01-01 00:00:00', 'UTC'), '%064x', %d, %d, '%s', '', '', toDateTime('%s', 'UTC'))`,
		k.L, uint64(k.L)<<8|uint64(k.T), k.T, k.O, opType, ingestedAt)
	if err := raw.Exec(ctx, q); err != nil {
		t.Fatalf("insert %v: %v", k, err)
	}
}

// assertServedExactly: walkTypedOperations already proved strict descent, so
// equal length plus full membership means each match exactly once.
func assertServedExactly(t *testing.T, got, matches []opKey) {
	t.Helper()
	if len(got) != len(matches) {
		t.Fatalf("walk returned %d ops, want %d: %v", len(got), len(matches), got)
	}
	seen := map[opKey]bool{}
	for _, k := range got {
		seen[k] = true
	}
	for _, k := range matches {
		if !seen[k] {
			t.Errorf("match %v never served (gap)", k)
		}
	}
}

func typedOpsHandler(t *testing.T, er *chstore.ExplorerReader, page *explorer.OperationsView) *explorer.Handler {
	capture := func(w http.ResponseWriter, data any) {
		*page, _ = data.(explorer.OperationsView)
		w.WriteHeader(http.StatusOK)
	}
	return &explorer.Handler{
		Reader: er,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ParseLimit: func(_ http.ResponseWriter, r *http.Request, def, _ int) (int, bool) {
			n, err := strconv.Atoi(r.URL.Query().Get("limit"))
			if err != nil {
				return def, true
			}
			return n, true
		},
		LakeWatermark: func(context.Context) (uint32, bool, bool) { return 0, false, false },
		ClientAborted: func(*http.Request, error) bool { return false },
		WriteProblem: func(w http.ResponseWriter, _ *http.Request, _, title string, status int, detail string) {
			t.Errorf("problem %d %s: %s", status, title, detail)
			w.WriteHeader(status)
		},
		WriteJSON:   func(w http.ResponseWriter, data any, _ bool) { capture(w, data) },
		WriteJSONAt: func(w http.ResponseWriter, data any, _, _ bool, _ time.Time) { capture(w, data) },
	}
}

// appendTypedPage appends a page's ops at or above bottom, failing unless
// each is strictly below the request cursor and the previous op.
func appendTypedPage(t *testing.T, out []opKey, ops []explorer.OpView, cur opKey, bottom uint32) []opKey {
	t.Helper()
	for _, o := range ops {
		k := opKey{o.Ledger, o.TxIndex, o.OpIndex}
		if !k.less(cur) {
			t.Fatalf("cursor %v served %v, not strictly below it", cur, k)
		}
		if n := len(out); n > 0 && !k.less(out[n-1]) {
			t.Fatalf("op %v after %v: duplicate or out of order", k, out[n-1])
		}
		if k.L >= bottom {
			out = append(out, k)
		}
	}
	return out
}

// walkTypedOperations pages ?type=inflation from cursor `from` until the
// cursor drops below `bottom`, failing on any ordering or cursor regression.
func walkTypedOperations(t *testing.T, er *chstore.ExplorerReader, limit int, from opKey, bottom uint32) []opKey {
	t.Helper()
	var page explorer.OperationsView
	h := typedOpsHandler(t, er, &page)
	var out []opKey
	cur := from
	for pages := 0; cur.L >= bottom; pages++ {
		if pages > 64 {
			t.Fatalf("no progress after %d pages at cursor %v", pages, cur)
		}
		page = explorer.OperationsView{}
		rec := httptest.NewRecorder()
		h.Operations(rec, httptest.NewRequest(http.MethodGet,
			fmt.Sprintf("/v1/operations?type=inflation&limit=%d&cursor=%d.%d.%d", limit, cur.L, cur.T, cur.O), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("cursor %v: status %d", cur, rec.Code)
		}
		if len(page.Operations) > limit {
			t.Fatalf("cursor %v: %d ops, limit %d", cur, len(page.Operations), limit)
		}
		out = appendTypedPage(t, out, page.Operations, cur, bottom)
		if page.NextCursor == "" {
			break
		}
		next := parseOpCursor(t, page.NextCursor)
		if !next.less(cur) {
			t.Fatalf("next_cursor %v does not move below %v", next, cur)
		}
		cur = next
	}
	return out
}

func parseOpCursor(t *testing.T, s string) opKey {
	t.Helper()
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		t.Fatalf("cursor %q is not ledger.tx.op", s)
	}
	var n [3]uint32
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			t.Fatalf("cursor %q: %v", s, err)
		}
		n[i] = uint32(v)
	}
	return opKey{n[0], n[1], n[2]}
}

// TestClickHouseContractActivitySummaryRMTDedup is the live-ClickHouse proof
// for ContractActivitySummaryFor read stellar.contract_active_ledgers
// (a ReplacingMergeTree) with a bare count(), so an overlapping backfill window
// that re-inserts the same (contract, ledger) keys as a second un-merged part
// inflated ActiveLedgersTotal (a headline card number) and the daily bars up to
// ~2x until a background merge. The fix counts uniqExact(ledger_seq). SYSTEM STOP
// MERGES pins the two parts un-merged so the dedup MUST come from the query —
// reverting the fix makes the total read 6 instead of the 3 distinct ledgers.
func TestClickHouseContractActivitySummaryRMTDedup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const contractID = "CTEST_CHQ2_ACTIVITY_DEDUP_AAAAAAAAAAAAAAAAAAA"
	// Three DISTINCT active ledgers, all recent so they land inside the daily
	// window (close_time within the last few days).
	base := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Hour)
	ledgers := []uint32{80_100_001, 80_100_002, 80_100_003}

	raw := dialClickHouse(t, ctx, "stellar")
	if err := raw.Exec(ctx, "SYSTEM STOP MERGES stellar.contract_active_ledgers"); err != nil {
		t.Fatalf("SYSTEM STOP MERGES: %v", err)
	}
	t.Cleanup(func() {
		_ = raw.Exec(context.Background(), "SYSTEM START MERGES stellar.contract_active_ledgers")
	})

	// Two IDENTICAL inserts → two un-merged parts, each carrying the same three
	// (contract, ledger) keys (the overlapping-backfill / re-ingest state).
	for pass := 0; pass < 2; pass++ {
		b, err := raw.PrepareBatch(ctx,
			`INSERT INTO stellar.contract_active_ledgers (contract_id, ledger_seq, close_time, ingested_at)`)
		if err != nil {
			t.Fatalf("prepare active_ledgers batch (pass %d): %v", pass, err)
		}
		ing := time.Now().UTC().Add(time.Duration(pass) * time.Minute)
		for i, l := range ledgers {
			if err := b.Append(contractID, l, base.Add(time.Duration(i)*time.Hour), ing); err != nil {
				t.Fatalf("append active ledger (pass %d): %v", pass, err)
			}
		}
		if err := b.Send(); err != nil {
			t.Fatalf("send active_ledgers batch (pass %d): %v", pass, err)
		}
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	s, ok, err := er.ContractActivitySummaryFor(ctx, contractID, 30)
	if err != nil || !ok {
		t.Fatalf("ContractActivitySummaryFor: ok=%v err=%v", ok, err)
	}
	if s.ActiveLedgersTotal != 3 {
		t.Fatalf("ActiveLedgersTotal = %d, want 3 distinct ledgers — the un-merged duplicate part "+
			"must be deduped by uniqExact (pre-fix count(): 6)", s.ActiveLedgersTotal)
	}
	var dailySum uint64
	for _, d := range s.Daily {
		dailySum += d.ActiveLedgers
	}
	if dailySum != 3 {
		t.Fatalf("daily active-ledger sum = %d across %d days, want 3 (per-day bars must dedup too)",
			dailySum, len(s.Daily))
	}
}

// TestClickHouseContractEventsRecentPartialBackfill is the live-ClickHouse proof
// for contract_active_ledgers' availability probe is a
// LIMIT-1 table-global emptiness check that cannot see PARTIAL backfill
// coverage. In the applied-but-still-backfilling state the index is globally
// non-empty (some OTHER contract's rows) but holds NO rows for a quiet contract
// whose events do exist in contract_events. The old reader trusted that empty
// per-contract walk and served an authoritative "no events". The fix falls
// through to the unbounded contract_events scan (the source of truth). Reverting
// the fix makes ContractEventsRecent return 0 rows here instead of the real event.
func TestClickHouseContractEventsRecentPartialBackfill(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	const (
		quietContract = "CTEST_CHROLLUP3_QUIET_AAAAAAAAAAAAAAAAAAAAAAA"
		decoyContract = "CTEST_CHROLLUP3_DECOY_AAAAAAAAAAAAAAAAAAAAAAA"
		ledger        = uint32(5_000_101) // low ledger: the un-backfilled prefix
		txHash        = "3333333333333333333333333333333333333333333333333333333333333333"
	)
	closeTime := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	sink, err := chstore.Open(ctx, addr, 1000)
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close(ctx) })

	// Ingest the quiet contract's event. This also populates
	// contract_active_ledgers via its MV — which we then TRUNCATE to
	// reproduce the partial-backfill state (index present, this contract's
	// coverage NOT yet backfilled).
	ext := chstore.LedgerExtract{
		Ledger: chstore.LedgerRow{
			LedgerSeq: ledger, CloseTime: closeTime, LedgerHash: "aa11aa11", PrevHash: "bb22bb22",
			ProtocolVersion: 22, TxCount: 1, OpCount: 1, SorobanEventCount: 1,
		},
		Events: []chstore.ContractEventRow{{
			LedgerSeq: ledger, CloseTime: closeTime, TxHash: txHash, OpIndex: 0, EventIndex: 0,
			ContractID: quietContract, EventType: "contract", TopicCount: 1, Topic0Sym: "transfer",
			TopicsXDR: []string{scval.MustEncodeSymbol("transfer")}, DataXDR: scval.MustEncodeString("x"),
			OpArgsXDR: []string{}, InSuccessfulCall: 1,
		}},
	}
	if err := sink.Add(ctx, ext); err != nil {
		t.Fatalf("sink add: %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("sink flush: %v", err)
	}

	raw := dialClickHouse(t, ctx, "stellar")
	// Wipe the MV-populated coverage, then seed ONE decoy row so the table is
	// globally non-empty (probe passes) but holds nothing for the quiet
	// contract — the exact partial-backfill state the finding describes.
	if err := raw.Exec(ctx, "TRUNCATE TABLE stellar.contract_active_ledgers"); err != nil {
		t.Fatalf("truncate active_ledgers: %v", err)
	}
	db, err := raw.PrepareBatch(ctx,
		`INSERT INTO stellar.contract_active_ledgers (contract_id, ledger_seq, close_time, ingested_at)`)
	if err != nil {
		t.Fatalf("prepare decoy batch: %v", err)
	}
	if err := db.Append(decoyContract, uint32(90_000_000), closeTime, time.Now().UTC()); err != nil {
		t.Fatalf("append decoy: %v", err)
	}
	if err := db.Send(); err != nil {
		t.Fatalf("send decoy: %v", err)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("new explorer reader: %v", err)
	}
	t.Cleanup(func() { _ = er.Close() })

	rows, err := er.ContractEventsRecent(ctx, quietContract, 100, chstore.ContractEventsCursor{})
	if err != nil {
		t.Fatalf("ContractEventsRecent: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ContractEventsRecent returned %d rows, want 1 — the partial-backfill empty walk must "+
			"fall through to the unbounded contract_events scan, not serve a confidently-wrong empty page "+
			"(pre-fix: 0)", len(rows))
	}
	if rows[0].Seq != ledger {
		t.Fatalf("served event at ledger %d, want %d", rows[0].Seq, ledger)
	}
}

// TestExplorerScanQueries_ExecuteAgainstServer proves, against a REAL
// ClickHouse server, that every scan-shaped explorer query (extracted to
// builders + pinned with
// `SETTINGS max_threads/max_memory_usage`) still parses and executes — the
// unit tests pin the SQL text; this pins that the text is valid ClickHouse
// (a misplaced SETTINGS clause or a drifted placeholder count fails HERE,
// not in production). Result contents are asserted only where a seeded row
// exercises a code path that would otherwise short-circuit before its
// query (AccountState's trustline/offer reads run only for an EXISTING
// account).
func TestExplorerScanQueries_ExecuteAgainstServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	// A real, checksum-valid account with a real AccountEntry in the
	// current-state projection, so AccountState proceeds past the entry
	// lookup into the pinned trustline + offer scans.
	var seed [32]byte
	seed[0] = 0xE5
	account, err := strkey.Encode(strkey.VersionByteAccountID, seed[:])
	if err != nil {
		t.Fatalf("encode account strkey: %v", err)
	}
	var aid xdr.AccountId
	if err := aid.SetAddress(account); err != nil {
		t.Fatalf("set address: %v", err)
	}
	keyB64, err := xdr.MarshalBase64(xdr.LedgerKey{
		Type:    xdr.LedgerEntryTypeAccount,
		Account: &xdr.LedgerKeyAccount{AccountId: aid},
	})
	if err != nil {
		t.Fatalf("marshal account key: %v", err)
	}
	entryB64, err := xdr.MarshalBase64(xdr.LedgerEntry{
		LastModifiedLedgerSeq: 71_000_001,
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeAccount,
			Account: &xdr.AccountEntry{
				AccountId:  aid,
				Balance:    5_000_000,
				SeqNum:     7,
				Thresholds: xdr.Thresholds{1, 0, 0, 0},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal account entry: %v", err)
	}
	// The trustline + offer rows carry GENUINE LedgerKey XDR: the reader's
	// trustline/offer scans are PK-prefix range reads (`key_xdr LIKE
	// '<52-char real-XDR prefix>%'`, accountEntryKeyPrefix), so a synthetic
	// placeholder key can never match and would silently skip the very path
	// under test.
	var issuerSeed [32]byte
	issuerSeed[0] = 0xE6
	issuer, err := strkey.Encode(strkey.VersionByteAccountID, issuerSeed[:])
	if err != nil {
		t.Fatalf("encode issuer strkey: %v", err)
	}
	var issuerAID xdr.AccountId
	if err := issuerAID.SetAddress(issuer); err != nil {
		t.Fatalf("set issuer address: %v", err)
	}
	var code4 [4]byte
	copy(code4[:], "USDX")
	tlAsset := xdr.TrustLineAsset{
		Type:      xdr.AssetTypeAssetTypeCreditAlphanum4,
		AlphaNum4: &xdr.AlphaNum4{AssetCode: code4, Issuer: issuerAID},
	}
	assetID := "USDX-" + issuer
	tlKeyB64, err := xdr.MarshalBase64(xdr.LedgerKey{
		Type:      xdr.LedgerEntryTypeTrustline,
		TrustLine: &xdr.LedgerKeyTrustLine{AccountId: aid, Asset: tlAsset},
	})
	if err != nil {
		t.Fatalf("marshal trustline key: %v", err)
	}
	tlEntryB64, err := xdr.MarshalBase64(xdr.LedgerEntry{
		LastModifiedLedgerSeq: 71_000_001,
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeTrustline,
			TrustLine: &xdr.TrustLineEntry{
				AccountId: aid, Asset: tlAsset,
				Balance: 42, Limit: 1_000_000,
				Flags: xdr.Uint32(xdr.TrustLineFlagsAuthorizedFlag),
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal trustline entry: %v", err)
	}
	const offerID = xdr.Int64(9_001)
	offerKeyB64, err := xdr.MarshalBase64(xdr.LedgerKey{
		Type:  xdr.LedgerEntryTypeOffer,
		Offer: &xdr.LedgerKeyOffer{SellerId: aid, OfferId: offerID},
	})
	if err != nil {
		t.Fatalf("marshal offer key: %v", err)
	}
	offerEntryB64, err := xdr.MarshalBase64(xdr.LedgerEntry{
		LastModifiedLedgerSeq: 71_000_001,
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeOffer,
			Offer: &xdr.OfferEntry{
				SellerId: aid, OfferId: offerID,
				Selling: xdr.Asset{Type: xdr.AssetTypeAssetTypeNative},
				Buying: xdr.Asset{
					Type:      xdr.AssetTypeAssetTypeCreditAlphanum4,
					AlphaNum4: &xdr.AlphaNum4{AssetCode: code4, Issuer: issuerAID},
				},
				Amount: 1_500, Price: xdr.Price{N: 3, D: 2},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal offer entry: %v", err)
	}
	closeTime := time.Date(2024, 2, 2, 0, 0, 0, 0, time.UTC)
	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: 71_000_001, CloseTime: closeTime, TxHash: "e5a1", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "created", EntryType: "account",
			KeyXDR: keyB64, EntryXDR: entryB64, AccountID: account, Balance: 5_000_000,
		},
		{
			LedgerSeq: 71_000_001, CloseTime: closeTime, TxHash: "e5a1", OpIndex: 1, ChangeIndex: 0,
			IntraLedgerSeq: 2, ChangeType: "created", EntryType: "trustline",
			KeyXDR: tlKeyB64, EntryXDR: tlEntryB64, AccountID: account,
			Asset: assetID, Balance: 42,
		},
		{
			LedgerSeq: 71_000_001, CloseTime: closeTime, TxHash: "e5a1", OpIndex: 2, ChangeIndex: 0,
			IntraLedgerSeq: 3, ChangeType: "created", EntryType: "offer",
			KeyXDR: offerKeyB64, EntryXDR: offerEntryB64, AccountID: account,
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
	// The account-history readers refuse over an EMPTY ops_by_source, which
	// would skip their builders; one sourced row keeps them under test.
	raw := dialClickHouse(t, ctx, "stellar")
	if err := raw.Exec(ctx, `INSERT INTO stellar.ops_by_source
		(source_account, ledger_seq, tx_index, op_index) VALUES (?, 71000001, 0, 0)`, account); err != nil {
		t.Fatalf("seed ops_by_source: %v", err)
	}

	r, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	defer func() { _ = r.Close() }()

	contract, err := strkey.Encode(strkey.VersionByteContract, seed[:])
	if err != nil {
		t.Fatalf("encode contract strkey: %v", err)
	}
	cursor := chstore.ExplorerCursor{Ledger: 71_000_002, A: 1, B: 1}

	// Every pinned builder executes without a server-side parse/settings
	// error. Empty results are fine — validity, not content, is under test.
	for name, call := range map[string]func() error{
		"RecentOperations(first page)": func() error {
			_, err := r.RecentOperations(ctx, 5, chstore.ExplorerCursor{})
			return err
		},
		"RecentOperations(cursor)": func() error {
			_, err := r.RecentOperations(ctx, 5, cursor)
			return err
		},
		"RecentOperationsOfType(first page)": func() error {
			_, err := r.RecentOperationsOfType(ctx, 5, chstore.ExplorerCursor{}, []string{"OperationTypePayment", "OperationTypeClawback"})
			return err
		},
		"RecentOperationsOfType(cursor)": func() error {
			_, err := r.RecentOperationsOfType(ctx, 5, cursor, []string{"OperationTypePayment"})
			return err
		},
		"OperationTypeStats": func() error {
			_, err := r.OperationTypeStats(ctx, 0)
			return err
		},
		"AccountTransactions(first page)": func() error {
			_, _, err := r.AccountTransactions(ctx, account, 5, chstore.ExplorerCursor{})
			return err
		},
		"AccountTransactions(cursor)": func() error {
			_, _, err := r.AccountTransactions(ctx, account, 5, cursor)
			return err
		},
		"AccountOperations(first page)": func() error {
			_, _, err := r.AccountOperations(ctx, account, 5, chstore.ExplorerCursor{})
			return err
		},
		"AccountOperations(cursor)": func() error {
			_, _, err := r.AccountOperations(ctx, account, 5, cursor)
			return err
		},
		"ContractEventsRecent(first page)": func() error {
			_, err := r.ContractEventsRecent(ctx, contract, 5, chstore.ContractEventsCursor{})
			return err
		},
		"ContractEventsRecent(cursor)": func() error {
			_, err := r.ContractEventsRecent(ctx, contract, 5, chstore.ContractEventsCursor{
				Ledger: cursor.Ledger, TxHash: "ff", OpIndex: cursor.A, EventIndex: cursor.B,
			})
			return err
		},
		"RecentContracts": func() error {
			_, err := r.RecentContracts(ctx, 5, 0)
			return err
		},
		"ContractInteractions": func() error {
			_, _, err := r.ContractInteractions(ctx, contract, 5, 0)
			return err
		},
		"ContractCodeHistory": func() error {
			_, err := r.ContractCodeHistory(ctx, contract)
			return err
		},
		"AssetHolders": func() error {
			_, _, err := r.AssetHolders(ctx, assetID, 5)
			return err
		},
		"AccountsByWealth": func() error {
			_, err := r.AccountsByWealth(ctx, []string{"native"}, []string{"0.4"}, 5)
			return err
		},
		"AccountsUnspendable": func() error {
			_, err := r.AccountsUnspendable(ctx, []string{account})
			return err
		},
		"AccountMovements(filter+cursor)": func() error {
			_, err := r.AccountMovements(ctx, account, 5,
				chstore.AccountMovementCursor{Ledger: 71_000_002, TxHash: "ff"},
				chstore.AccountMovementFilter{Kind: "payment", Direction: chstore.AccountMovementSent, Asset: "native"})
			return err
		},
	} {
		if err := call(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	// The seeded account exercises the pinned trustline/offer scans through
	// AccountState and must resolve with its real balance + trustline.
	st, err := r.AccountState(ctx, account)
	if err != nil {
		t.Fatalf("AccountState: %v", err)
	}
	if !st.Exists || st.Balance != 5_000_000 {
		t.Errorf("AccountState exists=%v balance=%d, want the seeded entry (true, 5000000)", st.Exists, st.Balance)
	}
	if len(st.Trustlines) != 1 || st.Trustlines[0].Balance != 42 || st.Trustlines[0].Limit != 1_000_000 {
		t.Errorf("AccountState trustlines = %+v, want the seeded 42-balance / 1000000-limit trustline", st.Trustlines)
	}
	// The offer arm is the same PK-prefix range shape; the seeded offer's
	// real LedgerKey must round-trip through it.
	if len(st.Offers) != 1 || st.Offers[0].OfferID != 9_001 || st.Offers[0].Amount != 1_500 {
		t.Errorf("AccountState offers = %+v, want the seeded offer 9001 (amount 1500)", st.Offers)
	}

	// The seeded trustline also proves the holders board end to end.
	holders, total, err := r.AssetHolders(ctx, assetID, 5)
	if err != nil || total != 1 || len(holders) != 1 || holders[0].Balance != 42 {
		t.Errorf("AssetHolders = %v total=%d err=%v, want the one seeded holder", holders, total, err)
	}
}

// TestContractCodeHistory_ServesTheSeededTimeline asserts ContractCodeHistory's
// returned VALUES on a real server, down both of its paths: first served by
// the keyed contract_instance_changes index (populated by the shipped MV),
// then — with this contract's index rows deleted while the table stays
// non-empty — as an unproven per-contract miss: the index's
// emptiness for this contract is not proof it never upgraded, so the reader
// falls back to the changes-log scan, which still holds the instance writes
// and returns the same real timeline. The seeded timeline carries an
// instance-storage rewrite that keeps the executable (must collapse onto the
// FIRST ledger that installed it) and is inserted out of order (the read
// must sort by ledger).
func TestContractCodeHistory_ServesTheSeededTimeline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	var cidRaw, keeperRaw xdr.Hash
	copy(cidRaw[:], "t427-code-history-timeline-cid")
	copy(keeperRaw[:], "t427-code-history-keeper-cid")
	cid, keeper := xdr.ContractId(cidRaw), xdr.ContractId(keeperRaw)
	contract, err := strkey.Encode(strkey.VersionByteContract, cidRaw[:])
	if err != nil {
		t.Fatalf("encode contract strkey: %v", err)
	}
	h0, h1, hKeeper := t356Hash(0x70), t356Hash(0x71), t356Hash(0x72)
	instKey, entry0 := t356InstanceKeyAndEntry(t, cid, h0)
	_, entry1 := t356InstanceKeyAndEntry(t, cid, h1)
	keeperKey, keeperEntry := t356InstanceKeyAndEntry(t, keeper, hKeeper)

	const deploy, rewrite, upgrade = uint32(72_427_010), uint32(72_427_020), uint32(72_427_030)
	t1 := time.Date(2025, 4, 1, 0, 0, 10, 0, time.UTC)
	t2, t3 := t1.Add(10*time.Second), t1.Add(20*time.Second)
	rows := []chstore.LedgerEntryChangeRow{
		{
			LedgerSeq: upgrade, CloseTime: t3, TxHash: "t427-upgrade", IntraLedgerSeq: 5,
			ChangeType: "updated", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry1,
		},
		{
			LedgerSeq: deploy, CloseTime: t1, TxHash: "t427-deploy", IntraLedgerSeq: 2,
			ChangeType: "created", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry0,
		},
		{
			LedgerSeq: rewrite, CloseTime: t2, TxHash: "t427-storage", IntraLedgerSeq: 7,
			ChangeType: "updated", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry0,
		},
		{
			LedgerSeq: deploy, CloseTime: t1, TxHash: "t427-keeper", IntraLedgerSeq: 3,
			ChangeType: "created", EntryType: "contract_data", KeyXDR: keeperKey, EntryXDR: keeperEntry,
		},
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
	want := []chstore.ContractCodeVersion{
		{Ledger: deploy, CloseTime: t1, WasmHash: hex.EncodeToString(h0[:])},
		{Ledger: upgrade, CloseTime: t3, WasmHash: hex.EncodeToString(h1[:])},
	}

	conn := dialClickHouse(t, ctx, "stellar")
	cidHex, keeperHex := hex.EncodeToString(cidRaw[:]), hex.EncodeToString(keeperRaw[:])
	if n := countInstanceIndexRows(t, ctx, conn, cidHex); n != 3 {
		t.Fatalf("contract_instance_changes holds %d rows for the contract, want the 3 seeded writes", n)
	}
	assertCodeHistory(t, ctx, addr, contract, "index-served", want)

	// Index miss for this contract on a non-empty (so "available") index is
	// NOT authoritative: instanceChangesIndexAvailable only proves the table
	// isn't globally empty, not that this contract's backfill has landed, so
	// the reader must fall back to the ~30 s key_xdr scan of the changes log
	// and return its real (non-empty) answer.
	syncCtx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"mutations_sync": "2"}))
	if err := conn.Exec(syncCtx,
		`ALTER TABLE stellar.contract_instance_changes DELETE WHERE contract_hash = ?`, cidHex); err != nil {
		t.Fatalf("delete index rows: %v", err)
	}
	if n := countInstanceIndexRows(t, ctx, conn, cidHex); n != 0 {
		t.Fatalf("contract_instance_changes still holds %d rows for the contract after the delete", n)
	}
	if n := countInstanceIndexRows(t, ctx, conn, keeperHex); n != 1 {
		t.Fatalf("keeper contract has %d index rows, want 1 (the index must stay non-empty)", n)
	}
	assertCodeHistory(t, ctx, addr, contract, "index miss falls back to legacy scan", want)
}

// TestContractCodeHistory_KeepsOldExecutableBeyondRawWriteCap seeds more raw
// instance writes than contractCodeHistoryMaxRows, all after the contract's
// first executable (and a return to it): a raw-row cap would drop the oldest
// executable; collapsing before the cap must keep the whole A->B->A timeline.
func TestContractCodeHistory_KeepsOldExecutableBeyondRawWriteCap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	var cidRaw xdr.Hash
	copy(cidRaw[:], "inv1752-code-history-cap-cid")
	cid := xdr.ContractId(cidRaw)
	contract, err := strkey.Encode(strkey.VersionByteContract, cidRaw[:])
	if err != nil {
		t.Fatalf("encode contract strkey: %v", err)
	}
	hA, hB := t356Hash(0x80), t356Hash(0x81)
	instKey, entryA := t356InstanceKeyAndEntry(t, cid, hA)
	_, entryB := t356InstanceKeyAndEntry(t, cid, hB)

	const base, rewrites = uint32(73_752_000), 10_050
	t0 := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)
	row := func(seq uint32, entry string) chstore.LedgerEntryChangeRow {
		return chstore.LedgerEntryChangeRow{
			LedgerSeq: seq, CloseTime: t0.Add(time.Duration(seq-base) * time.Second),
			TxHash: fmt.Sprintf("inv1752-%d", seq), IntraLedgerSeq: 1,
			ChangeType: "updated", EntryType: "contract_data", KeyXDR: instKey, EntryXDR: entry,
		}
	}
	rows := []chstore.LedgerEntryChangeRow{row(base, entryA)}
	for i := uint32(1); i <= rewrites; i++ {
		rows = append(rows, row(base+i, entryB))
	}
	rows = append(rows, row(base+rewrites+1, entryA))
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	want := []chstore.ContractCodeVersion{
		{Ledger: base, CloseTime: t0, WasmHash: hex.EncodeToString(hA[:])},
		{Ledger: base + 1, CloseTime: t0.Add(time.Second), WasmHash: hex.EncodeToString(hB[:])},
		{Ledger: base + rewrites + 1, CloseTime: t0.Add((rewrites + 1) * time.Second), WasmHash: hex.EncodeToString(hA[:])},
	}
	assertCodeHistory(t, ctx, addr, contract, "index-served beyond the raw-write cap", want)
}

func countInstanceIndexRows(t *testing.T, ctx context.Context, conn driver.Conn, contractHash string) uint64 {
	t.Helper()
	var n uint64
	if err := conn.QueryRow(ctx, `SELECT count() FROM stellar.contract_instance_changes FINAL
		WHERE contract_hash = ?`, contractHash).Scan(&n); err != nil {
		t.Fatalf("count contract_instance_changes: %v", err)
	}
	return n
}

// assertCodeHistory reads through a FRESH reader so its index-availability
// probe reflects the table as it is now, not a cached earlier verdict.
func assertCodeHistory(t *testing.T, ctx context.Context, addr, contract, leg string, want []chstore.ContractCodeVersion) {
	t.Helper()
	r, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("%s: NewExplorerReader: %v", leg, err)
	}
	defer func() { _ = r.Close() }()
	got, err := r.ContractCodeHistory(ctx, contract)
	if err != nil {
		t.Fatalf("%s: ContractCodeHistory: %v", leg, err)
	}
	if len(got) != len(want) {
		t.Fatalf("%s: code history = %+v, want %+v", leg, got, want)
	}
	for i := range want {
		if got[i].Ledger != want[i].Ledger || got[i].WasmHash != want[i].WasmHash ||
			!got[i].CloseTime.Equal(want[i].CloseTime) {
			t.Fatalf("%s: code history[%d] = %+v, want %+v (full %+v)", leg, i, got[i], want[i], got)
		}
	}
}

// cap76Checkpoint is the first checkpoint after the CAP-0076 upgrade ledger.
const cap76Checkpoint = 59_501_311

// resetNetworkStateLake empties the tables these tests read, before and after,
// so other tests' fixture rows cannot enter the lumen sum or the lake tip.
func resetNetworkStateLake(t *testing.T, ctx context.Context) {
	t.Helper()
	raw := dialClickHouse(t, ctx, "stellar")
	truncate := func() {
		for _, table := range []string{"ledgers", "ledger_entry_changes", "ledger_entries_current"} {
			if err := raw.Exec(context.Background(), "TRUNCATE TABLE stellar."+table); err != nil {
				t.Errorf("truncate %s: %v", table, err)
			}
		}
	}
	truncate()
	t.Cleanup(truncate)
}

func insertNetworkStateLedger(t *testing.T, ctx context.Context, seq uint32, totalCoins, feePool int64) {
	t.Helper()
	raw := dialClickHouse(t, ctx, "stellar")
	if err := raw.Exec(ctx, `INSERT INTO stellar.ledgers
		(ledger_seq, close_time, ledger_hash, prev_hash, protocol_version, total_coins, fee_pool)
		VALUES (?, ?, ?, '00', 24, ?, ?)`,
		seq, time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC), fmt.Sprintf("%064d", seq), totalCoins, feePool); err != nil {
		t.Fatalf("insert ledger %d: %v", seq, err)
	}
}

// entryChange builds a ledger_entry_changes row in the extractor's encoding.
func entryChange(t *testing.T, seq, intra uint32, changeType string, e xdr.LedgerEntry) chstore.LedgerEntryChangeRow {
	t.Helper()
	k, err := e.LedgerKey()
	if err != nil {
		t.Fatalf("ledger key: %v", err)
	}
	entryType, key, err := chstore.LedgerKeyColumns(k)
	if err != nil {
		t.Fatal(err)
	}
	row := chstore.LedgerEntryChangeRow{
		LedgerSeq: seq, CloseTime: time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC), TxHash: "network-state",
		ChangeIndex: intra, IntraLedgerSeq: intra, ChangeType: changeType, EntryType: entryType, KeyXDR: key,
	}
	if changeType == "removed" {
		return row
	}
	if row.EntryXDR, err = xdr.MarshalBase64(e); err != nil {
		t.Fatal(err)
	}
	if a, ok := e.Data.GetAccount(); ok {
		row.Balance = int64(a.Balance)
	}
	return row
}

func insertEntryChanges(t *testing.T, ctx context.Context, rows ...chstore.LedgerEntryChangeRow) {
	t.Helper()
	if _, err := chstore.InsertEntryChanges(ctx, clickhouseAddr(t), rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}
}

func cap76Data(name string, val uint64) xdr.LedgerEntry {
	id := xdr.ContractId{0xca, 0x76, 0x1e}
	sym := xdr.ScSymbol(name)
	v := xdr.Uint64(val)
	return xdr.LedgerEntry{
		LastModifiedLedgerSeq: 59_000_000,
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.ContractDataEntry{
				Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &id},
				Key:        xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
				Durability: xdr.ContractDataDurabilityPersistent,
				Val:        xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &v},
			},
		},
	}
}

// writeHotArchiveFixture lays out a file:// history archive whose checkpoint
// HAS carries one hot-archive bucket holding entries.
func writeHotArchiveFixture(t *testing.T, entries ...xdr.LedgerEntry) string {
	t.Helper()
	dir := t.TempDir()
	listType := xdr.BucketListTypeHotArchive
	var raw bytes.Buffer
	if err := xdr.MarshalFramed(&raw, xdr.HotArchiveBucketEntry{
		Type:      xdr.HotArchiveBucketEntryTypeHotArchiveMetaentry,
		MetaEntry: &xdr.BucketMetadata{LedgerVersion: 24, Ext: xdr.BucketMetadataExt{V: 1, BucketListType: &listType}},
	}); err != nil {
		t.Fatal(err)
	}
	for i := range entries {
		if err := xdr.MarshalFramed(&raw, xdr.HotArchiveBucketEntry{
			Type: xdr.HotArchiveBucketEntryTypeHotArchiveArchived, ArchivedEntry: &entries[i],
		}); err != nil {
			t.Fatal(err)
		}
	}
	hash := historyarchive.Hash(sha256.Sum256(raw.Bytes()))
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(dir, historyarchive.BucketPath(hash)), gz.Bytes())

	has := historyarchive.HistoryArchiveState{Version: 2, CurrentLedger: cap76Checkpoint, NetworkPassphrase: network.PublicNetworkPassphrase}
	zero := strings.Repeat("0", 64)
	for i := range has.HotArchiveBuckets {
		has.CurrentBuckets[i].Curr, has.CurrentBuckets[i].Snap = zero, zero
		has.HotArchiveBuckets[i].Curr, has.HotArchiveBuckets[i].Snap = zero, zero
	}
	has.HotArchiveBuckets[0].Curr = hex.EncodeToString(hash[:])
	body, err := json.Marshal(has)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(dir, historyarchive.CategoryCheckpointPath("history", cap76Checkpoint)), body)
	writeFixtureFile(t, filepath.Join(dir, ".well-known", "stellar-history.json"), body)
	return "file://" + dir
}

func writeFixtureFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestVerifyNetworkState_CAP76AmendmentFailsOnAmendedKeys replays the
// CAP-0076 case end to end: the lake holds archived entries as last seen in
// ledger meta, the history archive's hot-archive bucket holds two of them
// amended. The verb must exit with exactly the two mismatches; an archive
// matching the lake must pass.
func TestVerifyNetworkState_CAP76AmendmentFailsOnAmendedKeys(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	resetNetworkStateLake(t, ctx)
	insertNetworkStateLedger(t, ctx, cap76Checkpoint, 0, 0)
	const evicted = 59_000_000
	insertEntryChanges(t, ctx,
		entryChange(t, evicted, 1, "updated", cap76Data("amended_a", 1)),
		entryChange(t, evicted, 2, "updated", cap76Data("amended_b", 2)),
		entryChange(t, evicted, 3, "updated", cap76Data("untouched", 3)),
	)

	cfgPath := filepath.Join(t.TempDir(), "stellarindex.toml")
	writeFixtureFile(t, cfgPath, []byte("[stellar]\nnetwork = \"pubnet\"\n"))
	run := func(archiveURL, textfile string) error {
		return chops.Run([]string{
			"verify-network-state", "-config", cfgPath, "-ch-addr", addr, "-archive", archiveURL,
			"-checks", "hotarchive", "-checkpoint", fmt.Sprint(cap76Checkpoint), "-textfile", textfile,
		})
	}

	amended := writeHotArchiveFixture(t, cap76Data("amended_a", 101), cap76Data("amended_b", 102), cap76Data("untouched", 3))
	prom := filepath.Join(t.TempDir(), "network_state_verify.prom")
	var exit *opsutil.ExitCodeError
	if err := run(amended, prom); !errors.As(err, &exit) || exit.Code != 2 {
		t.Fatalf("amended archive: err = %v, want exit code 2 (the two amended keys)", err)
	}
	body, err := os.ReadFile(prom)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`stellarindex_network_state_verify_failures{check="hot_archive"} 2`,
		`stellarindex_network_state_verify_hot_archive_entries{class="matched"} 1`,
		`stellarindex_network_state_verify_hot_archive_entries{class="mismatched"} 2`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("textfile missing %q:\n%s", want, body)
		}
	}

	clean := writeHotArchiveFixture(t, cap76Data("amended_a", 1), cap76Data("amended_b", 2), cap76Data("untouched", 3))
	if err := run(clean, filepath.Join(t.TempDir(), "clean.prom")); err != nil {
		t.Fatalf("archive matching the lake: %v", err)
	}
}

func lumenAccount(seed byte, balance int64) xdr.LedgerEntry {
	var pk xdr.Uint256
	pk[0] = seed
	id := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pk}
	return xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type:    xdr.LedgerEntryTypeAccount,
		Account: &xdr.AccountEntry{AccountId: id, Balance: xdr.Int64(balance)},
	}}
}

func lumenCB(seed byte, asset xdr.Asset, amount int64) xdr.LedgerEntry {
	return xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeClaimableBalance,
		ClaimableBalance: &xdr.ClaimableBalanceEntry{
			BalanceId: xdr.ClaimableBalanceId{Type: xdr.ClaimableBalanceIdTypeClaimableBalanceIdTypeV0, V0: &xdr.Hash{seed}},
			Asset:     asset,
			Amount:    xdr.Int64(amount),
		},
	}}
}

func lumenLP(native, credit xdr.Asset, reserveNative int64) xdr.LedgerEntry {
	return xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeLiquidityPool,
		LiquidityPool: &xdr.LiquidityPoolEntry{
			LiquidityPoolId: xdr.PoolId{0x1f},
			Body: xdr.LiquidityPoolEntryBody{
				Type: xdr.LiquidityPoolTypeLiquidityPoolConstantProduct,
				ConstantProduct: &xdr.LiquidityPoolEntryConstantProduct{
					Params:   xdr.LiquidityPoolConstantProductParameters{AssetA: native, AssetB: credit, Fee: 30},
					ReserveA: xdr.Int64(reserveNative), ReserveB: 999_999,
				},
			},
		},
	}}
}

func lumenSACBalance(sac xdr.ContractId, holder byte, amount int64) xdr.LedgerEntry {
	sym := xdr.ScSymbol("Balance")
	var pk xdr.Uint256
	pk[0] = holder
	acct := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pk}
	vec := xdr.ScVec{
		{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
		{Type: xdr.ScValTypeScvAddress, Address: &xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &acct}},
	}
	pv := &vec
	return xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.ContractDataEntry{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &sac},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pv},
			Durability: xdr.ContractDataDurabilityPersistent,
			Val:        xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Lo: xdr.Uint64(amount)}},
		},
	}}
}

// TestNetworkStateReader_LumenConservationAtCommittedTip proves the lumen
// tally against real ClickHouse: it sums every native holding domain as of
// the newest stellar.ledgers row and rolls back changes the sink wrote for a
// ledger whose header is not yet committed.
func TestNetworkStateReader_LumenConservationAtCommittedTip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	resetNetworkStateLake(t, ctx)

	sac := xdr.ContractId{0x5a, 0xc0}
	sacID, err := strkey.Encode(strkey.VersionByteContract, sac[:])
	if err != nil {
		t.Fatal(err)
	}
	native := xdr.Asset{Type: xdr.AssetTypeAssetTypeNative}
	credit := xdr.MustNewCreditAsset("USDC", "GC3C4AKRBQLHOJ45U4XG35ESVWRDECWO5XLDGYADO6DPR3L7KIDVUMML")

	const tip = 100
	insertNetworkStateLedger(t, ctx, tip-1, 1_000, 200)
	insertNetworkStateLedger(t, ctx, tip, 1_000, 220)
	insertEntryChanges(t, ctx,
		entryChange(t, 90, 1, "created", lumenAccount(1, 600)),
		entryChange(t, 91, 1, "created", lumenAccount(3, 7)),
		entryChange(t, 99, 1, "removed", lumenAccount(3, 0)),
		entryChange(t, 95, 1, "created", lumenCB(1, native, 100)),
		entryChange(t, 95, 2, "created", lumenCB(2, credit, 5_000)),
		entryChange(t, 95, 3, "created", lumenLP(native, credit, 30)),
		entryChange(t, 95, 4, "created", lumenSACBalance(sac, 1, 50)),
		entryChange(t, 95, 5, "created", lumenSACBalance(xdr.ContractId{0x5a, 0xc1}, 1, 9_999)),
		// Ledger 101's changes are in the lake but its header is not.
		entryChange(t, tip+1, 1, "state", lumenAccount(1, 600)),
		entryChange(t, tip+1, 2, "updated", lumenAccount(1, 400)),
		entryChange(t, tip+1, 3, "created", lumenAccount(2, 200)),
		entryChange(t, tip+1, 4, "state", lumenCB(1, native, 100)),
		entryChange(t, tip+1, 5, "removed", lumenCB(1, native, 0)),
	)

	reader, err := chstore.NewNetworkStateReader(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	got, err := reader.LumenConservation(ctx, sacID)
	if err != nil {
		t.Fatalf("LumenConservation: %v", err)
	}
	if got.Ledger != tip || got.TotalCoins != 1_000 || got.FeePool != 220 || got.Preimages != 3 {
		t.Fatalf("header/preimages = (%d, %d, %d, %d), want (%d, 1000, 220, 3)", got.Ledger, got.TotalCoins, got.FeePool, got.Preimages, tip)
	}
	for _, c := range []struct {
		name string
		got  *big.Int
		want int64
	}{
		{"accounts", got.Accounts, 600},
		{"claimable_balances", got.ClaimableBalances, 100},
		{"liquidity_pools", got.LiquidityPools, 30},
		{"contract_balances", got.ContractBalances, 50},
	} {
		if c.got.Cmp(big.NewInt(c.want)) != 0 {
			t.Errorf("%s = %s, want %d", c.name, c.got, c.want)
		}
	}
	if r := got.Residual(); r.Sign() != 0 {
		t.Errorf("residual = %s, want 0", r)
	}

	// The same lake under a header claiming 5 more stroops exist must not conserve.
	insertNetworkStateLedger(t, ctx, tip, 1_005, 220)
	got, err = reader.LumenConservation(ctx, sacID)
	if err != nil {
		t.Fatalf("LumenConservation: %v", err)
	}
	if r := got.Residual(); r.Cmp(big.NewInt(-5)) != 0 {
		t.Fatalf("residual = %s, want -5", r)
	}
}

func lumenSACAllowance(sac xdr.ContractId, amount int64) xdr.LedgerEntry {
	sym := xdr.ScSymbol("Allowance")
	return xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.ContractDataEntry{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &sac},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
			Durability: xdr.ContractDataDurabilityTemporary,
			Val:        xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Lo: xdr.Uint64(amount)}},
		},
	}}
}

// evictionRow is the lake walker's eviction row: a `removed` with no tx and no
// preceding `state`, so it has no pre-image in ledger_entry_changes.
func evictionRow(t *testing.T, seq, intra uint32, e xdr.LedgerEntry) chstore.LedgerEntryChangeRow {
	row := entryChange(t, seq, intra, "removed", e)
	row.TxHash, row.OpIndex, row.ChangeIndex = "", -1, 0
	return row
}

// TestNetworkStateReader_LumenConservationEvictionPastTip: a native SAC
// allowance evicted in a ledger whose header is not yet committed holds no
// lumens and must not abort the tally; an evicted balance with no pre-image
// still fails loud, since its lumens would otherwise vanish from the sum.
func TestNetworkStateReader_LumenConservationEvictionPastTip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	resetNetworkStateLake(t, ctx)

	sac := xdr.ContractId{0x5a, 0xc0}
	sacID, err := strkey.Encode(strkey.VersionByteContract, sac[:])
	if err != nil {
		t.Fatal(err)
	}
	const tip = 100
	insertNetworkStateLedger(t, ctx, tip, 1_050, 0)
	insertEntryChanges(t, ctx,
		entryChange(t, 90, 1, "created", lumenAccount(1, 1_000)),
		entryChange(t, 95, 1, "created", lumenSACBalance(sac, 1, 50)),
		entryChange(t, 95, 2, "created", lumenSACAllowance(sac, 7_777)),
		evictionRow(t, tip+1, 1, lumenSACAllowance(sac, 0)),
	)

	reader, err := chstore.NewNetworkStateReader(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	got, err := reader.LumenConservation(ctx, sacID)
	if err != nil {
		t.Fatalf("LumenConservation with an allowance evicted past the tip: %v", err)
	}
	if got.Ledger != tip || got.Preimages != 0 || got.ContractBalances.Cmp(big.NewInt(50)) != 0 || got.Residual().Sign() != 0 {
		t.Fatalf("tally = ledger %d, preimages %d, contract balances %s, residual %s; want %d, 0, 50, 0",
			got.Ledger, got.Preimages, got.ContractBalances, got.Residual(), tip)
	}

	insertEntryChanges(t, ctx, evictionRow(t, tip+1, 2, lumenSACBalance(sac, 1, 0)))
	if _, err := reader.LumenConservation(ctx, sacID); err == nil || !strings.Contains(err.Error(), "no pre-image") {
		t.Fatalf("balance removed past the tip with no pre-image: err = %v, want a no-pre-image error", err)
	}
}

func TestNetworkStateReader_CurrentEntries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	resetNetworkStateLake(t, ctx)
	live := entryChange(t, 96, 1, "updated", cap76Data("live", 2))
	gone := entryChange(t, 97, 1, "removed", cap76Data("gone", 0))
	insertEntryChanges(t, ctx,
		entryChange(t, 95, 1, "created", cap76Data("live", 1)),
		live,
		entryChange(t, 90, 1, "created", cap76Data("gone", 1)),
		gone,
	)
	reader, err := chstore.NewNetworkStateReader(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	got, err := reader.CurrentEntries(ctx, "contract_data", []string{live.KeyXDR, gone.KeyXDR, "absent-key"})
	if err != nil {
		t.Fatalf("CurrentEntries: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got), got)
	}
	if e := got[live.KeyXDR]; e.LedgerSeq != 96 || e.ChangeType != "updated" || e.EntryXDR != live.EntryXDR {
		t.Errorf("live key resolved to %+v, want the ledger-96 update", e)
	}
	if e := got[gone.KeyXDR]; e.LedgerSeq != 97 || e.ChangeType != "removed" {
		t.Errorf("removed key resolved to %+v, want the ledger-97 removal", e)
	}
}

// TestGetSourceStatsFoldsFlippedOrientation pins that a source
// that printed the same market in both stored orientations (native/USDC
// and USDC/native) must be counted as ONE market, not two. Before the
// fix, GetSourceStats grouped its per_pair CTE on the raw (base_asset,
// quote_asset) columns with no canonical-orientation fold, so
// MarketsCount24h double-counted a flipped pair.
func TestGetSourceStatsFoldsFlippedOrientation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()

	const usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	now := time.Now().UTC()

	// Same source, same market, opposite stored orientations, both
	// inside the 24h window.
	seedGrainTrade(t, ctx, db, 1, "sdex", now.Add(-30*time.Minute), "native", usdc, "1", "0.5", "1")
	seedGrainTrade(t, ctx, db, 2, "sdex", now.Add(-20*time.Minute), usdc, "native", "0.5", "1", "1")

	stats, err := store.GetSourceStats(ctx)
	if err != nil {
		t.Fatalf("GetSourceStats: %v", err)
	}
	var row *timescale.SourceStats
	for i := range stats {
		if stats[i].Source == "sdex" {
			row = &stats[i]
		}
	}
	if row == nil {
		t.Fatalf("sdex missing from GetSourceStats: %+v", stats)
	}
	if row.MarketsCount24h != 1 {
		t.Errorf("sdex MarketsCount24h = %d, want 1 (native/USDC and USDC/native are the same market)", row.MarketsCount24h)
	}
	if row.TradeCount24h != 2 {
		t.Errorf("sdex TradeCount24h = %d, want 2 (both prints still counted)", row.TradeCount24h)
	}
}

// TestGetNetworkStatsFoldsFlippedOrientation pins the same defect
// in GetNetworkStats's MarketsCount24h: the DISTINCT over
// prices_1m must fold a market's two stored orientations before
// counting.
func TestGetNetworkStatsFoldsFlippedOrientation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()

	const usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	now := time.Now().UTC()

	seedGrainTrade(t, ctx, db, 101, "sdex", now.Add(-30*time.Minute), "native", usdc, "1", "0.5", "1")
	seedGrainTrade(t, ctx, db, 102, "soroswap", now.Add(-20*time.Minute), usdc, "native", "0.5", "1", "1")

	if _, err := db.ExecContext(ctx,
		`CALL refresh_continuous_aggregate('prices_1m', NULL, NULL)`,
	); err != nil {
		t.Fatalf("refresh cagg prices_1m: %v", err)
	}

	before, err := store.GetNetworkStats(ctx)
	if err != nil {
		t.Fatalf("GetNetworkStats: %v", err)
	}
	if before.MarketsCount24h != 1 {
		t.Errorf("MarketsCount24h = %d, want 1 (native/USDC on two venues, both orientations, is one market)", before.MarketsCount24h)
	}
}

// TestGetNetworkStatsLatestLedgerReadsOnlyLiveCursors pins that the
// home page's latest_ledger must be the live tip, not the highest
// ledger any one-shot job's shard cursor ever reached. Each job writes
// its own namespace ("census-backfill", "projected-rebuild", …), and a
// denylist of the literal "backfill" counted every one of them as live.
func TestGetNetworkStatsLatestLedgerReadsOnlyLiveCursors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cursors := []struct {
		source, sub string
		ledger      uint32
	}{
		{"ledgerstream", "", 62_000_000},
		{"projector", "blend", 61_999_990},
		{"backfill", "0-70000000:sdex", 70_000_000},
		{"census-backfill", "shard-3", 69_000_000},
		{"projected-rebuild", "soroswap:60000000-68000000", 68_000_000},
		{"tag-signer", "shard-1", 67_000_000},
	}
	for _, c := range cursors {
		if err := store.UpsertCursor(ctx, c.source, c.sub, c.ledger); err != nil {
			t.Fatalf("UpsertCursor(%s,%s): %v", c.source, c.sub, err)
		}
	}

	got, err := store.GetNetworkStats(ctx)
	if err != nil {
		t.Fatalf("GetNetworkStats: %v", err)
	}
	if got.LatestLedger != 62_000_000 {
		t.Errorf("LatestLedger = %d, want 62000000 (the ledgerstream tip; one-shot job cursors are not live)", got.LatestLedger)
	}
}

// TestUsageDailyBatchUpsertSpansChunks drives the chunked multi-row
// upsert against real Postgres: a batch larger than two chunks, with a
// duplicate key inside it, lands every row exactly once with the
// per-column maximum — the GREATEST contract the single-row statement
// gave, now over a statement Postgres would reject if the duplicate
// reached it unfolded.
func TestUsageDailyBatchUpsertSpansChunks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	day := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	const subjects = 1201
	rows := make([]usage.RollupRow, 0, subjects+1)
	for i := 0; i < subjects; i++ {
		rows = append(rows, usage.RollupRow{
			Day: day, Subject: fmt.Sprintf("id:acct:batch-%d", i), Endpoint: "/v1/price",
			OK: int64(i + 1), ClientErrors: 2,
		})
	}
	// A second snapshot of subject 7 with a larger OK but smaller 4xx:
	// the row must end on the maximum of each column, 8 / 2.
	rows = append(rows, usage.RollupRow{
		Day: day, Subject: "id:acct:batch-7", Endpoint: "/v1/price", OK: 8, ClientErrors: 1,
	})
	if err := store.UpsertUsageDaily(ctx, rows); err != nil {
		t.Fatalf("UpsertUsageDaily: %v", err)
	}
	// A second identical sweep must be a no-op (idempotent across chunks).
	if err := store.UpsertUsageDaily(ctx, rows); err != nil {
		t.Fatalf("UpsertUsageDaily replay: %v", err)
	}

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM usage_daily WHERE subject LIKE 'id:acct:batch-%'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != subjects {
		t.Fatalf("usage_daily holds %d batch rows, want %d", n, subjects)
	}
	for _, tc := range []struct {
		subject string
		ok, c4  int64
	}{
		{"id:acct:batch-0", 1, 2},
		{"id:acct:batch-7", 8, 2},
		{"id:acct:batch-500", 501, 2},
		{"id:acct:batch-1200", 1201, 2},
	} {
		got, err := store.ReadUsageDaily(ctx, tc.subject, 7)
		if err != nil {
			t.Fatalf("ReadUsageDaily(%s): %v", tc.subject, err)
		}
		if len(got) != 1 || got[0].OK != tc.ok || got[0].ClientErrors != tc.c4 {
			t.Errorf("%s = %+v, want one row ok=%d client=%d", tc.subject, got, tc.ok, tc.c4)
		}
	}
}

// TestUsageDailyBillableByDay drives the SQL the month-to-date meter
// reconciles evicted Redis day keys against: per-day billable
// units are ok + 4xx summed over endpoints, never 5xx or 429, for one
// subject, inside an inclusive [from, to] window.
func TestUsageDailyBillableByDay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const subj = "id:acct:acme"
	rows := []usage.RollupRow{
		{Day: "2026-04-30", Subject: subj, Endpoint: "/v1/price", OK: 1000},
		{Day: "2026-05-01", Subject: subj, Endpoint: "/v1/price", OK: 300, ClientErrors: 20, ServerErrors: 7, Throttled: 9},
		{Day: "2026-05-01", Subject: subj, Endpoint: "/v1/assets", OK: 80},
		{Day: "2026-05-03", Subject: subj, Endpoint: "/v1/price", OK: 5, ClientErrors: 1},
		{Day: "2026-05-04", Subject: subj, Endpoint: "/v1/price", OK: 500},
		{Day: "2026-05-01", Subject: "id:acct:other", Endpoint: "/v1/price", OK: 777},
	}
	if err := store.UpsertUsageDaily(ctx, rows); err != nil {
		t.Fatalf("UpsertUsageDaily: %v", err)
	}

	got, err := store.BillableByDay(ctx, subj, "2026-05-01", "2026-05-03")
	if err != nil {
		t.Fatalf("BillableByDay: %v", err)
	}
	want := map[string]int64{"2026-05-01": 400, "2026-05-03": 6}
	if !maps.Equal(got, want) {
		t.Errorf("BillableByDay = %v, want %v", got, want)
	}
}

// TestUsageDailyRetention pins that `usage_daily`, which holds per-account
// request history, is not kept forever. 0167 must attach exactly one
// armed 12-month retention job, and its down must remove it.
func TestUsageDailyRetention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	applyMigrationsUpTo(t, dsn, 165)
	if n, _, _ := usageDailyRetention(t, ctx, db); n != 0 {
		t.Fatalf("pre-0167 usage_daily retention jobs = %d, want 0", n)
	}

	applyMigrationsUpTo(t, dsn, 167)
	n, dropAfter, scheduled := usageDailyRetention(t, ctx, db)
	if n != 1 {
		t.Fatalf("usage_daily retention jobs after 0167 = %d, want 1", n)
	}
	if dropAfter != "1 year" {
		t.Errorf("usage_daily drop_after = %q, want \"1 year\" (12 months)", dropAfter)
	}
	if !scheduled {
		t.Error("usage_daily retention job is not scheduled — 0167 ships it armed")
	}

	applyMigrationsUpTo(t, dsn, 165)
	if n, _, _ := usageDailyRetention(t, ctx, db); n != 0 {
		t.Fatalf("usage_daily retention jobs after 0167 down = %d, want 0", n)
	}
}

func usageDailyRetention(t *testing.T, ctx context.Context, db *sql.DB) (jobs int, dropAfter string, scheduled bool) {
	t.Helper()
	const q = `
		SELECT COUNT(*),
		       COALESCE(MAX(config->>'drop_after'), ''),
		       COALESCE(BOOL_AND(scheduled), false)
		  FROM timescaledb_information.jobs
		 WHERE proc_name = 'policy_retention'
		   AND hypertable_name = 'usage_daily'`
	if err := db.QueryRowContext(ctx, q).Scan(&jobs, &dropAfter, &scheduled); err != nil {
		t.Fatalf("read usage_daily retention job: %v", err)
	}
	return jobs, dropAfter, scheduled
}

// TestUsageDailyRollupReSweepIdempotent pins the billing-adjacent
// invariant behind GET /v1/account/usage: the 5-minute usage-rollup
// worker (internal/usage.Rollup.Sweep) hands the sink the FULL
// CUMULATIVE per-(day, subject, endpoint) counters on EVERY sweep — it
// deliberately never resets or checkpoints the Redis source. That is
// only safe because timescale.Store.UpsertUsageDaily merges with
// GREATEST(existing, incoming), NOT additively. If that merge were ever
// flipped to `col = usage_daily.col + EXCLUDED.col`, every 5-minute
// sweep would RE-ADD the cumulative value and usage_daily would report
// k*N requests for N real requests — a permanent, non-self-correcting
// over-count on the surface a metered plan bills against.
//
// The unit suite (internal/usage/rollup_test.go) only proves the sweep
// hands the same cumulative batch on replay, then delegates: "idempotence
// is then the sink's GREATEST()-merge contract." Nothing exercised that
// contract against real Postgres until this test — so a regression that
// broke it would ship green. This drives the REAL SQL and asserts N, not
// kN. Flip either GREATEST to `+` in usage_daily.go and this goes red.
func TestUsageDailyRollupReSweepIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// day is now-relative: ReadUsageDaily reads a trailing now-anchored
	// window, so a hardcoded date is a calendar time-bomb — the original
	// window, so a hardcoded date ages out of the window and the test starts
	// failing untouched.
	day := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	const subj = "key:kid_bill_1"

	// One day of real traffic to /v1/price for one subject: 100 ok, 5 4xx,
	// 2 5xx, 3 throttled. These are the CUMULATIVE per-day counters the
	// Redis detail hash holds at sweep time.
	batch := []usage.RollupRow{{
		Day: day, Subject: subj, Endpoint: "/v1/price",
		OK: 100, ClientErrors: 5, ServerErrors: 2, Throttled: 3,
	}}

	// The worker sweeps the SAME cumulative batch every 5 minutes with no
	// new traffic in between. Six sweeps == 30 minutes of re-folding.
	for i := 0; i < 6; i++ {
		if err := store.UpsertUsageDaily(ctx, batch); err != nil {
			t.Fatalf("sweep %d UpsertUsageDaily: %v", i, err)
		}
	}

	rows, err := store.ReadUsageDaily(ctx, subj, 7)
	if err != nil {
		t.Fatalf("ReadUsageDaily: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ReadUsageDaily returned %d rows, want 1 (one endpoint, one day): %+v", len(rows), rows)
	}
	got := rows[0]
	// EXACTLY the single-sweep values — NOT 6x. An additive merge would
	// yield ok=600, client=30, server=12, throttled=18 here.
	if got.OK != 100 {
		t.Errorf("ok_count = %d after 6 sweeps, want 100 (additive over-count would give 600)", got.OK)
	}
	if got.ClientErrors != 5 {
		t.Errorf("client_error_count = %d, want 5 (additive would give 30)", got.ClientErrors)
	}
	if got.ServerErrors != 2 {
		t.Errorf("server_error_count = %d, want 2 (additive would give 12)", got.ServerErrors)
	}
	if got.Throttled != 3 {
		t.Errorf("throttled_count = %d, want 3 (additive would give 18)", got.Throttled)
	}
	// The wire-shape total /v1/account/usage serves: requests = ok + 4xx + 5xx.
	if reqs := got.OK + got.ClientErrors + got.ServerErrors; reqs != 107 {
		t.Errorf("wire requests = %d, want 107 (billing over-count if higher)", reqs)
	}
}

// TestUsageDailyRollupWithinDayGrowth pins the other half of the
// GREATEST contract: WITHIN a day the Redis counters only grow, so each
// sweep's cumulative value is >= the last. GREATEST must keep the LATEST
// (largest) value, never the sum of the partial snapshots. A sweep at
// t=5m sees 50 ok; a sweep at t=10m sees the full 100 ok. The persisted
// row must read 100 — additive would read 150.
func TestUsageDailyRollupWithinDayGrowth(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	day := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	const subj = "key:kid_bill_2"

	// Sweep 1: half a day's traffic captured.
	if err := store.UpsertUsageDaily(ctx, []usage.RollupRow{{
		Day: day, Subject: subj, Endpoint: "/v1/assets/{asset_id}", OK: 50,
	}}); err != nil {
		t.Fatalf("sweep 1: %v", err)
	}
	// Sweep 2: the day's full cumulative total (the counter grew to 100).
	if err := store.UpsertUsageDaily(ctx, []usage.RollupRow{{
		Day: day, Subject: subj, Endpoint: "/v1/assets/{asset_id}", OK: 100,
	}}); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	// Sweep 3: a late-arriving Redis snapshot that is SMALLER (e.g. the
	// detail hash partially expired, or a mid-day flush). GREATEST must
	// hold the row at 100 and never regress to 40.
	if err := store.UpsertUsageDaily(ctx, []usage.RollupRow{{
		Day: day, Subject: subj, Endpoint: "/v1/assets/{asset_id}", OK: 40,
	}}); err != nil {
		t.Fatalf("sweep 3: %v", err)
	}

	rows, err := store.ReadUsageDaily(ctx, subj, 7)
	if err != nil {
		t.Fatalf("ReadUsageDaily: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ReadUsageDaily returned %d rows, want 1: %+v", len(rows), rows)
	}
	if got := rows[0].OK; got != 100 {
		t.Errorf("ok_count = %d, want 100 (GREATEST of 50/100/40; additive would give 190, regress would give 40)", got)
	}
}

// gAccountFromSeed returns a strkey-valid 56-char G-address whose
// ed25519 key's first byte is `seed`. Deterministic for a given
// seed so fixture assertions stay reproducible.
func gAccountFromSeed(t *testing.T, seed byte) string {
	t.Helper()
	var raw [32]byte
	raw[0] = seed
	s, err := strkey.Encode(strkey.VersionByteAccountID, raw[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	return s
}

// TestStoreRoundTrip exercises the trade / oracle / cursor paths
// through a real TimescaleDB with our migrations applied. This is
// the first end-to-end "write → read" proof of the Go storage
// layer.
func TestStoreRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// ─── Trades ─────────────────────────────────────────────────
	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, _ := c.NewPair(c.NativeAsset(), usdc)

	tr := c.Trade{
		Source:      "sdex",
		Ledger:      52_430_001,
		TxHash:      "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe",
		OpIndex:     0,
		Timestamp:   time.Now().UTC().Truncate(time.Second),
		Pair:        pair,
		BaseAmount:  c.NewAmount(big.NewInt(1_000_000_000)), // 100 XLM in stroops
		QuoteAmount: c.NewAmount(big.NewInt(12_420_000)),    // 12.42 USDC
		Maker:       "maker-acc",
		Taker:       "taker-acc",
	}

	if err := store.InsertTrade(ctx, tr); err != nil {
		t.Fatalf("InsertTrade: %v", err)
	}
	// Idempotent re-insert should not error (ON CONFLICT DO NOTHING).
	if err := store.InsertTrade(ctx, tr); err != nil {
		t.Fatalf("InsertTrade (duplicate): %v", err)
	}

	n, err := store.CountTrades(ctx)
	if err != nil || n != 1 {
		t.Fatalf("CountTrades = %d, err=%v — want 1 row after duplicate-insert", n, err)
	}

	latest, err := store.LatestTradesForPair(ctx, pair, 5)
	if err != nil {
		t.Fatalf("LatestTradesForPair: %v", err)
	}
	if len(latest) != 1 {
		t.Fatalf("expected 1 trade, got %d", len(latest))
	}
	got := latest[0]
	if !got.Equal(tr) {
		t.Fatalf("trade identity not preserved: %+v", got)
	}
	if got.BaseAmount.Cmp(tr.BaseAmount) != 0 {
		t.Errorf("base_amount lost: got %s want %s", got.BaseAmount, tr.BaseAmount)
	}
	if got.QuoteAmount.Cmp(tr.QuoteAmount) != 0 {
		t.Errorf("quote_amount lost: got %s want %s", got.QuoteAmount, tr.QuoteAmount)
	}

	// ─── Oracle updates ─────────────────────────────────────────
	price, _ := new(big.Int).SetString("1242000000000000", 10)
	up := c.OracleUpdate{
		Source:     "reflector-dex",
		ContractID: "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		Ledger:     52_430_001,
		TxHash:     "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe",
		OpIndex:    0,
		Timestamp:  time.Now().UTC().Truncate(time.Second),
		Asset:      c.NativeAsset(),
		Quote:      usdc,
		Price:      c.NewAmount(price),
		Decimals:   14,
		Confidence: 0.95,
		// OracleUpdate.Validate requires a checksum-valid Observer strkey.
		// Hand-crafted "GRELAYER_FAKE" is 13 chars (expected 56),
		// so it was rejected after canonical tightened validation.
		// Generate a checksum-valid G-address from a deterministic
		// seed instead.
		Observer: gAccountFromSeed(t, 0xAA),
	}
	if err := store.InsertOracleUpdate(ctx, up); err != nil {
		t.Fatalf("InsertOracleUpdate: %v", err)
	}

	gotUp, err := store.LatestOracleUpdateForAsset(ctx, "reflector-dex", c.NativeAsset())
	if err != nil {
		t.Fatalf("LatestOracleUpdateForAsset: %v", err)
	}
	if !gotUp.Equal(up) {
		t.Fatalf("oracle identity lost: %+v", gotUp)
	}
	if gotUp.Price.Cmp(up.Price) != 0 {
		t.Errorf("price lost: got %s want %s", gotUp.Price, up.Price)
	}
	if gotUp.Decimals != 14 {
		t.Errorf("decimals lost: got %d want 14", gotUp.Decimals)
	}

	// Not-found path.
	_, err = store.LatestOracleUpdateForAsset(ctx, "reflector-dex", usdc)
	if err == nil {
		t.Fatal("expected ErrNotFound for USDC (never inserted for this source)")
	}

	// ─── Cursors ────────────────────────────────────────────────
	if err := store.UpsertCursor(ctx, "soroswap", "", 52_430_001); err != nil {
		t.Fatalf("UpsertCursor: %v", err)
	}
	cur, err := store.GetCursor(ctx, "soroswap", "")
	if err != nil {
		t.Fatalf("GetCursor: %v", err)
	}
	if cur.LastLedger != 52_430_001 {
		t.Errorf("cursor lost: got %d", cur.LastLedger)
	}

	// Update path.
	if err := store.UpsertCursor(ctx, "soroswap", "", 52_430_100); err != nil {
		t.Fatal(err)
	}
	cur, _ = store.GetCursor(ctx, "soroswap", "")
	if cur.LastLedger != 52_430_100 {
		t.Errorf("cursor update lost: got %d", cur.LastLedger)
	}

	// Second subsource for the same source shouldn't interfere.
	if err := store.UpsertCursor(ctx, "soroswap", "pair:CAB...", 99); err != nil {
		t.Fatal(err)
	}
	cur, _ = store.GetCursor(ctx, "soroswap", "pair:CAB...")
	if cur.LastLedger != 99 {
		t.Errorf("sub cursor wrong: got %d", cur.LastLedger)
	}
	cur, _ = store.GetCursor(ctx, "soroswap", "")
	if cur.LastLedger != 52_430_100 {
		t.Errorf("root cursor wrong after sub insert: got %d", cur.LastLedger)
	}

	// ─── ListCursors ────────────────────────────────────────────
	// After the upserts above we have 2 cursors: soroswap/"" and
	// soroswap/"pair:CAB...". ListCursors returns both, sorted by
	// (source, sub_source).
	all, err := store.ListCursors(ctx)
	if err != nil {
		t.Fatalf("ListCursors: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("ListCursors returned %d, want 2", len(all))
	}
	if all[0].Source != "soroswap" || all[0].Sub != "" {
		t.Errorf("ListCursors[0] = %+v, want soroswap/\"\"", all[0])
	}
	if all[1].Source != "soroswap" || all[1].Sub != "pair:CAB..." {
		t.Errorf("ListCursors[1] = %+v, want soroswap/pair:CAB...", all[1])
	}
	// UpdatedAt must be populated by the server-side now() call.
	for _, c := range all {
		if c.UpdatedAt.IsZero() {
			t.Errorf("cursor %s/%s has zero UpdatedAt", c.Source, c.Sub)
		}
	}

	// ─── Cursor monotonic-advance guard ─────────────────────────
	// DB-level refusal to regress last_ledger (ON CONFLICT DO UPDATE
	// ... WHERE EXCLUDED.last_ledger > ingestion_cursors.last_ledger).
	// Defense in depth: the orchestrator's Go-level advance-only rule
	// can't be the only line of defense for a misconfigured two-
	// indexer race.
	if err := store.UpsertCursor(ctx, "soroswap", "", 10_000); err != nil {
		t.Fatalf("UpsertCursor (regression attempt): %v", err)
	}
	cur, _ = store.GetCursor(ctx, "soroswap", "")
	if cur.LastLedger != 52_430_100 {
		t.Errorf("regression-attempt should have been ignored; got %d, want 52430100",
			cur.LastLedger)
	}
	// Equal-value attempt also no-ops (WHERE > , not >=).
	if err := store.UpsertCursor(ctx, "soroswap", "", 52_430_100); err != nil {
		t.Fatalf("UpsertCursor (same value): %v", err)
	}
	cur, _ = store.GetCursor(ctx, "soroswap", "")
	if cur.LastLedger != 52_430_100 {
		t.Errorf("same-value upsert shouldn't change stored cursor")
	}
	// Advancement still works.
	if err := store.UpsertCursor(ctx, "soroswap", "", 52_430_200); err != nil {
		t.Fatal(err)
	}
	cur, _ = store.GetCursor(ctx, "soroswap", "")
	if cur.LastLedger != 52_430_200 {
		t.Errorf("advance after regression-attempt lost: got %d", cur.LastLedger)
	}

	// ─── first_ledger semantics (migration 0046) ────────────────
	// The cursor we created above started at last_ledger=52_430_001;
	// migration 0046 should have captured that as first_ledger on
	// the INSERT branch of UpsertCursor and preserved it across
	// every subsequent advance.
	if cur.FirstLedger != 52_430_001 {
		t.Errorf("FirstLedger not captured on insert / drifted across updates: got %d, want 52430001",
			cur.FirstLedger)
	}

	// A brand-new (source, sub) pair: first_ledger == last_ledger
	// on the very first write.
	if err := store.UpsertCursor(ctx, "phoenix", "", 60_000_000); err != nil {
		t.Fatalf("UpsertCursor phoenix: %v", err)
	}
	phoenixCur, err := store.GetCursor(ctx, "phoenix", "")
	if err != nil {
		t.Fatalf("GetCursor phoenix: %v", err)
	}
	if phoenixCur.FirstLedger != 60_000_000 {
		t.Errorf("fresh cursor FirstLedger = %d, want 60000000", phoenixCur.FirstLedger)
	}
	if phoenixCur.LastLedger != 60_000_000 {
		t.Errorf("fresh cursor LastLedger = %d, want 60000000", phoenixCur.LastLedger)
	}

	// Advance the phoenix cursor and confirm FirstLedger sticks
	// at the original value — the SET clause must NOT touch it.
	if err := store.UpsertCursor(ctx, "phoenix", "", 60_500_000); err != nil {
		t.Fatalf("UpsertCursor phoenix advance: %v", err)
	}
	phoenixCur, _ = store.GetCursor(ctx, "phoenix", "")
	if phoenixCur.FirstLedger != 60_000_000 {
		t.Errorf("FirstLedger drifted on advance: got %d, want 60000000 (anchor must be preserved)",
			phoenixCur.FirstLedger)
	}
	if phoenixCur.LastLedger != 60_500_000 {
		t.Errorf("LastLedger after advance = %d, want 60500000", phoenixCur.LastLedger)
	}
}

// TestCursorFirstLedgerBackfillMigration verifies migration 0046's
// backfill — for every existing backfill cursor at the time the
// migration ran, first_ledger should equal the `from` integer parsed
// out of sub_source. The rows are seeded at schema version 45 and the
// real 0046 up file is then applied, so the test cannot drift from
// the migration's SQL.
func TestCursorFirstLedgerBackfillMigration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrationsUpTo(t, dsn, 45)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	_, err = db.ExecContext(ctx,
		// 51000000 written without the PG16 underscore digit separator —
		// the pinned image is timescale/timescaledb:…-pg15, where
		// `51_000_000` is a syntax error.
		`INSERT INTO ingestion_cursors (source, sub_source, last_ledger)
		   VALUES ('backfill', '50500000-53174999:soroswap', 51000000)`,
	)
	if err != nil {
		t.Fatalf("insert pre-migration row: %v", err)
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO ingestion_cursors (source, sub_source, last_ledger)
		   VALUES ('backfill', 'malformed-no-decoder', 1)`,
	)
	if err != nil {
		t.Fatalf("insert malformed row: %v", err)
	}

	applyMigrationsUpTo(t, dsn, 46)

	var affected int64
	err = db.QueryRowContext(ctx,
		`SELECT count(*) FROM ingestion_cursors
		  WHERE source = 'backfill' AND first_ledger IS NOT NULL`,
	).Scan(&affected)
	if err != nil {
		t.Fatalf("count backfilled rows: %v", err)
	}
	if affected != 1 {
		t.Errorf("migration backfilled %d rows, want 1 (only the soroswap range matches the regex)", affected)
	}

	// Verify the soroswap range got 50500000.
	var firstLedger sql.NullInt64
	err = db.QueryRowContext(ctx,
		`SELECT first_ledger FROM ingestion_cursors
		  WHERE source = 'backfill' AND sub_source = '50500000-53174999:soroswap'`,
	).Scan(&firstLedger)
	if err != nil {
		t.Fatalf("select soroswap first_ledger: %v", err)
	}
	if !firstLedger.Valid || firstLedger.Int64 != 50_500_000 {
		t.Errorf("first_ledger = %v, want 50500000", firstLedger)
	}

	// Malformed sub_source: regex filter skipped it, first_ledger
	// stays NULL (better than silently writing 0).
	err = db.QueryRowContext(ctx,
		`SELECT first_ledger FROM ingestion_cursors
		  WHERE source = 'backfill' AND sub_source = 'malformed-no-decoder'`,
	).Scan(&firstLedger)
	if err != nil {
		t.Fatalf("select malformed first_ledger: %v", err)
	}
	if firstLedger.Valid {
		t.Errorf("malformed first_ledger = %v, want NULL", firstLedger.Int64)
	}
}

// TestCursorFirstLedgerMigrationReversible verifies migration 0046
// can be rolled back without data loss on the rest of the table —
// dropping the column doesn't disturb existing (source, sub_source,
// last_ledger) rows.
func TestCursorFirstLedgerMigrationReversible(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Seed a row via the production path so first_ledger is set.
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = store.Close() }()
	if err := store.UpsertCursor(ctx, "comet", "", 51_500_000); err != nil {
		t.Fatalf("UpsertCursor: %v", err)
	}

	// Roll the column back via the down migration's DROP COLUMN.
	if _, err := db.ExecContext(ctx, `ALTER TABLE ingestion_cursors DROP COLUMN first_ledger`); err != nil {
		t.Fatalf("down migration ALTER DROP COLUMN: %v", err)
	}

	// last_ledger should still be there.
	var lastLedger int64
	err = db.QueryRowContext(ctx,
		`SELECT last_ledger FROM ingestion_cursors WHERE source = 'comet' AND sub_source = ''`,
	).Scan(&lastLedger)
	if err != nil {
		t.Fatalf("select last_ledger after down: %v", err)
	}
	if lastLedger != 51_500_000 {
		t.Errorf("post-rollback last_ledger = %d, want 51500000", lastLedger)
	}

	// Column should be gone.
	var exists bool
	err = db.QueryRowContext(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM information_schema.columns
		   WHERE table_name = 'ingestion_cursors' AND column_name = 'first_ledger'
		)
	`).Scan(&exists)
	if err != nil {
		t.Fatalf("information_schema check: %v", err)
	}
	if exists {
		t.Error("first_ledger column still present after down migration")
	}
}

// TestInsertTrade_MultiOpSameTxBothLand covers the most common
// real-world pattern that would have caught the Aquarius fanout
// bug: a single Soroban tx with multiple operations, each emitting
// its own trade. The trades share (source, ledger, tx_hash, ts)
// but differ on OpIndex — both MUST persist. Before the fanout
// fix, op=0,i=1,j=0 and op=1,i=0,j=0 collided on OpIndex=256 and
// ON CONFLICT DO NOTHING silently dropped the second.
func TestInsertTrade_MultiOpSameTxBothLand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, _ := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	pair, _ := c.NewPair(c.NativeAsset(), usdc)

	ts := time.Now().UTC().Truncate(time.Second)
	tx := "1111111111111111111111111111111111111111111111111111111111111111"
	base := c.Trade{
		Source: "sdex", Ledger: 52_430_001, TxHash: tx,
		Timestamp: ts, Pair: pair,
		BaseAmount:  c.NewAmount(big.NewInt(1_000_000_000)),
		QuoteAmount: c.NewAmount(big.NewInt(12_420_000)),
	}
	tr0 := base
	tr0.OpIndex = 0
	tr1 := base
	tr1.OpIndex = 1

	if err := store.InsertTrade(ctx, tr0); err != nil {
		t.Fatalf("op=0: %v", err)
	}
	if err := store.InsertTrade(ctx, tr1); err != nil {
		t.Fatalf("op=1: %v", err)
	}

	n, err := store.CountTrades(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("CountTrades = %d, want 2 — multi-op trades dropped?", n)
	}
}

// startTimescale gives this package's ~40 call sites the shared
// Timescale bootstrap. Every container start in the repo goes through
// harness.StartTimescale so the image pin, the container flags and the
// readiness gate cannot drift apart. Each call starts its OWN
// container — no shared fixture. Returns the connection DSN.
func startTimescale(t *testing.T, ctx context.Context) string {
	t.Helper()
	return harness.StartTimescale(t, ctx)
}

func applyMigrations(t *testing.T, dsn string) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
	m, err := migrate.New("file://"+migrationsDir, dsn)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Up(); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	// Quiesce the CAGG refresh policies HERE, for every test container,
	// not per-test: 12 of the 13 files that `CALL
	// refresh_continuous_aggregate(...)` never called
	// quiesceCAGGRefreshPolicies and raced the policy job TimescaleDB
	// fires shortly after add_continuous_aggregate_policy — the 55P03
	// "concurrent refresh" flake that failed TestAPI_EndToEnd +
	// TestVWAPUSDFXResolver_BootstrapsWithoutUSDVolume in CI
	// (migration 0147 lengthened the chain enough to shift the timing
	// into collision). Integration tests materialize every view by
	// hand; a scheduled background refresh adds nothing they assert.
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open for policy quiesce: %v", err)
	}
	defer db.Close()
	quiesceCAGGRefreshPolicies(t, context.Background(), db)
}

// quiesceCAGGRefreshPolicies unschedules every continuous-aggregate
// refresh-policy background job. A test that drives refreshes explicitly
// via `CALL refresh_continuous_aggregate(...)` otherwise races the policy
// job TimescaleDB fires shortly after `add_continuous_aggregate_policy`,
// and the two overlapping refreshes of one CAGG are rejected with 55P03
// ("could not refresh continuous aggregate ... due to a concurrent
// refresh"). Disabling the policy changes nothing the tests assert — they
// materialize every view by hand — it just makes that manual refresh
// deterministic. (Reproducible pre-existing flake: `go test -count=2 -run
// TestTWAPPointsInRange_TimeWeighted`.)
func quiesceCAGGRefreshPolicies(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		SELECT alter_job(job_id, scheduled => false)
		  FROM timescaledb_information.jobs
		 WHERE proc_name = 'policy_refresh_continuous_aggregate'`); err != nil {
		t.Fatalf("quiesce CAGG refresh policies: %v", err)
	}
}

// TestTWAPPointsInRange_TimeWeighted proves the twap_1h CAGG (migration
// 0081) + TWAPPointsInRange read is TIME-weighted at 1-minute
// resolution, NOT trade-count-weighted. Two minutes in one hour:
//   - minute A: 3 trades at price 1.0
//   - minute B: 1 trade  at price 3.0
//
// A trade-count mean would be (3·1 + 1·3)/4 = 1.5. A time-weighted mean
// (each minute equal weight) is (1.0 + 3.0)/2 = 2.0. We assert 2.0.
func TestTWAPPointsInRange_TimeWeighted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	pair, _ := c.NewPair(c.NativeAsset(), usdc)

	// 2 days ago, snapped to the hour → both the hour bucket
	// (bucket <= now()-1h) AND the day bucket (bucket <= now()-1d) are
	// fully closed, and both minutes share one hour + one day bucket.
	base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Hour)
	const stroops = 100_000_000 // 10 XLM
	mk := func(ts time.Time, opIdx uint32, quote int64) c.Trade {
		return c.Trade{
			Source:      "sdex",
			Ledger:      52_430_001,
			TxHash:      "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe",
			OpIndex:     opIdx,
			Timestamp:   ts,
			Pair:        pair,
			BaseAmount:  c.NewAmount(big.NewInt(stroops)),
			QuoteAmount: c.NewAmount(big.NewInt(quote)),
			Maker:       "maker-acc",
			Taker:       "taker-acc",
		}
	}
	// Minute A (base): 3 prints at price 1.0 (quote == base).
	// Minute B (base+1m): 1 print at price 3.0 (quote == 3·base).
	trades := []c.Trade{
		mk(base.Add(0*time.Second), 0, stroops),
		mk(base.Add(10*time.Second), 1, stroops),
		mk(base.Add(20*time.Second), 2, stroops),
		mk(base.Add(1*time.Minute), 3, 3*stroops),
	}
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	quiesceCAGGRefreshPolicies(t, ctx, db)
	// prices_1m first (the twap CAGG's source), then the hierarchical
	// twap_1h — order matters for a CAGG-on-CAGG refresh.
	for _, cagg := range []string{"prices_1m", "twap_1h"} {
		if _, err := db.ExecContext(ctx,
			"CALL refresh_continuous_aggregate('"+cagg+"', NULL, NULL)"); err != nil {
			t.Fatalf("refresh %s: %v", cagg, err)
		}
	}

	pts, err := store.TWAPPointsInRange(ctx, pair, timescale.Granularity1h, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("TWAPPointsInRange: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("got %d twap buckets, want 1 (hour %s)", len(pts), base.Format(time.RFC3339))
	}
	twap, ok := new(big.Rat).SetString(pts[0].VWAP)
	if !ok {
		t.Fatalf("twap %q not numeric", pts[0].VWAP)
	}
	// Expect 2.0 (time-weighted), reject 1.5 (count-weighted). Allow a
	// tiny tolerance for NUMERIC text rounding.
	want := big.NewRat(2, 1)
	diff := new(big.Rat).Sub(twap, want)
	diff.Abs(diff)
	if diff.Cmp(big.NewRat(1, 1000)) > 0 {
		t.Errorf("twap = %s, want ~2.0 (time-weighted; 1.5 would be trade-count-weighted)", pts[0].VWAP)
	}

	// 1d grain must also be gated + readable (same bucket, one day).
	if _, err := db.ExecContext(ctx,
		"CALL refresh_continuous_aggregate('twap_1d', NULL, NULL)"); err != nil {
		t.Fatalf("refresh twap_1d: %v", err)
	}
	dayPts, err := store.TWAPPointsInRange(ctx, pair, timescale.Granularity1d, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("TWAPPointsInRange(1d): %v", err)
	}
	if len(dayPts) != 1 {
		t.Fatalf("got %d daily twap buckets, want 1", len(dayPts))
	}

	// Unsupported grain must error (the storage-side gate).
	if _, err := store.TWAPPointsInRange(ctx, pair, timescale.Granularity15m, time.Time{}, time.Time{}, 0); err == nil {
		t.Error("TWAPPointsInRange(15m) = nil error, want unsupported-granularity error")
	}
}

// TestTWAPSampleCount_CoverageWeighted proves migration 0126's
// `sample_count` column materializes in the twap_1h CAGG AND that
// TWAPPointsInRange folds the two stored market directions by minute
// COVERAGE, not trade count. This is the on-Postgres
// twin of the cannedConn unit test.
//
// One hour of a two-sided XLM/USDC market:
//
//   - direction A (XLM/USDC @ 0.5): 5 distinct minutes, 1 trade each
//     → minute coverage 5, trade_count 5, oriented price 0.5
//
//   - direction B (USDC/XLM @ 5.0): 1 minute, 20 trades
//     → minute coverage 1, trade_count 20, oriented price 1/5 = 0.2
//
//     coverage-weighted (CORRECT): (0.5·5 + 0.2·1)/(5+1) = 2.7/6 = 0.45
//     trade-count-weighted (bug):  (0.5·5 + 0.2·20)/(5+20) = 6.5/25 = 0.26
//     equal-weighted (regression): (0.5 + 0.2)/2 = 0.35
func TestTWAPSampleCount_CoverageWeighted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	fwd, _ := c.NewPair(c.NativeAsset(), usdc) // XLM/USDC (requested)
	rev, _ := c.NewPair(usdc, c.NativeAsset()) // USDC/XLM (flipped storage)

	base := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Hour)
	const stroops = 100_000_000 // 10 units
	mk := func(pair c.Pair, ts time.Time, opIdx uint32, baseAmt, quoteAmt int64) c.Trade {
		return c.Trade{
			Source:      "sdex",
			Ledger:      52_430_001,
			TxHash:      "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe",
			OpIndex:     opIdx,
			Timestamp:   ts,
			Pair:        pair,
			BaseAmount:  c.NewAmount(big.NewInt(baseAmt)),
			QuoteAmount: c.NewAmount(big.NewInt(quoteAmt)),
			Maker:       "maker-acc",
			Taker:       "taker-acc",
		}
	}
	var trades []c.Trade
	var op uint32
	// Direction A: 5 distinct minutes, 1 trade each, price 0.5.
	for m := 0; m < 5; m++ {
		trades = append(trades, mk(fwd, base.Add(time.Duration(m)*time.Minute), op, stroops, stroops/2))
		op++
	}
	// Direction B: 1 minute, 20 trades, price 5.0 (USDC/XLM).
	for k := 0; k < 20; k++ {
		trades = append(trades, mk(rev, base.Add(10*time.Minute).Add(time.Duration(k)*time.Second), op, stroops, 5*stroops))
		op++
	}
	for _, tr := range trades {
		if err := store.InsertTrade(ctx, tr); err != nil {
			t.Fatalf("InsertTrade: %v", err)
		}
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	quiesceCAGGRefreshPolicies(t, ctx, db)
	for _, cagg := range []string{"prices_1m", "twap_1h"} {
		if _, err := db.ExecContext(ctx,
			"CALL refresh_continuous_aggregate('"+cagg+"', NULL, NULL)"); err != nil {
			t.Fatalf("refresh %s: %v", cagg, err)
		}
	}

	// 1. sample_count materialized, and equals each direction's minute count.
	for _, want := range []struct {
		b, q string
		n    int64
	}{
		{fwd.Base.String(), fwd.Quote.String(), 5},
		{rev.Base.String(), rev.Quote.String(), 1},
	} {
		var sc int64
		if err := db.QueryRowContext(ctx,
			"SELECT sample_count FROM twap_1h WHERE base_asset = $1 AND quote_asset = $2 AND bucket = $3",
			want.b, want.q, base,
		).Scan(&sc); err != nil {
			t.Fatalf("read twap_1h.sample_count for %s/%s: %v", want.b, want.q, err)
		}
		if sc != want.n {
			t.Errorf("twap_1h.sample_count[%s/%s] = %d, want %d (minute coverage)", want.b, want.q, sc, want.n)
		}
	}

	// 2. TWAPPointsInRange serves the coverage-weighted union, not the
	//    trade-count-weighted (0.26) or equal-weighted (0.35) answer.
	pts, err := store.TWAPPointsInRange(ctx, fwd, timescale.Granularity1h, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("TWAPPointsInRange: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("got %d twap buckets, want 1 (both directions fold into one)", len(pts))
	}
	served, ok := new(big.Rat).SetString(pts[0].VWAP)
	if !ok {
		t.Fatalf("served twap %q not numeric", pts[0].VWAP)
	}
	near := func(want *big.Rat) bool {
		d := new(big.Rat).Sub(served, want)
		d.Abs(d)
		return d.Cmp(big.NewRat(1, 1000)) <= 0
	}
	if !near(big.NewRat(45, 100)) {
		t.Errorf("served TWAP = %s, want coverage-weighted 0.45", pts[0].VWAP)
	}
	if near(big.NewRat(26, 100)) {
		t.Errorf("served TWAP = %s is the trade-count-weighted 0.26 — the M-B defect", pts[0].VWAP)
	}
	if near(big.NewRat(35, 100)) {
		t.Errorf("served TWAP = %s is the equal-weighted 0.35 — coverage ignored", pts[0].VWAP)
	}
}
