package clickhouse

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math/big"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// ContractStorageSupply is a token's supply summed from the balance LEDGER
// ENTRIES its contract holds in Soroban contract storage, rather than from the
// SEP-41 / CAP-67 event log.
//
// It answers a DIFFERENT QUESTION from the event-derived reading, and the two
// must never be added together:
//
//   - Event-derived supply (Σmint − Σburn − Σclawback over stellar.supply_flows,
//     [TokenSupply]) is an ISSUANCE accumulation. It is correct only if the log
//     is complete from the token's first ledger; a token that emitted no events
//     for part of its life accumulates to a figure that is too low, and one that
//     emitted none at all accumulates to a confident ZERO.
//   - Storage-derived supply (this type) is a DISTRIBUTION level: the sum of the
//     balances that exist right now. It carries no history and therefore cannot
//     be made wrong by a gap in the history.
//
// Because it is a level and not an accumulation, it SUPERSEDES the event
// reading for a token the event log cannot see — it does not supplement it.
// See [ContractStorageSupplyReader.ContractStorageSupply] for the handover rule
// and the conditions under which this reading is refused.
type ContractStorageSupply struct {
	ContractID string

	// Total is Σ of every `Balance(Address) → i128` entry the contract holds,
	// in the token's smallest unit (raw, undecimalised — ADR-0003: an i128 is
	// never handed to a JSON number).
	Total *big.Int

	// Decimals is the scale read from the contract's OWN instance storage, and
	// DecimalsFound reports whether the chain actually declared one. A caller
	// must not publish a decimalised figure when DecimalsFound is false: the
	// exponent would be invented, and an invented exponent is a published money
	// figure wrong by a power of ten.
	Decimals      uint32
	DecimalsFound bool

	// BalanceEntries is how many balance entries were summed. Zero with a nil
	// error means the contract holds no balances in storage — which is the
	// normal reading for a Stellar Asset Contract's classic asset, whose
	// balances live in trustlines.
	BalanceEntries int

	// DeclaredTotal is the contract's OWN self-reported total supply, when its
	// instance storage publishes one under `TotalSupply`. nil when it does not.
	//
	// This is an INDEPENDENT second measurement of the same quantity: the
	// contract's internal running total versus our sum of its individual
	// balance entries. They are maintained by different code paths inside the
	// contract, so agreement is meaningful evidence that we decoded every entry
	// — see [ContractStorageSupply.SelfConsistent].
	DeclaredTotal *big.Int

	// DeclaredHolders is the contract's OWN self-reported holder count, when its
	// instance storage publishes one under `HolderCount`. nil when it does not.
	//
	// Compared against BalanceEntries it is the completeness check that matters
	// most for this basis: state expiry makes an archived balance entry
	// invisible to us, and a contract that says it has twelve holders while we
	// can see ten is telling us we are missing two.
	DeclaredHolders *uint32

	// AsOfLedger is the highest ledger any summed entry was last written at.
	AsOfLedger uint32

	// isSAC records that the instance entry named a Stellar Asset Contract.
	// Unexported so the refusal can only leave this package as an error —
	// a caller must not be able to read the field and serve the sum anyway.
	isSAC bool
}

// SelfConsistent reports whether every cross-check the contract itself offered
// agreed with what we decoded.
//
// It is TRUE when a contract offered no cross-checks at all — absence of
// contradiction, which is all a bare token can give us. Callers that need to
// distinguish "checked and agreed" from "nothing to check" read DeclaredTotal /
// DeclaredHolders directly.
func (s ContractStorageSupply) SelfConsistent() bool {
	if s.DeclaredTotal != nil && s.Total != nil && s.DeclaredTotal.Cmp(s.Total) != 0 {
		return false
	}
	if s.DeclaredHolders != nil && int(*s.DeclaredHolders) != s.BalanceEntries {
		return false
	}
	return true
}

// ErrStorageSupplyIsStellarAsset is returned for a Stellar Asset Contract.
//
// It is a REFUSAL, not a failure. A SAC's contract storage holds only the slice
// of a classic asset that has been wrapped into Soroban; the rest of the asset
// sits in trustlines, claimable balances and liquidity-pool reserves that this
// reader cannot see and is not looking at. Summing a SAC's storage balances and
// calling the result the token's supply would understate it by however much of
// the asset never entered Soroban — measured on pubnet 2026-09-15 against KALE
// (CB23WRDQ…), storage held 471,938,508,419,832 against an event-derived
// 3,224,226,487,856,012, a 6.8x understatement that carries no internal sign of
// being wrong.
//
// The classic asset's own supply is served by the classic path, which is built
// for exactly this and knows about all four holding domains.
var ErrStorageSupplyIsStellarAsset = fmt.Errorf("clickhouse: contract is a Stellar Asset Contract; its storage holds only the wrapped slice of a classic asset")

// ErrStorageSupplyTooManyEntries is returned when a contract holds more balance
// entries than [maxContractStorageBalanceEntries].
//
// Also a refusal rather than a failure: this read decodes every entry in the Go
// process, so an unbounded holder set would be an unbounded response. Refusing
// is the honest outcome — a truncated sum is a WRONG supply figure that looks
// exactly like a right one, which is the failure mode this whole reader exists
// to remove.
var ErrStorageSupplyTooManyEntries = fmt.Errorf("clickhouse: contract holds more storage balance entries than this reader will decode")

// maxContractStorageBalanceEntries bounds one storage-supply read.
//
// The bound protects the API path, which pays a full decode per entry inside a
// per-request budget. 25,000 sits far above the population this basis actually
// serves — the tokens the event log cannot see are small, closed holder sets
// (the twenty-four private-credit deal tokens measured on pubnet 2026-09-15 ran
// 1 to 12 holders each) — and far below the largest storage-balance holder sets
// on pubnet (~62k), which are ordinary event-emitting tokens that never reach
// this path because their flow-derived reading answers first.
const maxContractStorageBalanceEntries = 25_000

// balanceKeyMarker is the fixed base64 window a `Balance(Address)` contract-data
// LedgerKey always carries, and instanceKeyMarker is the contract-instance one.
//
// A contract-data key's first 40 bytes are (LedgerEntryType, ScAddress type,
// 32-byte contract id) — a whole number of base64 groups at byte 39 — so every
// byte of the SCVal key that follows lands at a FIXED base64 offset for every
// contract. That makes both markers plain string comparisons on the stored
// column, with no base64Decode and no per-row XDR parse, which is what keeps
// the server-side filter cheap enough to run beside a primary-key range.
//
// The markers are a PRE-FILTER ONLY. Every row that survives them is still
// decoded and re-checked in Go by [balanceKeyHolder]: the marker proves the
// key's shape, not that the value is a balance. Crucially `BalanceCheckpoints`
// — a real key on these contracts, holding a VECTOR of historical balances —
// shares the leading symbol text `Balance` and would be swept in by any prefix
// match; it differs here only in the symbol LENGTH bytes the marker pins.
const (
	balanceKeyMarker = "ABAAAAABAAAAAgAAAA8AAAAHQmFsYW5jZQAAAAAS"
	// holderCountKeyMarker matches `Vec[Symbol("HolderCount")]`, a TOP-LEVEL
	// persistent entry rather than an instance-storage field. It is fetched
	// because it is the completeness check this basis most needs: the contract
	// stating how many holders it believes it has, against how many balance
	// entries the lake can actually show us.
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

// contractStorageSupplyQuery reads one contract's balance + instance entries.
//
// `entry_type` then `key_xdr` lead ledger_entries_current's ORDER BY, so the
// startsWith() is a primary-key prefix range over exactly one contract's
// entries rather than a scan — the same lookup shape [ExplorerReader.TokenDecimals]
// uses, and the reason this read is cheap enough to sit behind an API request.
//
// FINAL is mandatory: ledger_entries_current is a ReplacingMergeTree keyed by
// (entry_type, key_xdr), so without it a balance that has been updated returns
// once per unmerged part and the sum silently counts stale copies of the same
// holder.
//
// `change_type != 'removed'` drops entries the ledger has deleted. A removed
// balance is not a zero balance we may add; it is an entry that no longer
// exists, and eight of them were present on pubnet 2026-09-15.
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

// ContractStorageSupply sums a token's supply out of its Soroban contract
// storage, for tokens the SEP-41 / CAP-67 event log cannot see.
//
// # Why this exists
//
// The supply pipeline derives circulating supply from events, and that is
// complete for every token that emits them — including classic assets, since
// CAP-67 makes an issuer payment emit a mint. It is blind to a token that emits
// NONE. Such tokens are not undercounted in stellar.supply_flows, they are
// ABSENT from it, and an absent contract sums to a confident zero.
//
// # The handover rule
//
// A token whose event log starts partway through its life has BOTH a storage
// level and an event accumulation, and they must NOT be added — the same tokens
// are in both. The storage reading wins outright, and the reason is structural
// rather than a preference: a level measures what exists now and cannot be made
// wrong by missing history, while an accumulation is only ever as complete as
// its log. Events remain useful against this basis as a CROSS-CHECK, never as
// an addend.
//
// That the two agree when the log IS complete was measured, not assumed. On
// pubnet 2026-09-15, summing storage for three event-emitting Wasm tokens
// reproduced their event-derived totals exactly, to the unit:
//
//	EUTBL    (CBGV2QFQ…)  28,327,867,109,034
//	USTBL    (CARUUX2F…)   3,621,637,634,835
//	deJTRSY  (CBI7UCH5…)   8,763,619,974,700,234,898,508,352
//
// # What this reading is blind to
//
// It sees only balances that exist as ledger entries NOW. Soroban state expiry
// archives contract-data entries, and an archived balance is invisible here
// while remaining real and restorable. The figure is therefore a LOWER BOUND,
// and a different kind of lower bound from the classic trustline sum: that one
// is blind to whole holding DOMAINS, this one is blind to TIME. Callers must
// carry the bound onto the wire.
//
// # Refusals
//
// Returns [ErrStorageSupplyIsStellarAsset] for a SAC and
// [ErrStorageSupplyTooManyEntries] above the entry cap. A contract that simply
// holds no balances returns a zero Total with BalanceEntries == 0 and no error;
// the caller distinguishes that from a refusal and must not publish it as a
// supply.
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
	seen := 0
	for rows.Next() {
		var keyB64, entryB64 string
		var ledger uint32
		if err := rows.Scan(&keyB64, &entryB64, &ledger); err != nil {
			return ContractStorageSupply{}, fmt.Errorf("clickhouse: scan contract storage supply row: %w", err)
		}
		seen++
		if seen > maxContractStorageBalanceEntries {
			return ContractStorageSupply{}, fmt.Errorf("%w: %s (>%d)",
				ErrStorageSupplyTooManyEntries, contractID, maxContractStorageBalanceEntries)
		}
		if err := out.apply(keyB64, entryB64, ledger); err != nil {
			return ContractStorageSupply{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return ContractStorageSupply{}, fmt.Errorf("clickhouse: contract storage supply %s: %w", contractID, err)
	}
	if out.isSAC {
		return ContractStorageSupply{}, fmt.Errorf("%w: %s", ErrStorageSupplyIsStellarAsset, contractID)
	}
	return out, nil
}

// apply folds one decoded row into the running result.
func (s *ContractStorageSupply) apply(keyB64, entryB64 string, ledger uint32) error {
	var key xdr.LedgerKey
	if xdr.SafeUnmarshalBase64(keyB64, &key) != nil {
		// A key the lake stored but we cannot parse is a decode gap, not an
		// empty holder: skipping it would silently drop a balance from the
		// sum. Refuse the whole reading instead.
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
		// The key says balance and the value is not a number we recognise.
		// Refuse rather than treat it as zero.
		return fmt.Errorf("clickhouse: contract storage supply %s: balance entry carries no decodable amount", s.ContractID)
	}
	if amount.Sign() < 0 {
		// A negative held balance is impossible under correct token accounting.
		// It means we decoded the wrong field, so the sum cannot be trusted.
		return fmt.Errorf("clickhouse: contract storage supply %s: negative balance entry %s", s.ContractID, amount)
	}
	s.Total.Add(s.Total, amount)
	s.BalanceEntries++
	if ledger > s.AsOfLedger {
		s.AsOfLedger = ledger
	}
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

// instanceStorageKeyName returns the name of an instance-storage entry, under
// either of the two encodings that are live on pubnet.
//
// A bare `Symbol` is what the soroban-token-sdk writes for METADATA. A
// SINGLE-ELEMENT VECTOR holding a symbol is what Rust's `#[contracttype]`
// derives for a fieldless enum variant, which is how every hand-written token
// that keys its instance storage off a `DataKey` enum spells the same thing —
// including all twenty-four private-credit deal tokens, whose scale and
// self-declared total sit under `Vec[Symbol("Config")]` and
// `Vec[Symbol("TotalSupply")]`.
//
// Reading only the bare symbol was a real defect, not a hypothetical one: it
// silently produced DecimalsFound=false and a nil cross-check for exactly the
// population this reader was built to serve, so the supply would have been
// summed correctly and then published with a guessed exponent and nothing to
// check it against.
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

// balanceKeyHolder reports whether a contract-data key is a per-holder balance
// key: the two-element vector `[Symbol("Balance"), Address]`.
//
// The exact-length, exact-arity check is the point. `BalanceCheckpoints(Address)`
// is a real key on the tokens this reader serves and holds a VECTOR of past
// balances; anything that matched the symbol by PREFIX would sum a token's
// balance history into its supply. Measured on pubnet 2026-09-15, the
// twenty-four private-credit deal tokens each carry one BalanceCheckpoints entry
// per holder, so a prefix match would have roughly doubled every figure.
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

// balanceAmount pulls the i128 out of a balance entry's value.
//
// Two shapes are accepted because both are live on pubnet: a bare i128 (the
// soroban-token-sdk's current `Balance(Address) → i128`, and what every one of
// the twenty-four private-credit deal tokens stores), and a map carrying an
// `amount` field alongside authorization flags (the SAC / older token-sdk
// `BalanceValue{amount, authorized, clawback}`).
//
// An i128 is reassembled from its signed high and unsigned low halves: reading
// Lo as signed would corrupt every balance whose low word has the top bit set,
// which is roughly half of all values.
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

// contractDataKeyPrefix returns the base64 prefix every contract-data key for
// one contract begins with.
//
// A contract-data LedgerKey opens with a 4-byte LedgerEntryType, a 4-byte
// ScAddress discriminant and the 32-byte contract id: 40 bytes before anything
// contract-specific. 39 of those are a whole number of base64 groups, so
// encoding the first 39 yields 52 characters that are a prefix of the full key's
// encoding no matter what follows — which is what lets the lookup ride
// ledger_entries_current's (entry_type, key_xdr) primary key.
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
