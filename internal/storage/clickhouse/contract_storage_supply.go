package clickhouse

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math/big"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// ContractStorageSupply is a token's supply summed from the balance ledger entries its contract
// holds in Soroban storage, not from the event log. It is a DISTRIBUTION level; the event-derived
// supply ([TokenSupply]) is an issuance accumulation, too low for a log with gaps and a confident
// zero for a token with no events. Never add the two. See the ContractStorageSupply method for the
// handover rule.
type ContractStorageSupply struct {
	ContractID string

	// Total is Σ of every Balance(Address) → i128 entry, raw smallest unit (ADR-0003: never a JSON
	// number).
	Total *big.Int

	// Decimals is read from the contract's own instance storage; DecimalsFound reports whether the
	// chain declared one. Do not publish a decimalised figure when false: the exponent would be
	// invented.
	Decimals      uint32
	DecimalsFound bool

	// BalanceEntries is how many balances were summed. Zero with a nil error means no balances in
	// storage, normal for a SAC's classic asset (its balances live in trustlines).
	BalanceEntries int

	// DeclaredTotal is the contract's own TotalSupply from instance storage (nil if absent): an
	// independent measurement, so agreement is evidence every entry was decoded (SelfConsistent).
	DeclaredTotal *big.Int

	// DeclaredHolders is the contract's own HolderCount (nil if absent): the completeness check,
	// since a balance the current-state projection never captured is missing from the sum.
	DeclaredHolders *uint32

	// ArchivedEntries/ArchivedTotal are the part of BalanceEntries/Total in persistent balances
	// whose TTL had lapsed at the tip. They stay in the sum: archived persistent balances are still
	// owned and restorable. ArchivedTotal is nil when ArchivedEntries is zero.
	ArchivedEntries int
	ArchivedTotal   *big.Int

	// AsOfLedger is the highest ledger any summed entry was last written at.
	AsOfLedger uint32

	// balances holds every decoded balance until [ContractStorageSupply.settle]
	// has judged it against its TTL.
	balances []heldBalance

	// isSAC/sawInstance stay unexported so refusals can only leave this package as an error; a
	// caller must not read the fields and serve the sum anyway.
	isSAC       bool
	sawInstance bool
}

// HasSelfChecks reports whether the contract published anything to cross-check. Gate SelfConsistent
// on it: "nothing to check" is not a verdict.
func (s ContractStorageSupply) HasSelfChecks() bool {
	return s.DeclaredTotal != nil || s.DeclaredHolders != nil
}

// SelfConsistent reports whether every cross-check the contract offered agreed with what we
// decoded. TRUE when it offered none (absence of contradiction); gate on HasSelfChecks to tell the
// difference.
func (s ContractStorageSupply) SelfConsistent() bool {
	if s.DeclaredTotal != nil && s.Total != nil && s.DeclaredTotal.Cmp(s.Total) != 0 {
		return false
	}
	if s.DeclaredHolders != nil && int(*s.DeclaredHolders) != s.BalanceEntries {
		return false
	}
	return true
}

// ErrStorageSupplyIsStellarAsset is a REFUSAL for a Stellar Asset Contract: its storage holds only
// the wrapped slice of a classic asset (the rest is in trustlines, claimable balances and pool
// reserves), so summing it understates supply with no sign of being wrong (6.8x on KALE). The
// classic path serves the asset's supply.
var ErrStorageSupplyIsStellarAsset = fmt.Errorf("clickhouse: contract is a Stellar Asset Contract; its storage holds only the wrapped slice of a classic asset")

// ErrStorageSupplyTooManyEntries is a refusal above maxContractStorageBalanceEntries: every entry
// is decoded in Go, and a truncated sum is a wrong supply that looks right.
var ErrStorageSupplyTooManyEntries = fmt.Errorf("clickhouse: contract holds more storage balance entries than this reader will decode")

// ErrStorageSupplyNoInstance: balances exist but no instance entry was captured. The instance is
// the only evidence separating a Wasm token from a SAC, so the SAC refusal has not been evaluated,
// merely not fired. See ErrStorageSupplyIsStellarAsset.
var ErrStorageSupplyNoInstance = fmt.Errorf("clickhouse: no contract instance entry captured, so the Stellar-Asset-Contract check could not be run")

// maxContractStorageBalanceEntries bounds one storage-supply read, which pays a full decode per
// entry inside a request budget. 25,000 is far above the closed holder sets this basis serves (1 to
// 12 holders) and below the largest pubnet holder sets (~62k), which are event-emitting tokens
// answered by the flow-derived reading first.
const maxContractStorageBalanceEntries = 25_000

// balanceKeyMarker is the fixed base64 window a Balance(Address) contract-data key carries;
// instanceKeyMarker is the instance one. A key's first 40 bytes are (type, address type, contract
// id), so the SCVal key lands at a FIXED base64 offset: plain string comparisons, no per-row XDR
// parse.
// Markers are a PRE-FILTER ONLY; survivors are re-checked by balanceKeyHolder. `BalanceCheckpoints`
// (a vector of past balances) shares the leading symbol text `Balance` and differs only in the
// symbol LENGTH bytes the marker pins.
const (
	balanceKeyMarker = "ABAAAAABAAAAAgAAAA8AAAAHQmFsYW5jZQAAAAAS"
	// holderCountKeyMarker matches Vec[Symbol("HolderCount")], a top-level persistent entry: the
	// contract's own holder count, the completeness check for this basis.
	holderCountKeyMarker = "ABAAAAABAAAAAQAAAA8AAAALSG9sZGVyQ291bnQA"
	instanceKeyMarker    = "ABQAAAAB"
	// wideMarkerChars is the shared length of the two vector-key markers.
	wideMarkerChars = 40
	// contractKeyPrefixChars is the base64 length of the (type, address)
	// header — bytes 0..39 of every contract-data key for one contract.
	contractKeyPrefixChars = 52
	// markerOffset is the 1-indexed SQL substring position where the SCVal
	// key's encoding begins, i.e. just past contractKeyPrefixChars.
	markerOffset = contractKeyPrefixChars + 5
)

// contractStorageSupplyQuery reads one contract's balance + instance entries. startsWith() on
// key_xdr is a primary-key prefix range ((entry_type, key_xdr) lead the ORDER BY), the same shape
// as TokenDecimals.
// FINAL is mandatory: ledger_entries_current is a ReplacingMergeTree, so without it an updated
// balance returns once per unmerged part and stale copies are summed. `change_type != 'removed'`
// drops deleted entries: a removed balance no longer exists.
const contractStorageSupplyQuery = `
	SELECT key_xdr, entry_xdr, ledger_seq
	FROM stellar.ledger_entries_current FINAL
	WHERE entry_type = 'contract_data'
	  AND startsWith(key_xdr, ?)
	  AND change_type != 'removed'
	  AND entry_xdr != ''
	  AND (substring(key_xdr, ?, ?) IN (?) OR substring(key_xdr, ?, ?) = ?)
	LIMIT ?`

// ContractStorageSupplyReader is the seam the storage-derived supply reading is
// served through. Implemented by [ExplorerReader].
type ContractStorageSupplyReader interface {
	ContractStorageSupply(ctx context.Context, contractID string) (ContractStorageSupply, error)
}

// ContractStorageSupply sums a token's supply out of its Soroban contract storage, for tokens the
// SEP-41 / CAP-67 event log cannot see (they are ABSENT from supply_flows, so events sum to a
// confident zero).
// Handover: a token with both a storage level and an event accumulation takes the storage reading
// outright and the two are never added; a level cannot be wrong from missing history. Events stay a
// cross-check. Measured: storage reproduced the event-derived totals of three complete Wasm tokens
// exactly.
// Expiry: the lake never records an eviction, so every balance is judged against
// stellar.ttl_live_until at the lake tip. A lapsed TEMPORARY balance is dropped; a lapsed
// PERSISTENT one is archived, still owned, and stays in Total (disclosed via
// ArchivedEntries/ArchivedTotal). A key with no TTL row is kept.
// Blind spot: only entries the current-state projection captured are seen, so the figure is a LOWER
// BOUND; callers must carry the bound to the wire. The contract's HolderCount exposes the gap.
// Refusals: ErrStorageSupplyIsStellarAsset, ErrStorageSupplyTooManyEntries,
// ErrStorageSupplyNoInstance, and failure when the TTL lookup cannot run (an unjudged sum could
// include deleted balances). A contract with no balances returns zero Total, BalanceEntries == 0
// and no error; do not publish that as a supply.
func (r *ExplorerReader) ContractStorageSupply(ctx context.Context, contractID string) (ContractStorageSupply, error) {
	prefix, err := contractDataKeyPrefix(contractID)
	if err != nil {
		return ContractStorageSupply{}, err
	}
	rows, err := r.conn.Query(ctx, contractStorageSupplyQuery,
		prefix,
		markerOffset, wideMarkerChars, []string{balanceKeyMarker, holderCountKeyMarker},
		markerOffset, len(instanceKeyMarker), instanceKeyMarker,
		maxContractStorageBalanceEntries+1,
	)
	if err != nil {
		return ContractStorageSupply{}, fmt.Errorf("clickhouse: contract storage supply %s: %w", contractID, err)
	}
	defer func() { _ = rows.Close() }()

	out := ContractStorageSupply{ContractID: contractID, Total: new(big.Int)}
	deferred, err := out.scanInstanceFirst(rows)
	if err != nil {
		return ContractStorageSupply{}, err
	}
	for _, row := range deferred {
		if err := out.apply(row.keyB64, row.entryB64, row.ledger); err != nil {
			return ContractStorageSupply{}, err
		}
	}
	if out.isSAC {
		return ContractStorageSupply{}, fmt.Errorf("%w: %s", ErrStorageSupplyIsStellarAsset, contractID)
	}
	if out.BalanceEntries > 0 && !out.sawInstance {
		// The instance entry is the only disproof of SAC; without it the SAC refusal has not been
		// evaluated, so refuse.
		return ContractStorageSupply{}, fmt.Errorf(
			"%w: %s (no contract instance entry captured, so the Stellar-Asset-Contract check could not be run)",
			ErrStorageSupplyNoInstance, contractID)
	}
	if err := r.settleLapsedBalances(ctx, &out); err != nil {
		return ContractStorageSupply{}, err
	}
	return out, nil
}

// heldBalance is one decoded balance entry awaiting its TTL verdict.
type heldBalance struct {
	keyB64    string
	amount    *big.Int
	ledger    uint32
	temporary bool
}

// settleLapsedBalances reads every summed balance's live_until and the lake tip
// it is judged at, then hands both to [ContractStorageSupply.settle].
func (r *ExplorerReader) settleLapsedBalances(ctx context.Context, s *ContractStorageSupply) error {
	if len(s.balances) == 0 {
		return nil
	}
	tip, err := r.LakeTipLedger(ctx)
	if err != nil {
		return fmt.Errorf("clickhouse: contract storage supply %s: %w", s.ContractID, err)
	}
	keys := make([]string, len(s.balances))
	for i, b := range s.balances {
		keys[i] = b.keyB64
	}
	liveUntil, err := resolveTTLLiveUntil(ctx, r.conn, keys)
	if err != nil {
		return fmt.Errorf("clickhouse: contract storage supply %s: %w", s.ContractID, err)
	}
	s.settle(liveUntil, tip)
	return nil
}

// settle re-derives the sum under TTL verdicts at tip: lapsed temporary dropped, lapsed persistent
// kept as archived. A live_until below the entry's own last write is stale TTL data (a write needs
// a live entry), so the key stays live.
func (s *ContractStorageSupply) settle(liveUntil map[string]uint32, tip uint32) {
	s.Total, s.BalanceEntries, s.AsOfLedger = new(big.Int), 0, 0
	s.ArchivedEntries, s.ArchivedTotal = 0, nil
	archived := new(big.Int)
	for _, b := range s.balances {
		lu, ok := liveUntil[b.keyB64]
		lapsed := ok && lu >= b.ledger && TTLVerdictAt(lu, tip) == TTLArchived
		if lapsed && b.temporary {
			continue
		}
		s.Total.Add(s.Total, b.amount)
		s.BalanceEntries++
		s.AsOfLedger = max(s.AsOfLedger, b.ledger)
		if lapsed {
			s.ArchivedEntries++
			archived.Add(archived, b.amount)
		}
	}
	if s.ArchivedEntries > 0 {
		s.ArchivedTotal = archived
	}
}

// storageRow is one query row held back, undecoded, until the SAC check has run.
type storageRow struct {
	keyB64, entryB64 string
	ledger           uint32
}

// scanInstanceFirst decodes only the instance entry and holds every other row back undecoded: the
// SAC refusal depends on the instance alone and must not wait behind, or be pre-empted by a decode
// error in, up to 25k balance decodes it will discard.
func (s *ContractStorageSupply) scanInstanceFirst(rows driver.Rows) ([]storageRow, error) {
	var deferred []storageRow
	seen := 0
	for rows.Next() {
		var row storageRow
		if err := rows.Scan(&row.keyB64, &row.entryB64, &row.ledger); err != nil {
			return nil, fmt.Errorf("clickhouse: scan contract storage supply row: %w", err)
		}
		seen++
		if seen > maxContractStorageBalanceEntries {
			return nil, fmt.Errorf("%w: %s (>%d)",
				ErrStorageSupplyTooManyEntries, s.ContractID, maxContractStorageBalanceEntries)
		}
		if !isInstanceKeyB64(row.keyB64) {
			deferred = append(deferred, row)
			continue
		}
		if err := s.apply(row.keyB64, row.entryB64, row.ledger); err != nil {
			return nil, err
		}
		if s.isSAC {
			return nil, fmt.Errorf("%w: %s", ErrStorageSupplyIsStellarAsset, s.ContractID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: contract storage supply %s: %w", s.ContractID, err)
	}
	return deferred, nil
}

// isInstanceKeyB64 reports whether a stored key carries the contract-instance
// marker the query's server-side filter admits it by. A row it misses is still
// decoded later, and apply identifies the instance by decode, not by marker.
func isInstanceKeyB64(keyB64 string) bool {
	start := markerOffset - 1 // markerOffset is 1-indexed for SQL substring()
	return strings.HasPrefix(keyB64[min(start, len(keyB64)):], instanceKeyMarker)
}

// apply folds one decoded row into the running result.
func (s *ContractStorageSupply) apply(keyB64, entryB64 string, ledger uint32) error {
	var key xdr.LedgerKey
	if xdr.SafeUnmarshalBase64(keyB64, &key) != nil {
		// An undecodable key is a decode gap, not an empty holder; skipping it would silently drop
		// a balance, so refuse the reading.
		return fmt.Errorf("clickhouse: contract storage supply %s: undecodable key_xdr", s.ContractID)
	}
	cd, ok := key.GetContractData()
	if !ok {
		return nil
	}
	var entry xdr.LedgerEntry
	if xdr.SafeUnmarshalBase64(entryB64, &entry) != nil {
		return fmt.Errorf("clickhouse: contract storage supply %s: undecodable entry_xdr", s.ContractID)
	}
	ed, ok := entry.Data.GetContractData()
	if !ok {
		return nil
	}

	if cd.Key.Type == xdr.ScValTypeScvLedgerKeyContractInstance {
		s.sawInstance = true
		s.applyInstance(ed.Val)
		return nil
	}
	// HolderCount is a top-level persistent entry on these tokens, not an
	// instance-storage field, so it arrives here rather than in applyInstance.
	if name, ok := instanceStorageKeyName(cd.Key); ok && name == "HolderCount" {
		if u, ok := ed.Val.GetU32(); ok {
			n := uint32(u)
			s.DeclaredHolders = &n
		}
		return nil
	}
	if !balanceKeyHolder(cd.Key) {
		return nil
	}
	amount, ok := balanceAmount(ed.Val)
	if !ok {
		// Key says balance, value unrecognised: refuse rather than treat as zero.
		return fmt.Errorf("clickhouse: contract storage supply %s: balance entry carries no decodable amount", s.ContractID)
	}
	if amount.Sign() < 0 {
		// A negative balance is impossible; we decoded the wrong field, so the sum is
		// untrustworthy.
		return fmt.Errorf("clickhouse: contract storage supply %s: negative balance entry %s", s.ContractID, amount)
	}
	s.Total.Add(s.Total, amount)
	s.BalanceEntries++
	if ledger > s.AsOfLedger {
		s.AsOfLedger = ledger
	}
	s.balances = append(s.balances, heldBalance{
		keyB64: keyB64, amount: amount, ledger: ledger,
		temporary: cd.Durability == xdr.ContractDataDurabilityTemporary,
	})
	return nil
}

// applyInstance reads the cross-checks and the scale out of the instance entry.
func (s *ContractStorageSupply) applyInstance(val xdr.ScVal) {
	inst, ok := val.GetInstance()
	if !ok {
		return
	}
	if inst.Executable.Type == xdr.ContractExecutableTypeContractExecutableStellarAsset {
		s.isSAC = true
		return
	}
	if d, ok := decimalsFromInstance(inst); ok {
		s.Decimals, s.DecimalsFound = d, true
	}
	if inst.Storage == nil {
		return
	}
	for _, kv := range *inst.Storage {
		name, ok := instanceStorageKeyName(kv.Key)
		if !ok {
			continue
		}
		switch name {
		case "TotalSupply":
			if v, ok := balanceAmount(kv.Val); ok {
				s.DeclaredTotal = v
			}
		case "HolderCount":
			if u, ok := kv.Val.GetU32(); ok {
				n := uint32(u)
				s.DeclaredHolders = &n
			}
		}
	}
}

// instanceStorageKeyName returns an instance-storage entry's name under either live encoding: a
// bare Symbol (soroban-token-sdk METADATA) or a single-element vector holding a symbol (Rust
// #[contracttype] fieldless enum variant, used by hand-written tokens with a DataKey enum). Reading
// only the bare symbol leaves DecimalsFound=false and no cross-check for exactly those tokens.
func instanceStorageKeyName(key xdr.ScVal) (string, bool) {
	if sym, ok := key.GetSym(); ok {
		return string(sym), true
	}
	vec, ok := key.GetVec()
	if !ok || vec == nil || len(*vec) != 1 {
		return "", false
	}
	sym, ok := (*vec)[0].GetSym()
	return string(sym), ok
}

// balanceKeyHolder reports whether a key is a per-holder balance: the two-element vector
// [Symbol("Balance"), Address]. The exact-arity check is the point: BalanceCheckpoints(Address)
// holds a vector of past balances, and a prefix match would sum balance history into supply
// (roughly doubling the deal tokens).
func balanceKeyHolder(key xdr.ScVal) bool {
	if key.Type != xdr.ScValTypeScvVec {
		return false
	}
	vec, ok := key.GetVec()
	if !ok || vec == nil || len(*vec) != 2 {
		return false
	}
	sym, ok := (*vec)[0].GetSym()
	if !ok || string(sym) != "Balance" {
		return false
	}
	return (*vec)[1].Type == xdr.ScValTypeScvAddress
}

// balanceAmount pulls the i128 out of a balance value: a bare i128 (soroban-token-sdk) or a map
// with an `amount` field (SAC / older token-sdk BalanceValue).
// An i128 is reassembled from signed Hi and unsigned Lo: reading Lo as signed corrupts every
// balance whose low word has the top bit set.
func balanceAmount(val xdr.ScVal) (*big.Int, bool) {
	switch val.Type {
	case xdr.ScValTypeScvI128:
		parts, ok := val.GetI128()
		if !ok {
			return nil, false
		}
		hi := big.NewInt(int64(parts.Hi))
		lo := new(big.Int).SetUint64(uint64(parts.Lo))
		return new(big.Int).Add(new(big.Int).Lsh(hi, 64), lo), true
	case xdr.ScValTypeScvMap:
		m, ok := val.GetMap()
		if !ok || m == nil {
			return nil, false
		}
		for _, kv := range *m {
			sym, ok := kv.Key.GetSym()
			if !ok || string(sym) != "amount" {
				continue
			}
			return balanceAmount(kv.Val)
		}
	}
	return nil, false
}

// contractDataKeyPrefix returns the base64 prefix of every contract-data key for one contract: 39
// of the 40 leading bytes (type, address discriminant, contract id) are whole base64 groups, so the
// 52-char prefix lets the lookup ride the (entry_type, key_xdr) primary key.
func contractDataKeyPrefix(contractID string) (string, error) {
	raw, err := strkey.Decode(strkey.VersionByteContract, contractID)
	if err != nil {
		return "", fmt.Errorf("clickhouse: contract storage supply: bad contract id %q: %w", contractID, err)
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("clickhouse: contract storage supply: contract id %q decoded to %d bytes, want 32", contractID, len(raw))
	}
	buf := make([]byte, 0, 40)
	buf = binary.BigEndian.AppendUint32(buf, uint32(xdr.LedgerEntryTypeContractData))
	buf = binary.BigEndian.AppendUint32(buf, uint32(xdr.ScAddressTypeScAddressTypeContract))
	buf = append(buf, raw...)
	return base64.StdEncoding.EncodeToString(buf[:39]), nil
}
