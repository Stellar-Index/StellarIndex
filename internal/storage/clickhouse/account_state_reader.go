package clickhouse

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

// AccountState is the current on-chain state of an account, reconstructed from
// the latest ledger_entry_changes per key (ADR-0038 Phase C). Exists=false
// when the account has no live AccountEntry (never created, or merged away).
type AccountState struct {
	Exists        bool
	Balance       int64 // native XLM, stroops
	SeqNum        int64
	NumSubEntries uint32
	Flags         uint32
	// Liabilities (AccountEntry ext.v1) and sponsorship counters (ext.v2)
	// make minimum balance and spendable XLM derivable:
	// min = (2 + NumSubEntries + NumSponsoring - NumSponsored) × base_reserve,
	// spendable = Balance - min - SellingLiabilities. Zero when absent.
	BuyingLiabilities  int64
	SellingLiabilities int64
	NumSponsoring      uint32
	NumSponsored       uint32
	HomeDomain         string
	MasterWeight       byte
	ThreshLow          byte
	ThreshMed          byte
	ThreshHigh         byte
	LastModifiedLedger uint32
	Signers            []AccountSigner
	Trustlines         []TrustlineState
	Offers             []OfferState
}

type AccountSigner struct {
	Key    string
	Weight uint32
}

type TrustlineState struct {
	Asset   string
	Balance int64
	Limit   int64
	Flags   uint32
	// SellingLiabilities is the part of Balance locked by open offers
	// (TrustLineEntry ext.v1), as BuyingLiabilities is of Limit.
	BuyingLiabilities  int64
	SellingLiabilities int64
	// PoolShare marks a liquidity-pool-share trustline (asset "pool:<hex>"):
	// a claim on a pool's two reserves, not a balance of one asset.
	PoolShare bool
}

type OfferState struct {
	OfferID int64
	Selling string
	Buying  string
	Amount  int64
	PriceN  int32
	PriceD  int32
}

// AssetHolder is one holder of an asset, ranked by current trustline balance.
type AssetHolder struct {
	AccountID string
	Balance   int64
}

// AccountState reconstructs an account's current state from the lake: the
// latest AccountEntry (balance/signers/thresholds/flags/home-domain), plus its
// live trustlines and offers (latest non-removed change per key). Relies on
// the account_id skip-index (ADR-0038 Phase C). Returns Exists=false (no error)
// for an unknown / merged account.
func (r *ExplorerReader) AccountState(ctx context.Context, account string) (AccountState, error) {
	st, err := r.accountEntry(ctx, account)
	if errors.Is(err, errCorruptAccountEntry) {
		// A corrupt stored entry degrades to "no state" rather than 500-ing
		// the request — the row is the substrate's problem, not the caller's.
		return AccountState{}, nil
	}
	if err != nil || !st.Exists {
		return st, err
	}

	tl, err := r.accountTrustlines(ctx, account)
	if err != nil {
		return st, err
	}
	st.Trustlines = tl
	of, err := r.accountOffers(ctx, account)
	if err != nil {
		return st, err
	}
	st.Offers = of
	return st, nil
}

// AccountSigners returns the account's entry-level state (thresholds,
// master weight, signers) without its trustlines and offers. Unlike
// [ExplorerReader.AccountState], a corrupt stored entry is an error: an
// authentication check must not read an unparseable entry as "no account".
func (r *ExplorerReader) AccountSigners(ctx context.Context, account string) (AccountState, error) {
	return r.accountEntry(ctx, account)
}

var errCorruptAccountEntry = errors.New("clickhouse: corrupt stored account entry")

// accountEntry reads the latest AccountEntry. Exists=false (no error) for
// an unknown or merged account; errCorruptAccountEntry when the stored
// entry does not decode.
func (r *ExplorerReader) accountEntry(ctx context.Context, account string) (AccountState, error) {
	var st AccountState

	// Account entry — the current-state projection (ledger_entries_current)
	// already holds the latest entry per key (ReplacingMergeTree); FINAL forces
	// read-time dedup. A trailing 'removed' = merged away.
	// Query by key_xdr, NOT account_id (site-audit follow-up). The table
	// is ORDER BY (entry_type, key_xdr), so account_id — not a sort-key
	// column — cannot use the primary index and every read did a full
	// FINAL scan of the 43.6M-row current-state table. Measured on R1:
	// 0.42s standalone, but under the bounded api_serving profile
	// (2 threads) plus concurrent load it ballooned to the handler's 8s
	// ceiling, which kept /v1/issuers/{g} and /v1/accounts/{g} at 8s and
	// held the whole site's p95 SLO in breach. The account's LedgerKey
	// XDR is a PK prefix, so this is a point lookup — 0.028s, and it does
	// not balloon. Same fix class as NativeLiquidityPoolReserves /
	// TokenDecimals, which already key on key_xdr.
	keyXDR, err := accountKeyXDR(account)
	if err != nil {
		return st, err
	}
	const accQ = `SELECT entry_xdr, change_type, balance, ledger_seq
		FROM stellar.ledger_entries_current FINAL
		WHERE key_xdr = ? AND entry_type = 'account'
		LIMIT 1`
	var (
		entryXDR, changeType string
		bal                  int64
		ledgerSeq            uint32
	)
	row := r.conn.QueryRow(ctx, accQ, keyXDR)
	if err := row.Scan(&entryXDR, &changeType, &bal, &ledgerSeq); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Unknown account / not in the captured window — the empty
			// state, surfaced via Exists=false rather than an error.
			return st, nil
		}
		return st, fmt.Errorf("clickhouse: account entry %s: %w", account, err)
	}
	if changeType == "removed" || entryXDR == "" {
		return st, nil
	}
	st, ok := accountStateFromEntry(entryXDR, bal, ledgerSeq)
	if !ok {
		return AccountState{}, fmt.Errorf("%w: %s", errCorruptAccountEntry, account)
	}
	return st, nil
}

// accountTrustlinesQuery is a PRIMARY-INDEX range read: an account's
// trustline LedgerKeys share a fixed key_xdr prefix (accountEntryKeyPrefix),
// so `key_xdr LIKE '<prefix>%'` prunes to the account's contiguous slice of
// the (entry_type, key_xdr) sort order; the exact account_id equality closes
// the prefix's one-byte residual. Measured on r1 (2026-07-30, whale
// account): 5.18s via the old account_id bloom skip-index → 0.069s. The
// scan-settings pin stays as a guard rail, not load-bearing tuning.
const accountTrustlinesQuery = `SELECT asset, entry_xdr AS ex, balance AS bal
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'trustline' AND key_xdr LIKE ?
		  AND account_id = ? AND change_type != 'removed'
		ORDER BY bal DESC` + explorerScanSettings

func (r *ExplorerReader) accountTrustlines(ctx context.Context, account string) ([]TrustlineState, error) {
	prefix, err := accountEntryKeyPrefix(account, xdr.LedgerEntryTypeTrustline)
	if err != nil {
		return nil, err
	}
	rows, err := r.conn.Query(ctx, accountTrustlinesQuery, prefix+"%", account)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: account trustlines: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TrustlineState
	for rows.Next() {
		var asset, ex string
		var bal int64
		if err := rows.Scan(&asset, &ex, &bal); err != nil {
			return nil, fmt.Errorf("clickhouse: scan trustline: %w", err)
		}
		out = append(out, trustlineStateFromEntry(asset, ex, bal))
	}
	return out, rows.Err()
}

// accountStateFromEntry decodes a stored AccountEntry (base64 LedgerEntry
// XDR). ok=false when the XDR is corrupt or not an account entry.
func accountStateFromEntry(entryXDR string, bal int64, ledgerSeq uint32) (AccountState, bool) {
	var le xdr.LedgerEntry
	if err := xdr.SafeUnmarshalBase64(entryXDR, &le); err != nil {
		return AccountState{}, false
	}
	acc, ok := le.Data.GetAccount()
	if !ok {
		return AccountState{}, false
	}
	liab := acc.Liabilities()
	st := AccountState{
		Exists:             true,
		Balance:            bal,
		SeqNum:             int64(acc.SeqNum),
		NumSubEntries:      uint32(acc.NumSubEntries),
		Flags:              uint32(acc.Flags),
		BuyingLiabilities:  int64(liab.Buying),
		SellingLiabilities: int64(liab.Selling),
		NumSponsoring:      uint32(acc.NumSponsoring()),
		NumSponsored:       uint32(acc.NumSponsored()),
		HomeDomain:         string(acc.HomeDomain),
		MasterWeight:       byte(acc.Thresholds[0]),
		ThreshLow:          byte(acc.Thresholds[1]),
		ThreshMed:          byte(acc.Thresholds[2]),
		ThreshHigh:         byte(acc.Thresholds[3]),
		LastModifiedLedger: ledgerSeq,
	}
	for _, s := range acc.Signers {
		st.Signers = append(st.Signers, AccountSigner{Key: signerAddress(s.Key), Weight: uint32(s.Weight)})
	}
	return st, true
}

// trustlineStateFromEntry builds one trustline from its key-derived asset
// id, balance column and stored TrustLineEntry XDR. A corrupt entry keeps
// the balance and leaves the entry-only fields zero.
func trustlineStateFromEntry(asset, entryXDR string, bal int64) TrustlineState {
	t := TrustlineState{Asset: asset, Balance: bal, PoolShare: strings.HasPrefix(asset, "pool:")}
	var le xdr.LedgerEntry
	if xdr.SafeUnmarshalBase64(entryXDR, &le) != nil {
		return t
	}
	if tl, ok := le.Data.GetTrustLine(); ok {
		liab := tl.Liabilities()
		t.Limit = int64(tl.Limit)
		t.Flags = uint32(tl.Flags)
		t.BuyingLiabilities = int64(liab.Buying)
		t.SellingLiabilities = int64(liab.Selling)
	}
	return t
}

// accountOffersQuery — same PK-prefix range shape + rationale as
// accountTrustlinesQuery (offer LedgerKeys start with the seller's
// AccountId after the discriminant).
const accountOffersQuery = `SELECT entry_xdr AS ex
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'offer' AND key_xdr LIKE ?
		  AND account_id = ? AND change_type != 'removed'` + explorerScanSettings

func (r *ExplorerReader) accountOffers(ctx context.Context, account string) ([]OfferState, error) {
	prefix, err := accountEntryKeyPrefix(account, xdr.LedgerEntryTypeOffer)
	if err != nil {
		return nil, err
	}
	rows, err := r.conn.Query(ctx, accountOffersQuery, prefix+"%", account)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: account offers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []OfferState
	for rows.Next() {
		var ex string
		if err := rows.Scan(&ex); err != nil {
			return nil, fmt.Errorf("clickhouse: scan offer: %w", err)
		}
		var le xdr.LedgerEntry
		if xdr.SafeUnmarshalBase64(ex, &le) != nil {
			continue
		}
		o, ok := le.Data.GetOffer()
		if !ok {
			continue
		}
		out = append(out, OfferState{
			OfferID: int64(o.OfferId),
			Selling: xdrjson.AssetID(o.Selling),
			Buying:  xdrjson.AssetID(o.Buying),
			Amount:  int64(o.Amount),
			PriceN:  int32(o.Price.N),
			PriceD:  int32(o.Price.D),
		})
	}
	return out, rows.Err()
}

// assetHoldersQuery / assetHoldersCountQuery are AssetHolders' two FINAL
// scans over the trustline prefix (idx_lecur_asset bloom). Scan-shaped —
// their cost scales with the ASSET's holder count, not the request — hence
// the explorerScanSettings pin (route-sweep 2026-07-29: one huge asset's
// /v1/assets/{id}/holders was in the 8s 503 class; latency for repeats is
// the hot_reads.go cache's job, the pin bounds the scan that DOES run).
const (
	assetHoldersQuery = `SELECT account_id, balance
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'trustline' AND asset = ? AND change_type != 'removed' AND balance > 0
		ORDER BY balance DESC
		LIMIT ?` + explorerScanSettings
	assetHoldersCountQuery = `SELECT toInt64(count())
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'trustline' AND asset = ? AND change_type != 'removed' AND balance > 0` + explorerScanSettings
)

// nativeHoldersQuery / nativeHoldersCountQuery are the NATIVE-XLM arm of
// AssetHolders. Native XLM has NO trustlines — every account holds XLM in
// its AccountEntry balance — so the trustline-shaped queries above return
// an empty board with holder_count 0 BY CONSTRUCTION for it (live bug,
// 2026-07-31: /v1/assets/native/holders served {"holder_count":0} instantly
// while every issued asset's board did real work). The native board ranks
// the ACCOUNT range instead. entry_type is the FIRST column of the table's
// ORDER BY (entry_type, key_xdr), so this is a primary-index RANGE read
// over the account rows (30.7M of the 43.6M current-state total), not a
// whole-table scan — measured on r1 2026-07-31 under this exact SETTINGS
// pin: 2.36s ranking + 2.11s count (9,915,982 funded accounts). Same cost
// class as a large issued asset's trustline board, and like every holders
// board it is served exclusively through the explorer's SWR cache
// (hot_reads.go) — the scans run on the detached 90s refresh budget, never
// a request deadline once warm.
const (
	nativeHoldersQuery = `SELECT account_id, balance
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'account' AND change_type != 'removed' AND balance > 0
		ORDER BY balance DESC
		LIMIT ?` + explorerScanSettings
	nativeHoldersCountQuery = `SELECT toInt64(count())
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'account' AND change_type != 'removed' AND balance > 0` + explorerScanSettings
)

// AssetHolders returns the top holders of an asset by current balance, plus
// the total count of holders with a positive balance. For issued assets the
// balance is the holder's trustline balance; for `asset == "native"` it is
// the AccountEntry XLM balance (see nativeHoldersQuery — native has no
// trustlines) and the count is the number of funded accounts. Pure SQL —
// no per-holder XDR decode. Callers pass the CANONICAL board key: the
// handler folds XLM's alias forms (crypto:XLM — canonical.AssetAliases)
// down to "native" before reaching here.
func (r *ExplorerReader) AssetHolders(ctx context.Context, asset string, limit int) ([]AssetHolder, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	// Precomputed fast path (inventory #4): keyed reads off the 30-min
	// rollup — the difference between sub-millisecond and two FINAL
	// scans per request. Read errors fall through to the legacy path
	// (availability over speed); ok=false means the rollup isn't
	// provisioned/usable.
	if out, total, ok, err := r.holdersRollupBoard(ctx, asset, limit); err == nil && ok {
		return out, total, nil
	}
	if asset == "native" {
		return r.holdersBoard(ctx, nativeHoldersQuery, []any{limit}, nativeHoldersCountQuery, nil)
	}
	return r.holdersBoard(ctx, assetHoldersQuery, []any{asset, limit}, assetHoldersCountQuery, []any{asset})
}

// holdersBoard runs one (ranking, count) holders-query pair — the shared
// scan/aggregate shape of the trustline and native arms of AssetHolders.
func (r *ExplorerReader) holdersBoard(ctx context.Context, holdersQ string, holdersArgs []any, countQ string, countArgs []any) ([]AssetHolder, int64, error) {
	rows, err := r.conn.Query(ctx, holdersQ, holdersArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("clickhouse: asset holders: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AssetHolder
	for rows.Next() {
		var h AssetHolder
		if err := rows.Scan(&h.AccountID, &h.Balance); err != nil {
			return nil, 0, fmt.Errorf("clickhouse: scan holder: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	var total int64
	if err := r.conn.QueryRow(ctx, countQ, countArgs...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("clickhouse: asset holder count: %w", err)
	}
	return out, total, nil
}

// AccountWealth is one row of the wealth-ranked accounts directory.
type AccountWealth struct {
	AccountID string
	// USD is the ranking key: sum(balance × price) in float64. On the usd basis
	// it is dollars; on the native_xlm basis read NativeStroops instead.
	USD float64
	// NativeStroops is the account entry's exact XLM balance in stroops — the
	// served value on the native_xlm basis, where USD's float64 cannot carry
	// the 7th decimal above 2^53 stroops (~900.7M XLM).
	NativeStroops canonical.Amount
	// Locked marks a provably-unspendable account (a locked burn address —
	// master weight 0 and all thresholds 0). Resolved by the background
	// refresh so it is served from cache; do NOT resolve it on the request
	// path (site-audit S3: AccountsUnspendable is a FINAL scan, 6-8s, and it
	// was the residual /v1/accounts latency after the ranking itself was
	// cached).
	Locked bool
}

// accountsByWealthQuery is AccountsByWealth's SQL. balance is stroops (1e7);
// k = "native" for the account entry, else the trustline asset.
// has(assets, k) keeps only priced rows; indexOf maps the key to its price.
// Sum per account, rank desc. native_stroops carries the exact XLM balance
// (widened before summing) and breaks float ties, so the native_xlm order is
// exact too.
//
// This is a background-refresh query (never on a request deadline — see
// accounts_wealth_cache.go). The FINAL scan of 43.6M current-state rows
// measured ~23s on R1 and is close to the connection's default 30s
// max_execution_time, which real production price arrays (30+ assets) plus
// serving contention tip over. The refresh has a 3-minute Go budget; the
// max_execution_time = 150 gives the CH side matching headroom so the query
// completes and the cache populates, instead of dying silently at 30s.
//
// max_threads/max_memory (route-sweep 2026-07-29): at DEFAULT threads the
// whole-table FINAL fan-out over the post-D3 part layout is the 40× memory
// class — the refresh died repeatedly, so the cache never filled and
// /v1/accounts sat on its 503 warming state forever. Pinning the refresh is
// what actually un-503s the route; the cache only ever serves what a
// completed refresh stored. The settings live in SQL text (not
// clickhouse.WithSettings) so the pin is test-assertable and immune to the
// driver's observed context-settings drop (see cbLookupCreatesQuery).
const accountsByWealthQuery = `SELECT account_id,
		sum(toFloat64(balance) / 1e7 * arrayElement(?, indexOf(?, k))) AS usd,
		sumIf(toInt128(balance), k = 'native') AS native_stroops
		FROM (
			SELECT account_id, balance, if(entry_type = 'account', 'native', asset) AS k
			FROM stellar.ledger_entries_current FINAL
			WHERE change_type != 'removed' AND entry_type IN ('account', 'trustline')
		)
		WHERE has(?, k)
		GROUP BY account_id
		HAVING usd > 0
		ORDER BY usd DESC, native_stroops DESC
		LIMIT ?
		SETTINGS max_threads = 4, max_memory_usage = 8589934592, max_execution_time = 150`

// AccountsByWealth ranks accounts by total USD value of their holdings —
// native XLM (the account entry) plus every trustline asset for which the
// caller supplied a USD price. assets/prices are parallel arrays (assets[i]
// priced at prices[i]; the native XLM key is "native"). Computed over the
// current-state projection in one pass (sum balance×price per account); only
// priced assets contribute. Coverage tracks the entry-change capture +
// backfill — accounts/assets not yet captured simply aren't ranked yet.
func (r *ExplorerReader) AccountsByWealth(ctx context.Context, assets []string, prices []float64, limit int) ([]AccountWealth, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if len(assets) == 0 || len(assets) != len(prices) {
		return nil, nil
	}
	rows, err := r.conn.Query(ctx, accountsByWealthQuery, prices, assets, assets, limit)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: accounts by wealth: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AccountWealth
	for rows.Next() {
		var (
			w      AccountWealth
			native big.Int
		)
		if err := rows.Scan(&w.AccountID, &w.USD, &native); err != nil {
			return nil, fmt.Errorf("clickhouse: scan account wealth: %w", err)
		}
		w.NativeStroops = canonical.NewAmount(&native)
		out = append(out, w)
	}
	return out, rows.Err()
}

// accountsUnspendableQuery — an account_id-bloom-probed FINAL scan (the IN
// list keeps the probe count small, so the bloom stays effective, but the
// scan shape is the same as accountTrustlinesQuery). Runs on the wealth
// cache's background refresh only; pinned for the same fan-out reason as
// accountsByWealthQuery.
const accountsUnspendableQuery = `SELECT account_id, entry_xdr FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'account' AND account_id IN (?) AND change_type != 'removed'` + explorerScanSettings

// AccountsUnspendable reports which of the given accounts are locked
// burn addresses: master weight 0 with no other signers — stellar-core
// only ever admits the master key as a signer when its weight is
// nonzero, so master weight 0 plus an empty signer list means no
// signature set can ever reach ANY threshold, including a nonzero one
// (Pass-B ACC-1: the SDF burn address ranked as the "richest account",
// $11.3B of dead XLM presented as wealth). Decoded from the current
// account entry XDR. Threshold values are irrelevant to reachability
// here: they gate which OPERATIONS a given signing weight authorizes,
// not whether any weight can ever be produced.
func (r *ExplorerReader) AccountsUnspendable(ctx context.Context, accountIDs []string) (map[string]bool, error) {
	if len(accountIDs) == 0 {
		return nil, nil
	}
	rows, err := r.conn.Query(ctx, accountsUnspendableQuery, accountIDs)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: accounts unspendable: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]bool)
	for rows.Next() {
		var id, entryB64 string
		if err := rows.Scan(&id, &entryB64); err != nil {
			return nil, fmt.Errorf("clickhouse: scan unspendable: %w", err)
		}
		var entry xdr.LedgerEntry
		if xdr.SafeUnmarshalBase64(entryB64, &entry) != nil {
			continue
		}
		acc, ok := entry.Data.GetAccount()
		if !ok {
			continue
		}
		if accountIsUnspendable(acc.Thresholds, len(acc.Signers)) {
			out[id] = true
		}
	}
	return out, rows.Err()
}

// accountIsUnspendable is the reachability check behind AccountsUnspendable:
// master weight 0 with zero other signers means no signature set exists at
// any weight, so the account is locked regardless of its threshold values
// (thresholds gate which operations a given weight authorizes, not whether
// any weight is reachable at all).
func accountIsUnspendable(th xdr.Thresholds, numSigners int) bool {
	return th.MasterKeyWeight() == 0 && numSigners == 0
}

// signerAddress renders a SignerKey strkey without panicking on an unknown
// discriminant (degrades to "").
func signerAddress(k xdr.SignerKey) string {
	s, err := k.GetAddress()
	if err != nil {
		return ""
	}
	return s
}

// AccountHomeDomains returns account → home_domain for the given accounts that
// carry a non-empty home_domain in the current-state projection. Batch helper
// for the issuer-enrich backfill: the lake doesn't denormalize home_domain to a
// column, so it's decoded from the account entry XDR. Accounts with no entry /
// no home_domain are simply absent from the map.
func (r *ExplorerReader) AccountHomeDomains(ctx context.Context, accounts []string) (map[string]string, error) {
	if len(accounts) == 0 {
		return map[string]string{}, nil
	}
	const q = `SELECT account_id, entry_xdr FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'account' AND account_id IN (?) AND change_type != 'removed' AND entry_xdr != ''`
	rows, err := r.conn.Query(ctx, q, accounts)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: account home_domains: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]string)
	for rows.Next() {
		var acct, entryXDR string
		if err := rows.Scan(&acct, &entryXDR); err != nil {
			return nil, fmt.Errorf("clickhouse: scan home_domain: %w", err)
		}
		var le xdr.LedgerEntry
		if xdr.SafeUnmarshalBase64(entryXDR, &le) != nil {
			continue
		}
		if acc, ok := le.Data.GetAccount(); ok {
			if hd := string(acc.HomeDomain); hd != "" {
				out[acct] = hd
			}
		}
	}
	return out, rows.Err()
}

// accountKeyXDR returns the base64 LedgerKey XDR for an account G-strkey —
// the primary-key form of stellar.ledger_entries_current, so a lookup on
// it is a PK-prefix point read rather than a full-column scan on
// account_id. Mirrors liquidityPoolKeyXDR / instanceKeyXDR.
func accountKeyXDR(gStrkey string) (string, error) {
	var aid xdr.AccountId
	if err := aid.SetAddress(gStrkey); err != nil {
		return "", fmt.Errorf("clickhouse: account key %q: %w", gStrkey, err)
	}
	lk := xdr.LedgerKey{
		Type:    xdr.LedgerEntryTypeAccount,
		Account: &xdr.LedgerKeyAccount{AccountId: aid},
	}
	b64, err := xdr.MarshalBase64(lk)
	if err != nil {
		return "", fmt.Errorf("clickhouse: marshal account key: %w", err)
	}
	return b64, nil
}

// accountEntryKeyPrefix returns a base64 STRING prefix that matches every
// ledger_entries_current key_xdr of the given LedgerEntryType belonging to
// the account — the PK-range form of "this account's trustlines/offers".
//
// Why it works: both LedgerKey shapes start
// [type discriminant (4B)] [AccountId: key type (4B) + 32 raw key bytes],
// so an account's entries of one type share a fixed 40-byte binary prefix
// and are CONTIGUOUS under the table's ORDER BY (entry_type, key_xdr).
// key_xdr is stored as base64 TEXT, and base64 is prefix-stable only at
// 3-byte boundaries, so the prefix is cut at 39 bytes (52 base64 chars) —
// one raw byte short of the full account. That residual ambiguity (a
// neighbour key differing only in the last account byte) is closed by the
// caller keeping its exact `account_id = ?` filter; the prefix's job is
// only to turn the read into a primary-index range.
//
// Measured on r1 (2026-07-30, the route-sweep whale account): trustline
// read 5.18s via the account_id bloom skip-index → 0.069s via this prefix
// — the difference between /v1/accounts/{g} needing the whole
// stale-serving apparatus and answering interactively.
func accountEntryKeyPrefix(gStrkey string, entryType xdr.LedgerEntryType) (string, error) {
	var aid xdr.AccountId
	if err := aid.SetAddress(gStrkey); err != nil {
		return "", fmt.Errorf("clickhouse: account key prefix %q: %w", gStrkey, err)
	}
	raw := make([]byte, 0, 40)
	raw = append(raw,
		byte(uint32(entryType)>>24), byte(uint32(entryType)>>16),
		byte(uint32(entryType)>>8), byte(uint32(entryType)))
	aidBytes, err := aid.MarshalBinary()
	if err != nil {
		return "", fmt.Errorf("clickhouse: marshal account id: %w", err)
	}
	raw = append(raw, aidBytes...)
	if len(raw) < 39 {
		return "", fmt.Errorf("clickhouse: account key prefix: unexpected %d-byte key head", len(raw))
	}
	return base64.StdEncoding.EncodeToString(raw[:39]), nil
}
