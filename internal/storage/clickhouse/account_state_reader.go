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
	// Liabilities (ext.v1) and sponsorship counts (ext.v2) give
	// min = (2 + NumSubEntries + NumSponsoring - NumSponsored) × base_reserve and
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
	// AsOfLedger is the lake watermark read BEFORE the scan, so it never names a ledger
	// later than the data. Stamped by [ExplorerReader.refreshAccountState]; a live read
	// that bypasses the cache gets 0, as does an unreadable watermark.
	AsOfLedger uint32
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

// AccountState reconstructs an account's current state: the latest AccountEntry plus
// its live trustlines and offers (latest non-removed change per key). Exists=false,
// no error, for an unknown or merged account.
func (r *ExplorerReader) AccountState(ctx context.Context, account string) (AccountState, error) {
	st, err := r.accountEntry(ctx, account)
	if errors.Is(err, errCorruptAccountEntry) {
		// A corrupt stored entry degrades to "no state" rather than a 500.
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

// AccountSigners returns the entry-level state without trustlines and offers. Unlike
// [ExplorerReader.AccountState], a corrupt entry is an error: an authentication
// check must not read it as "no account".
func (r *ExplorerReader) AccountSigners(ctx context.Context, account string) (AccountState, error) {
	return r.accountEntry(ctx, account)
}

var errCorruptAccountEntry = errors.New("clickhouse: corrupt stored account entry")

// accountEntry reads the latest AccountEntry; errCorruptAccountEntry when it does
// not decode.
func (r *ExplorerReader) accountEntry(ctx context.Context, account string) (AccountState, error) {
	var st AccountState

	// FINAL: ledger_entries_current is ReplacingMergeTree; a trailing 'removed' = merged.
	// Query by key_xdr, not account_id: the table is ORDER BY (entry_type, key_xdr), so
	// the account's LedgerKey XDR is a PK point lookup and account_id full-scans.
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
			// Unknown account: empty state via Exists=false, not an error.
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

// accountTrustlinesQuery is a primary-index range read: an account's trustline keys
// share a key_xdr prefix (accountEntryKeyPrefix); the exact account_id equality
// closes the prefix's one-byte residual. The scan-settings pin is a guard rail.
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

// Same PK-prefix range shape as accountTrustlinesQuery (offer keys start with the seller).
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

// Two FINAL scans over the trustline prefix (idx_lecur_asset bloom). Cost scales with
// the asset's holder count, hence the explorerScanSettings pin; repeat latency is
// hot_reads.go's job.
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

// Native XLM has no trustlines, so the queries above would return an empty board with
// holder_count 0 by construction. Rank the 'account' rows instead: entry_type leads
// the sort key, so it is a primary-index range read. Served via the SWR cache
// (hot_reads.go), never on a request deadline once warm.
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

// AssetHolders returns the top holders by current balance plus the count of positive
// balances. For "native" the balance is the AccountEntry XLM balance (see
// nativeHoldersQuery). Callers pass the canonical board key: the handler folds XLM
// alias forms (canonical.AssetAliases) to "native" first.
func (r *ExplorerReader) AssetHolders(ctx context.Context, asset string, limit int) ([]AssetHolder, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	// Precomputed rollup first; a read error falls through to the live scans
	// (availability over speed), ok=false means the rollup is not usable.
	if out, total, ok, err := r.holdersRollupBoard(ctx, asset, limit); err == nil && ok {
		return out, total, nil
	}
	if asset == "native" {
		return r.holdersBoard(ctx, nativeHoldersQuery, []any{limit}, nativeHoldersCountQuery, nil)
	}
	return r.holdersBoard(ctx, assetHoldersQuery, []any{asset, limit}, assetHoldersCountQuery, []any{asset})
}

// holdersBoard runs one (ranking, count) query pair.
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
	// Value is the ranking key and served figure: exact sum of balance × price in whole
	// units of the basis (dollars on usd, XLM on native_xlm).
	Value *big.Rat
	// NativeStroops is the exact XLM balance in stroops, the served value on native_xlm,
	// where float64 cannot carry the 7th decimal above 2^53 stroops.
	NativeStroops canonical.Amount
	// Locked marks a provably-unspendable account (locked burn address). Resolved by the
	// background refresh; never resolve it on the request path (FINAL scan, seconds).
	Locked bool
}

// balance is stroops (1e7); k = "native" for the account entry, else the trustline
// asset; only priced rows (has(assets, k)) count. The sum is Decimal256, never
// Float64, because it is the served figure; stroop-to-unit division happens in Go.
// Background refresh only (accounts_wealth_cache.go): the FINAL scan is slow, so
// threads, memory and max_execution_time are pinned in SQL text (test-assertable;
// the driver drops context settings, see cbLookupCreatesQuery).
const accountsByWealthQuery = `WITH arrayMap(p -> toDecimal256(p, 18), ?) AS px,
		sum(toDecimal256(balance, 0) * arrayElement(px, indexOf(?, k))) AS stroop_value
		SELECT account_id, toString(stroop_value),
		sumIf(toInt128(balance), k = 'native') AS native_stroops
		FROM (
			SELECT account_id, balance, if(entry_type = 'account', 'native', asset) AS k
			FROM stellar.ledger_entries_current FINAL
			WHERE change_type != 'removed' AND entry_type IN ('account', 'trustline')
		)
		WHERE has(?, k)
		GROUP BY account_id
		HAVING stroop_value > 0
		ORDER BY stroop_value DESC, native_stroops DESC, account_id
		LIMIT ?
		SETTINGS max_threads = 4, max_memory_usage = 8589934592, max_execution_time = 150`

// stroopsPerUnit converts a stroop-denominated sum to whole units.
var stroopsPerUnit = big.NewRat(10_000_000, 1)

// wealthPriceScale is the precision accountsByWealthQuery casts prices to; rendering
// to it keeps ClickHouse from meeting an unparseable form and rounds visibly.
const wealthPriceScale = 18

// wealthPriceArgs renders prices as plain decimals, refusing a non-positive one: one
// bad element would fail the whole ranking.
func wealthPriceArgs(prices []string) ([]string, error) {
	out := make([]string, len(prices))
	for i, p := range prices {
		r, ok := new(big.Rat).SetString(p)
		if !ok || r.Sign() <= 0 {
			return nil, fmt.Errorf("clickhouse: accounts by wealth: price %q is not a positive decimal", p)
		}
		out[i] = r.FloatString(wealthPriceScale)
	}
	return out, nil
}

// AccountsByWealth ranks accounts by USD value of native XLM plus every trustline
// asset with a caller-supplied price (assets/prices are parallel arrays; native key
// is "native"). Unpriced or uncaptured assets simply do not contribute.
func (r *ExplorerReader) AccountsByWealth(ctx context.Context, assets, prices []string, limit int) ([]AccountWealth, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if len(assets) == 0 || len(assets) != len(prices) {
		return nil, nil
	}
	px, err := wealthPriceArgs(prices)
	if err != nil {
		return nil, err
	}
	rows, err := r.conn.Query(ctx, accountsByWealthQuery, px, assets, assets, limit)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: accounts by wealth: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AccountWealth
	for rows.Next() {
		var (
			w      AccountWealth
			stroop string
			native big.Int
		)
		if err := rows.Scan(&w.AccountID, &stroop, &native); err != nil {
			return nil, fmt.Errorf("clickhouse: scan account wealth: %w", err)
		}
		v, ok := new(big.Rat).SetString(stroop)
		if !ok {
			return nil, fmt.Errorf("clickhouse: account wealth %s: unparseable sum %q", w.AccountID, stroop)
		}
		w.Value = v.Quo(v, stroopsPerUnit)
		w.NativeStroops = canonical.NewAmount(&native)
		out = append(out, w)
	}
	return out, rows.Err()
}

// account_id-bloom-probed FINAL scan; the IN list keeps probes few. Wealth-cache
// background refresh only.
const accountsUnspendableQuery = `SELECT account_id, entry_xdr FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'account' AND account_id IN (?) AND change_type != 'removed'` + explorerScanSettings

// AccountsUnspendable reports locked burn addresses: master weight 0 and no other
// signers, so no signature set reaches ANY threshold. Thresholds are irrelevant:
// they gate which operations a weight authorizes, not whether a weight is reachable.
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

// accountIsUnspendable: master weight 0 with no other signers is locked regardless
// of thresholds.
func accountIsUnspendable(th xdr.Thresholds, numSigners int) bool {
	return th.MasterKeyWeight() == 0 && numSigners == 0
}

// signerAddress renders a SignerKey strkey, "" for an unknown discriminant.
func signerAddress(k xdr.SignerKey) string {
	s, err := k.GetAddress()
	if err != nil {
		return ""
	}
	return s
}

// AccountHomeDomains returns account → home_domain for accounts with a live,
// decodable entry (decoded from XDR; the lake has no home_domain column). Keep three
// states apart: non-empty = declared domain, "" = entry read, none declared, absent
// key = not read.
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
			out[acct] = string(acc.HomeDomain)
		}
	}
	return out, rows.Err()
}

// accountKeyXDR returns the base64 LedgerKey XDR for a G-strkey, the primary-key form
// of ledger_entries_current (PK point read, not an account_id scan).
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

// accountEntryKeyPrefix returns a base64 prefix matching every key_xdr of the given
// entry type for the account, turning the read into a primary-index range. Both key
// shapes start [type (4B)][AccountId (4B + 32B)], contiguous under ORDER BY. Base64
// is prefix-stable only at 3-byte boundaries, so cut at 39 bytes (52 chars); the
// caller's exact `account_id = ?` closes the one-byte residual.
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
