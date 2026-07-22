package clickhouse

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"sort"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// NativeLiquidityPoolState is one classic (CAP-38) constant-product
// liquidity pool's decoded CURRENT reserve state (ADR-0039), read
// straight from the `liquidity_pool` LedgerEntry in the certified
// lake's current-state table. Both sides are always present — native
// pools carry TWO-sided reserves on-chain (unlike the watched-asset-
// gated `lp_reserve_observations` supply path, which only records the
// operator-watched side). Reserves are classic 7-decimal stroops kept
// as *big.Int (ADR-0003), even though they fit in int64, so the shared
// constant-product depth math consumes them uniformly with the Soroban
// AMM path.
type NativeLiquidityPoolState struct {
	PoolHex     string // 32-byte LiquidityPoolId, lowercase hex (Horizon/SDEX form)
	PoolStrkey  string // L-strkey (SEP-23)
	AssetA      string // canonical asset_id ("native" | "CODE-ISSUER")
	ReserveA    *big.Int
	AssetB      string
	ReserveB    *big.Int
	TotalShares *big.Int
	Trustlines  int64 // pool-share trustline count (number of LPs)
	FeeBps      int32 // Params.Fee — basis points (30 for every constant-product pool today)
	// Ledger is the ledger_seq at which the pool entry last changed —
	// its reserves are current as of this ledger and unchanged since.
	Ledger uint32
}

// NativeLiquidityPoolReserves reads the CURRENT two-sided reserve state
// for the given classic liquidity pools from the lake in a single
// batched `key_xdr IN (...)` lookup against ledger_entries_current
// (PK-prefix on (entry_type, key_xdr) — cheap even for a handful at
// once). Pool ids may be L-strkey (SEP-23) or 32-byte hex. Pools whose
// entry isn't captured, was removed, or isn't a ConstantProduct pool
// are absent from the result — callers treat absence as "reserves
// unavailable", never as zero.
func (r *ExplorerReader) NativeLiquidityPoolReserves(ctx context.Context, poolIDs []string) (map[string]NativeLiquidityPoolState, error) {
	keys := make([]string, 0, len(poolIDs))
	for _, p := range poolIDs {
		pid, err := poolIDToXDR(p)
		if err != nil {
			return nil, fmt.Errorf("clickhouse: liquidity pool id %q: %w", p, err)
		}
		k, err := liquidityPoolKeyXDR(pid)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return map[string]NativeLiquidityPoolState{}, nil
	}

	const q = `SELECT ledger_seq, entry_xdr
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'liquidity_pool' AND key_xdr IN (?) AND entry_xdr != ''`
	rows, err := r.conn.Query(ctx, q, keys)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: liquidity pool reserves lookup: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]NativeLiquidityPoolState, len(poolIDs))
	for rows.Next() {
		var ledgerSeq uint32
		var b64 string
		if err := rows.Scan(&ledgerSeq, &b64); err != nil {
			return nil, fmt.Errorf("clickhouse: scan liquidity pool state: %w", err)
		}
		st, ok := nativeLPStateFromEntry(b64)
		if !ok {
			continue
		}
		st.Ledger = ledgerSeq
		out[st.PoolStrkey] = st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: liquidity pool reserves rows: %w", err)
	}
	return out, nil
}

// NativeLiquidityPoolsRanked reads EVERY classic constant-product pool's
// current state from the lake and returns the top `limit` ranked by
// pool-share trustline count descending (number of liquidity providers
// — the only cross-pool-comparable size signal available without USD
// pricing of arbitrary pool assets; pool shares and reserves are in
// per-pair units and don't compare across pools). Ties break on pool
// id for a stable order. The scan is a single PK-prefix-scoped
// aggregate over the `liquidity_pool` entry_type (~40k pools, ~0.07s on
// r1); callers cache the ranked result rather than re-scan per request.
func (r *ExplorerReader) NativeLiquidityPoolsRanked(ctx context.Context, limit int) ([]NativeLiquidityPoolState, error) {
	// argMax over the ReplacingMergeTree versions (manual dedup —
	// cheaper than FINAL for a whole-prefix scan), latest entry per pool.
	//
	// Version key: `version` where the D3 reproject has run, else
	// `ledger_seq`.
	//
	// site-audit S3: this query hard-coded `version`, a column that does not
	// exist on R1's pre-D3 table, so /v1/liquidity-pools 500'd on 100% of
	// requests ("Unknown identifier `version`"). `version` is the
	// (ledger_seq<<32)|intra_ledger_seq RMT version that
	// deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql (D3)
	// introduces to disambiguate same-ledger changes (C2-4c). D3 is
	// freeze-gated and runs after D2, so the served table is still
	// ReplacingMergeTree(ledger_seq) with no such column — while CI's
	// ClickHouse has the post-D3 schema.
	//
	// Probing (once per process) makes the query correct on both: `version`
	// gives true C2-4c same-ledger-last-wins resolution where it exists;
	// `ledger_seq` matches the pre-D3 engine exactly and carries the same
	// same-ledger-tie limitation FINAL already has there. When D3 lands on
	// R1 the probe flips and pools get C2-4c automatically, no code change.
	// The trailing removal (empty entry_xdr) is dropped by `HAVING e != ''`.
	versionKey := "ledger_seq"
	if r.ledgerEntriesVersioned(ctx) {
		versionKey = "version"
	}
	q := fmt.Sprintf(`SELECT argMax(entry_xdr, %s) AS e, max(ledger_seq) AS led
		FROM stellar.ledger_entries_current
		WHERE entry_type = 'liquidity_pool'
		GROUP BY key_xdr
		HAVING e != ''`, versionKey)
	rows, err := r.conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: liquidity pools ranked scan: %w", err)
	}
	defer func() { _ = rows.Close() }()

	all := make([]NativeLiquidityPoolState, 0, 4096)
	for rows.Next() {
		var b64 string
		var ledgerSeq uint32
		if err := rows.Scan(&b64, &ledgerSeq); err != nil {
			return nil, fmt.Errorf("clickhouse: scan ranked liquidity pool: %w", err)
		}
		st, ok := nativeLPStateFromEntry(b64)
		if !ok {
			continue
		}
		st.Ledger = ledgerSeq
		all = append(all, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: liquidity pools ranked rows: %w", err)
	}

	sort.Slice(all, func(i, j int) bool {
		if all[i].Trustlines != all[j].Trustlines {
			return all[i].Trustlines > all[j].Trustlines
		}
		return all[i].PoolStrkey < all[j].PoolStrkey
	})
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// nativeLPStateFromEntry decodes one `liquidity_pool` LedgerEntry into
// a two-sided reserve state. ok=false when the entry isn't a
// ConstantProduct pool or an asset side can't be canonicalised — refuse
// to guess rather than misreport.
func nativeLPStateFromEntry(b64 string) (NativeLiquidityPoolState, bool) {
	var entry xdr.LedgerEntry
	if xdr.SafeUnmarshalBase64(b64, &entry) != nil {
		return NativeLiquidityPoolState{}, false
	}
	lp, ok := entry.Data.GetLiquidityPool()
	if !ok {
		return NativeLiquidityPoolState{}, false
	}
	cp, ok := lp.Body.GetConstantProduct()
	if !ok {
		return NativeLiquidityPoolState{}, false // only ConstantProduct exists today
	}
	assetA, ok := classicAssetID(cp.Params.AssetA)
	if !ok {
		return NativeLiquidityPoolState{}, false
	}
	assetB, ok := classicAssetID(cp.Params.AssetB)
	if !ok {
		return NativeLiquidityPoolState{}, false
	}
	poolStrkey, err := strkey.Encode(strkey.VersionByteLiquidityPool, lp.LiquidityPoolId[:])
	if err != nil {
		return NativeLiquidityPoolState{}, false
	}
	return NativeLiquidityPoolState{
		PoolHex:     hex.EncodeToString(lp.LiquidityPoolId[:]),
		PoolStrkey:  poolStrkey,
		AssetA:      assetA,
		ReserveA:    big.NewInt(int64(cp.ReserveA)),
		AssetB:      assetB,
		ReserveB:    big.NewInt(int64(cp.ReserveB)),
		TotalShares: big.NewInt(int64(cp.TotalPoolShares)),
		Trustlines:  int64(cp.PoolSharesTrustLineCount),
		FeeBps:      int32(cp.Params.Fee),
	}, true
}

// liquidityPoolKeyXDR builds the base64 LedgerKey for a classic
// LiquidityPool entry — matching the `key_xdr` column verbatim.
func liquidityPoolKeyXDR(poolID xdr.PoolId) (string, error) {
	lk := xdr.LedgerKey{
		Type:          xdr.LedgerEntryTypeLiquidityPool,
		LiquidityPool: &xdr.LedgerKeyLiquidityPool{LiquidityPoolId: poolID},
	}
	b64, err := xdr.MarshalBase64(lk)
	if err != nil {
		return "", fmt.Errorf("clickhouse: marshal liquidity pool key: %w", err)
	}
	return b64, nil
}

// poolIDToXDR parses a liquidity-pool id in either accepted form —
// L-strkey (SEP-23) or 32-byte hex — into an xdr.PoolId.
func poolIDToXDR(s string) (xdr.PoolId, error) {
	var pid xdr.PoolId
	if raw, err := strkey.Decode(strkey.VersionByteLiquidityPool, s); err == nil {
		if len(raw) != len(pid) {
			return xdr.PoolId{}, fmt.Errorf("liquidity pool strkey decoded to %d bytes", len(raw))
		}
		copy(pid[:], raw)
		return pid, nil
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return xdr.PoolId{}, fmt.Errorf("not an L-strkey or hex pool id")
	}
	if len(raw) != len(pid) {
		return xdr.PoolId{}, fmt.Errorf("hex pool id must be 32 bytes, got %d", len(raw))
	}
	copy(pid[:], raw)
	return pid, nil
}

// classicAssetID converts an xdr.Asset (a classic-pool reserve side) to
// its canonical asset_id string: "native", or "<CODE>-<ISSUER>" for
// credit alphanum4/12. ok=false for any other (impossible for a
// classic LP) asset type.
func classicAssetID(a xdr.Asset) (string, bool) {
	switch a.Type {
	case xdr.AssetTypeAssetTypeNative:
		return "native", true
	case xdr.AssetTypeAssetTypeCreditAlphanum4:
		a4, ok := a.GetAlphaNum4()
		if !ok {
			return "", false
		}
		issuer, err := strkey.Encode(strkey.VersionByteAccountID, a4.Issuer.Ed25519[:])
		if err != nil {
			return "", false
		}
		return trimAssetCode(a4.AssetCode[:]) + "-" + issuer, true
	case xdr.AssetTypeAssetTypeCreditAlphanum12:
		a12, ok := a.GetAlphaNum12()
		if !ok {
			return "", false
		}
		issuer, err := strkey.Encode(strkey.VersionByteAccountID, a12.Issuer.Ed25519[:])
		if err != nil {
			return "", false
		}
		return trimAssetCode(a12.AssetCode[:]) + "-" + issuer, true
	}
	return "", false
}

// trimAssetCode drops the null padding from a fixed-size classic
// asset-code byte array ("USDC\x00…" → "USDC").
func trimAssetCode(b []byte) string {
	n := len(b)
	for n > 0 && b[n-1] == 0 {
		n--
	}
	return string(b[:n])
}
