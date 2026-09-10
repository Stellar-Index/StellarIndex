//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/google/uuid"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// legacyBlendReservesSQL is the BlendPoolReserves lookup EXACTLY as it stood
// before the #504 rewrite, frozen here as the differential oracle: a
// 250,000-ledger windowed fold over stellar.ledger_entry_changes, resolving
// the latest entry per key itself. The rewrite must agree with it row for row
// on every reserve the window could see, while no longer reading one granule
// per WRITE the pool made inside that window.
func legacyBlendReservesSQL() string {
	return `SELECT key_xdr, argMax(entry_xdr, (ledger_seq, intra_ledger_seq)) AS latest_xdr
		FROM stellar.ledger_entry_changes
		WHERE entry_type = 'contract_data'
		  AND ledger_seq > (SELECT max(ledger_seq) FROM stellar.ledger_entry_changes) - ?
		  AND key_xdr IN (?)
		GROUP BY key_xdr
		HAVING argMax(change_type, (ledger_seq, intra_ledger_seq)) != 'removed'`
}

// legacyBlendWindowLedgers is the frozen oracle's window width.
const legacyBlendWindowLedgers = uint32(250_000)

// TestBlendPoolReserves_CurrentStateProjectionBoundsTheRead is the
// live-ClickHouse proof for #504 (`/v1/lending/pools/{pool}/reserves` →
// 503 lending-timeout at 12.1s on the largest Blend pool; 9.31s — 78% of the
// same 12s budget — on a SMALL one).
//
// Pathology: the reserve lookup folded the latest entry per key out of
// stellar.ledger_entry_changes across a 250,000-ledger (~14-day) window. A
// Blend ResData entry is REWRITTEN on nearly every pool interaction, so one
// reserve's key alone matches tens of thousands of rows scattered through
// that window's granules (74,834 rows for the busiest mainnet pool's USDC
// reserve, r1 2026-09-03). The read's cost was therefore a function of pool
// WRITE ACTIVITY, not of how many reserves were asked for — a multi-second
// floor under EVERY pool, which is why the small pool already sat at 78% of
// the ceiling and the largest merely crossed it first.
//
// The fix reads stellar.ledger_entries_current, whose sort key IS
// (entry_type, key_xdr): ~one row per requested key, the same shape the three
// sibling pool-state readers (Soroswap / Phoenix / Comet) have always used.
//
// Removing that window, however, also removed a bound it was serving by
// ACCIDENT. An archived (TTL-lapsed) Soroban entry has had no writes since it
// lapsed, so the narrow window dropped it as a side effect of being narrow,
// while ledger_entries_current keeps its last-known value forever. Reading the
// projection without an explicit staleness bound hands a DEAD pool's final
// reserves to the handler, which prices them at today's USD rate into
// `tvl_usd` and stamps the current watermark with flags.stale=false — a
// fabricated TVL where the pre-fix code returned an empty reserve list. So the
// read is paired with the same archived-entry drop the three sibling readers
// carry, and this test pins BOTH halves: quiet-but-live must answer, archived
// must not. Conflating those two is the bug this fixture exists to prevent.
//
// Fixture — one pool, five reserves chosen to cover every axis:
//
//   - assetHot: ResData rewritten across `churn` ledgers inside the window,
//     the LAST write carrying a distinct b_rate. This is the pathology: the
//     legacy fold must read every one of those writes to find the winner.
//   - assetGone: written, then REMOVED as its final change. Must be absent
//     from BOTH paths — the removed-key drop is the semantics the old
//     `HAVING argMax(change_type, ...) != 'removed'` carried, and that the
//     empty-entry_xdr filter over FINAL must preserve.
//   - assetTied: two writes in the SAME ledger, the stale one sorting FIRST
//     in the base table. Pins audit-2026-07-16 C2-4c through the new path:
//     the projection's version is (ledger_seq << 32) | intra_ledger_seq, so
//     FINAL keeps the LAST intra-ledger change, exactly as the old composite
//     argMax did.
//   - assetQuiet: a single write far BELOW the legacy window, with a TTL
//     entry that is still LIVE. Quiet is not dead: the old shape reported it
//     absent ("consistent with captured window") and the projection answers.
//     The genuine coverage win — and it must SURVIVE the archived drop.
//   - assetArchived: written in the same old ledger as assetQuiet, but with a
//     LAPSED TTL. As visible to the projection as assetQuiet is; the ONLY
//     thing separating the two is the TTL verdict. Must be ABSENT, or the
//     route publishes a dead pool's reserves as current liquidity.
//
// The test then:
//
//  1. DIFFERENTIAL: the frozen legacy SQL and the reader must agree on the
//     winning entry for every reserve the window could see (hot, tied), agree
//     on the removal (gone absent from both), and differ on the two OLD
//     reserves — quiet ADMITTED where the legacy window could not see it,
//     archived still absent but now for a stated reason rather than as a
//     lucky side effect of the scan bound.
//  2. READ-ROWS: both paths measured via system.query_log. The legacy shape
//     reads at least the pool's whole in-window write history; the new one
//     must read a small multiple of the key count. Red-proof: with the old
//     query text in the reader the two figures are equal and the bound
//     below fails.
func TestBlendPoolReserves_CurrentStateProjectionBoundsTheRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)
	raw := dialClickHouse(t, ctx, "stellar")

	const (
		poolSeed     = byte(0xC0)
		hotSeed      = byte(0xC1)
		goneSeed     = byte(0xC2)
		tiedSeed     = byte(0xC3)
		quietSeed    = byte(0xC4)
		archivedSeed = byte(0xC5)

		// Sits inside the 250k window below the suite's highest seeded
		// ledger (4_000_000_000, argmax_intra_ledger_seq_readers_test.go)
		// whichever test runs first — asserted below rather than assumed.
		base  = uint32(3_999_900_000)
		churn = 3_000
		// Far below the window: the legacy shape cannot see these two.
		quietLedger = uint32(1_000_000)

		// TTL verdicts are judged against max(ledger_seq) at compute time,
		// which the suite shares. These two sit far either side of any
		// plausible tip (the suite's is 4e9, and this fixture's own top is
		// below it), so the verdicts hold whichever test seeded first.
		liveUntilLive     = uint32(4_294_000_000)
		liveUntilArchived = uint32(1_500_000_000)

		staleBRate    = uint64(1_000_000_000_000) // 1.0 at 12 decimals
		finalBRate    = uint64(2_000_000_000_000) // 2.0 — the winning write
		quietBRate    = uint64(3_000_000_000_000) // 3.0
		archivedBRate = uint64(4_000_000_000_000) // 4.0 — must never be served
	)
	closeTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	pool := contractIDFromSeed(poolSeed)
	poolStr := mustContractStrkey(t, poolSeed)
	hot, gone, tied := contractIDFromSeed(hotSeed), contractIDFromSeed(goneSeed), contractIDFromSeed(tiedSeed)
	quiet, archived := contractIDFromSeed(quietSeed), contractIDFromSeed(archivedSeed)
	hotStr := mustContractStrkey(t, hotSeed)
	goneStr := mustContractStrkey(t, goneSeed)
	tiedStr := mustContractStrkey(t, tiedSeed)
	quietStr := mustContractStrkey(t, quietSeed)
	archivedStr := mustContractStrkey(t, archivedSeed)

	hotKey := resDataKeyB64(t, pool, hot)
	goneKey := resDataKeyB64(t, pool, gone)
	tiedKey := resDataKeyB64(t, pool, tied)
	quietKey := resDataKeyB64(t, pool, quiet)
	archivedKey := resDataKeyB64(t, pool, archived)

	hotFinalEntry := resDataEntryB64(t, pool, hot, base+churn, finalBRate)
	tiedFinalEntry := resDataEntryB64(t, pool, tied, base+churn, finalBRate)
	quietEntry := resDataEntryB64(t, pool, quiet, quietLedger, quietBRate)
	archivedEntry := resDataEntryB64(t, pool, archived, quietLedger, archivedBRate)

	mustExec := func(q string, args ...any) {
		t.Helper()
		if err := raw.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %.90q: %v", q, err)
		}
	}

	// The hot reserve's churn: `churn` writes of the STALE state, one per
	// ledger, in ONE insert so they land together. This is what the legacy
	// fold has to read in full to find the winner.
	mustExec(fmt.Sprintf(`INSERT INTO stellar.ledger_entry_changes
		(ledger_seq, close_time, tx_hash, op_index, change_index, change_type, entry_type, key_xdr, entry_xdr, intra_ledger_seq)
		SELECT toUInt32(%d + number), toDateTime('2024-01-01 00:00:00', 'UTC'), lpad(toString(number), 64, '0'), 0, 0,
		       'updated', 'contract_data', '%s', '%s', 1
		FROM numbers(%d)`, base, hotKey, resDataEntryB64(t, pool, hot, base, staleBRate), churn))

	rows := []chstore.LedgerEntryChangeRow{
		// assetHot: the winning write, one ledger past the churn.
		{
			LedgerSeq: base + churn, CloseTime: closeTime, TxHash: "blend504-hot", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "updated", EntryType: "contract_data",
			KeyXDR: hotKey, EntryXDR: hotFinalEntry,
		},
		// assetGone: a live write, then a REMOVAL as the final change.
		{
			LedgerSeq: base + 10, CloseTime: closeTime, TxHash: "blend504-gone", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "updated", EntryType: "contract_data",
			KeyXDR: goneKey, EntryXDR: resDataEntryB64(t, pool, gone, base+10, staleBRate),
		},
		{
			LedgerSeq: base + 11, CloseTime: closeTime, TxHash: "blend504-gone", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "removed", EntryType: "contract_data",
			KeyXDR: goneKey, EntryXDR: "",
		},
		// assetTied: the LATER same-ledger change (intra 9), written first so
		// it sorts LAST in the base table's ORDER BY — a ledger_seq-only
		// tie-break keeps the stale row below instead.
		{
			LedgerSeq: base + churn, CloseTime: closeTime, TxHash: "blend504-tied", OpIndex: 1, ChangeIndex: 0,
			IntraLedgerSeq: 9, ChangeType: "updated", EntryType: "contract_data",
			KeyXDR: tiedKey, EntryXDR: tiedFinalEntry,
		},
		{
			LedgerSeq: base + churn, CloseTime: closeTime, TxHash: "blend504-tied", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 8, ChangeType: "updated", EntryType: "contract_data",
			KeyXDR: tiedKey, EntryXDR: resDataEntryB64(t, pool, tied, base+churn, staleBRate),
		},
		// assetQuiet + assetArchived: one write each, far below the legacy
		// window, IDENTICAL in every respect the projection can see. Only
		// their TTL entries (seeded below) differ.
		{
			LedgerSeq: quietLedger, CloseTime: closeTime, TxHash: "blend504-quiet", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "updated", EntryType: "contract_data",
			KeyXDR: quietKey, EntryXDR: quietEntry,
		},
		{
			LedgerSeq: quietLedger, CloseTime: closeTime, TxHash: "blend504-archived", OpIndex: 0, ChangeIndex: 0,
			IntraLedgerSeq: 1, ChangeType: "updated", EntryType: "contract_data",
			KeyXDR: archivedKey, EntryXDR: archivedEntry,
		},
		// The TTL entries that separate them. ttlChangeRow (ttl_liveness_test.go)
		// renders the lake's TTL change for a governed key; the
		// ttl_live_until_mv materialized view turns it into the slim projection
		// ClassifyTTLLiveness reads.
		ttlChangeRow(quietKey, base+1, 1, liveUntilLive, 48),
		ttlChangeRow(archivedKey, base+1, 1, liveUntilArchived, 48),
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	// Collapse ledger_entries_current to its steady state. In production
	// background merges keep the projection at ~one row per live key, which is
	// what makes the PK-prefix probe cheap; a container that was seeded
	// seconds ago has every version still sitting in unmerged parts, so
	// without this the read_rows figures below would measure merge lag rather
	// than query shape. FINAL returns the same ANSWER either way — this only
	// removes the fixture's own artefact.
	mustExec(`OPTIMIZE TABLE stellar.ledger_entries_current FINAL`)

	// The oracle derives its window from the table's global max ledger, which
	// the whole suite shares. Prove the fixture is actually inside it, or the
	// differential below would be comparing against an empty legacy answer.
	var maxLedger uint32
	if err := raw.QueryRow(ctx, `SELECT max(ledger_seq) FROM stellar.ledger_entry_changes`).Scan(&maxLedger); err != nil {
		t.Fatalf("read max ledger: %v", err)
	}
	if maxLedger < base+churn || maxLedger-legacyBlendWindowLedgers >= base {
		t.Fatalf("fixture outside the frozen oracle's window: max(ledger_seq)=%d, window starts at %d, fixture spans [%d, %d] — the legacy answer would be empty and every comparison below vacuous",
			maxLedger, maxLedger-legacyBlendWindowLedgers, base, base+churn)
	}
	if quietLedger > maxLedger-legacyBlendWindowLedgers {
		t.Fatalf("the quiet + archived reserves (ledger %d) landed INSIDE the legacy window (starts %d) — they exist to separate the retired capture-window caveat from the staleness bound, so they must sit below it",
			quietLedger, maxLedger-legacyBlendWindowLedgers)
	}
	// The TTL verdicts are judged at the lake tip; assert the fixture's two
	// live_until values still straddle it, or the archived assertion below
	// would pass for the wrong reason (or the quiet one fail spuriously).
	if liveUntilArchived >= maxLedger {
		t.Fatalf("assetArchived's live_until (%d) is at/above the lake tip (%d) — it would classify LIVE and the archived-drop assertion would be vacuous", liveUntilArchived, maxLedger)
	}
	if liveUntilLive < maxLedger {
		t.Fatalf("assetQuiet's live_until (%d) is below the lake tip (%d) — it would classify ARCHIVED and the quiet-survives assertion would be testing the opposite property", liveUntilLive, maxLedger)
	}

	keys := []string{hotKey, goneKey, tiedKey, quietKey, archivedKey}
	legacyWinners := func(qctx context.Context) map[string]string {
		t.Helper()
		out := map[string]string{}
		rs, err := raw.Query(qctx, legacyBlendReservesSQL(), legacyBlendWindowLedgers, keys)
		if err != nil {
			t.Fatalf("legacy blend reserves: %v", err)
		}
		defer func() { _ = rs.Close() }()
		for rs.Next() {
			var k, entry string
			if err := rs.Scan(&k, &entry); err != nil {
				t.Fatalf("legacy scan: %v", err)
			}
			out[k] = entry
		}
		if err := rs.Err(); err != nil {
			t.Fatalf("legacy rows: %v", err)
		}
		return out
	}

	reader, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	assets := []string{hotStr, goneStr, tiedStr, quietStr, archivedStr}

	// (1) Differential.
	legacy := legacyWinners(ctx)
	states, err := reader.BlendPoolReserves(ctx, poolStr, assets, nil)
	if err != nil {
		t.Fatalf("BlendPoolReserves: %v", err)
	}
	byAsset := make(map[string]chstore.BlendReserveState, len(states))
	for _, s := range states {
		byAsset[s.Asset] = s
	}

	// The fixture must actually reproduce the pathology the oracle models:
	// the legacy path has to see the hot + tied reserves, and must NOT see
	// the removed one.
	for _, want := range []struct {
		key, label, entry string
	}{
		{hotKey, "assetHot", hotFinalEntry},
		{tiedKey, "assetTied", tiedFinalEntry},
	} {
		got, ok := legacy[want.key]
		if !ok {
			t.Fatalf("legacy oracle did not resolve %s — the fixture no longer reproduces the pre-fix behaviour, so the agreement below is vacuous", want.label)
		}
		if got != want.entry {
			t.Fatalf("legacy oracle resolved %s to an unexpected write — the fixture's winner is not the one seeded as final", want.label)
		}
	}
	if _, present := legacy[goneKey]; present {
		t.Fatal("legacy oracle resolved assetGone — its final change is a removal; the fixture is wrong")
	}
	for _, tc := range []struct{ key, label string }{{quietKey, "assetQuiet"}, {archivedKey, "assetArchived"}} {
		if _, present := legacy[tc.key]; present {
			t.Fatalf("legacy oracle resolved %s from ledger %d — both old reserves must sit below the window, or the difference assertions mean nothing", tc.label, quietLedger)
		}
	}

	// Reader vs oracle: same answer for everything the window could see.
	for _, tc := range []struct {
		asset, label string
		wantBRate    uint64
	}{
		{hotStr, "assetHot", finalBRate},
		{tiedStr, "assetTied", finalBRate},
	} {
		got, ok := byAsset[tc.asset]
		if !ok {
			t.Errorf("%s absent from the reader's result; the legacy path resolved it", tc.label)
			continue
		}
		want := new(big.Int).SetUint64(tc.wantBRate)
		if got.Data.BRate == nil || got.Data.BRate.Cmp(want) != 0 {
			t.Errorf("%s b_rate = %v, want %d — the projection resolved a different write than the frozen fold did", tc.label, got.Data.BRate, tc.wantBRate)
		}
	}
	if _, present := byAsset[goneStr]; present {
		t.Error("assetGone present in the reader's result; want ABSENT — its final change was a removal, and `entry_xdr != ''` over FINAL must drop it exactly as the old HAVING did")
	}

	// The two OLD reserves — identical to the projection, opposite verdicts.
	// This pair is the whole point: dropping the 250k-ledger window retired a
	// capture-window caveat AND removed an accidental staleness bound, and the
	// fix must land on the right side of both.
	//
	// Quiet-but-live: ADMITTED. This is the coverage win — a reserve nobody has
	// touched in months is not a dead one, and the old shape reported it absent.
	q, ok := byAsset[quietStr]
	if !ok {
		t.Error("assetQuiet absent from the reader's result — a QUIET reserve is not a dead one; reading the current-state projection is supposed to retire the 250k-ledger capture-window caveat, and the archived-entry drop must not over-reach into live-but-old entries")
	} else if want := new(big.Int).SetUint64(quietBRate); q.Data.BRate == nil || q.Data.BRate.Cmp(want) != 0 {
		t.Errorf("assetQuiet b_rate = %v, want %d", q.Data.BRate, quietBRate)
	}

	// Positively archived: ABSENT. The projection still holds this entry's
	// last-known value and the query returns it — nothing in the SQL can tell
	// it apart from assetQuiet. Only the TTL verdict can, and if it is not
	// consulted the handler prices a dead pool's reserves at today's USD rate
	// into tvl_usd and publishes it with flags.stale=false.
	if got, present := byAsset[archivedStr]; present {
		t.Errorf("assetArchived present in the reader's result (b_rate=%v) — its TTL lapsed at ledger %d against a lake tip of %d, so its last-known reserves are NOT current liquidity. The pre-#504 250k-ledger window dropped it as a side effect of being narrow; removing that window without an explicit archived-entry drop publishes a fabricated TVL for a dead pool",
			got.Data.BRate, liveUntilArchived, maxLedger)
	}

	// (2) read_rows, one measured call down each path.
	t0 := time.Now().Add(-time.Second)
	legacyID := uuid.NewString()
	_ = legacyWinners(clickhouse.Context(ctx, clickhouse.WithQueryID(legacyID)))
	readerID := uuid.NewString()
	if _, err := reader.BlendPoolReserves(clickhouse.Context(ctx, clickhouse.WithQueryID(readerID)), poolStr, assets, nil); err != nil {
		t.Fatalf("BlendPoolReserves (measured): %v", err)
	}
	mustExec(`SYSTEM FLUSH LOGS`)

	readRows := func(id string) uint64 {
		t.Helper()
		var rr uint64
		if err := raw.QueryRow(ctx, `SELECT read_rows FROM system.query_log
			WHERE type = 'QueryFinish' AND event_time >= ? AND query_id = ?
			ORDER BY event_time_microseconds DESC LIMIT 1`, t0, id).Scan(&rr); err != nil {
			t.Fatalf("query_log (%s): %v", id, err)
		}
		return rr
	}
	legacyRead, readerRead := readRows(legacyID), readRows(readerID)
	t.Logf("read_rows: legacy=%d reader=%d (fixture: %d in-window writes to ONE reserve key, %d keys probed)",
		legacyRead, readerRead, churn, len(keys))

	if legacyRead < uint64(churn) {
		t.Fatalf("fixture no longer reproduces the pathology: the legacy shape read %d rows, expected at least the pool's %d in-window writes — the bound below would be vacuous",
			legacyRead, churn)
	}
	// The probe reads granules covering the requested keys, not the pool's
	// write history. Generous by design: what must NOT hold is the two
	// figures tracking each other.
	if readerRead*10 > legacyRead {
		t.Errorf("reader read %d rows vs legacy %d — the reserve lookup must be bounded by the KEY COUNT, not by how often the pool was written to (#504)",
			readerRead, legacyRead)
	}
}
