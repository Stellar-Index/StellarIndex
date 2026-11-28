package clickhouse

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// lendingPoolAuctionsLimit caps the rows one pool-wide Auction read returns; a read that reaches it is Truncated.
const lendingPoolAuctionsLimit = 10_000

// Auction key field names: PoolDataKey::Auction(AuctionKey{auct_type, user}). A contracttype struct is an ScMap sorted by
// field name, so the wire shape does not depend on the struct's declaration order (V1 declares user first, V2 auct_type).
const (
	auctionFieldType = "auct_type"
	auctionFieldUser = "user"
)

// LendingAuctionUser is one live auction of a pool: its type (0 user liquidation, 1 bad debt, 2 interest) and the user it
// liquidates.
type LendingAuctionUser struct {
	AuctionType uint32
	User        string
}

// LendingPoolAuctions is the set of live Auction keys one pool holds in the lake.
type LendingPoolAuctions struct {
	Auctions []LendingAuctionUser
	// Unrecognised counts keys under the Auction prefix whose shape is not AuctionKey{auct_type, user}: left out, never
	// guessed at.
	Unrecognised int
	// Truncated is set when the read reached lendingPoolAuctionsLimit, so keys past it are missing.
	Truncated bool
}

// LiveAuctionUsers reads every current Auction(AuctionKey) key of one pool as a primary-key prefix range, like
// LendingPoolPositions. Only the key is read: an archived entry is still the pool's auction (restorable), so it is included.
func (r *ExplorerReader) LiveAuctionUsers(ctx context.Context, pool string) (LendingPoolAuctions, error) {
	poolID, err := contractIDFromStrkey(pool)
	if err != nil {
		return LendingPoolAuctions{}, fmt.Errorf("clickhouse: lending pool id %q: %w", pool, err)
	}
	prefix, err := auctionKeyPrefix(poolID)
	if err != nil {
		return LendingPoolAuctions{}, err
	}
	ce, n, err := r.scanContractData(ctx, poolPositionsQuery, prefix, lendingPoolAuctionsLimit+1)
	if err != nil {
		return LendingPoolAuctions{}, err
	}
	out := assemblePoolAuctions(ce)
	out.Truncated = n > lendingPoolAuctionsLimit
	return out, nil
}

func assemblePoolAuctions(ce contractEntries) LendingPoolAuctions {
	out := LendingPoolAuctions{Auctions: []LendingAuctionUser{}}
	seen := map[string]bool{}
	add := func(k string) {
		if seen[k] {
			return
		}
		seen[k] = true
		if a, ok := auctionKeyUser(k); ok {
			out.Auctions = append(out.Auctions, a)
		} else {
			out.Unrecognised++
		}
	}
	for k := range ce.vals {
		add(k)
	}
	for k := range ce.archived {
		add(k)
	}
	sort.Slice(out.Auctions, func(i, j int) bool {
		if out.Auctions[i].User != out.Auctions[j].User {
			return out.Auctions[i].User < out.Auctions[j].User
		}
		return out.Auctions[i].AuctionType < out.Auctions[j].AuctionType
	})
	return out
}

// auctionKeyPrefix is the base64 prefix every Auction(key) LedgerKey of the pool shares, cut at a whole base64 quantum before
// the AuctionKey map; keys it also matches are dropped by auctionKeyUser.
func auctionKeyPrefix(poolID xdr.ContractId) (string, error) {
	arg := auctionKeyVal(0, contractAddressVal(xdr.ContractId{}))
	full, err := contractDataKeyXDR(poolID, "Auction", arg)
	if err != nil {
		return "", fmt.Errorf("clickhouse: auction key: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(full)
	if err != nil {
		return "", fmt.Errorf("clickhouse: auction key: %w", err)
	}
	argRaw, err := arg.MarshalBinary()
	if err != nil {
		return "", fmt.Errorf("clickhouse: auction key arg: %w", err)
	}
	n := len(raw) - len(argRaw) - 4 // the trailing 4 bytes are the durability
	n -= n % 3
	return base64.StdEncoding.EncodeToString(raw[:n]), nil
}

func auctionKeyVal(auctType uint32, user xdr.ScVal) xdr.ScVal {
	t, typeSym, userSym := xdr.Uint32(auctType), xdr.ScSymbol(auctionFieldType), xdr.ScSymbol(auctionFieldUser)
	m := xdr.ScMap{
		{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &typeSym}, Val: xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &t}},
		{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &userSym}, Val: user},
	}
	mp := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}
}

// auctionKeyUser is the auction an Auction(AuctionKey) LedgerKey names; false for any other key, or a key whose AuctionKey
// is not exactly {auct_type: u32, user: Address}.
func auctionKeyUser(keyB64 string) (LendingAuctionUser, bool) {
	var lk xdr.LedgerKey
	if xdr.SafeUnmarshalBase64(keyB64, &lk) != nil || lk.ContractData == nil {
		return LendingAuctionUser{}, false
	}
	vec, err := scval.AsVec(lk.ContractData.Key)
	if err != nil || len(vec) != 2 {
		return LendingAuctionUser{}, false
	}
	if sym, ok := vec[0].GetSym(); !ok || string(sym) != "Auction" {
		return LendingAuctionUser{}, false
	}
	fields, err := scval.AsMap(vec[1])
	if err != nil || len(fields) != 2 {
		return LendingAuctionUser{}, false
	}
	typeVal, okT := scval.MapField(fields, auctionFieldType)
	userVal, okU := scval.MapField(fields, auctionFieldUser)
	if !okT || !okU {
		return LendingAuctionUser{}, false
	}
	auctType, err := scval.AsU32(typeVal)
	if err != nil {
		return LendingAuctionUser{}, false
	}
	user, err := scval.AsAddressStrkey(userVal)
	if err != nil {
		return LendingAuctionUser{}, false
	}
	return LendingAuctionUser{AuctionType: auctType, User: user}, true
}
