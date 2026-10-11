package supply

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// xlmAssetKey is the single canonical key for native XLM in
// asset_supply_history + on the API surface. Every XLM-related
// computation produces this constant, never "native" / "XLM:" /
// other variants.
const xlmAssetKey = "XLM"

// XLMTotalSupplyStroops is the frozen-2019 pubnet native XLM total in stroops: 50_001_806_812 XLM
// x 10^7 (50 B genesis plus the inflation pool frozen by the October 2019 vote; ADR-0011). It
// never moves; only circulating does. A function returning a copy so callers can't mutate it.
func XLMTotalSupplyStroops() *big.Int {
	return new(big.Int).Set(xlmTotalSupplyStroops)
}

// xlmTotalSupplyStroops is the underlying immutable value. Returned
// only via the [XLMTotalSupplyStroops] copy-constructor so callers
// can't mutate the package-level constant.
var xlmTotalSupplyStroops = new(big.Int).Mul(
	big.NewInt(50_001_806_812),
	big.NewInt(10_000_000), // 10^7 stroops per XLM
)

// xlmTestNetworkTotalSupplyStroops is the testnet/futurenet native total: 100 B genesis lumens,
// equal to the ledger-header total_coins. Reset test networks never ran inflation or the 2019
// vote, so the pubnet figure is wrong there.
var xlmTestNetworkTotalSupplyStroops = new(big.Int).Mul(
	big.NewInt(100_000_000_000),
	big.NewInt(10_000_000), // 10^7 stroops per XLM
)

// ErrUnknownNetwork is returned for a passphrase that is not pubnet/testnet/futurenet. Fail-closed:
// serving the pubnet constant for an unrecognised network is the defect this guards against.
var ErrUnknownNetwork = errors.New("supply: no native XLM total known for network passphrase")

// XLMTotalSupplyStroopsForNetwork returns the native XLM total in stroops for the passphrase:
// pubnet is the frozen-2019 figure, testnet and futurenet are 100 B genesis lumens (== ledger
// total_coins). Anything else is [ErrUnknownNetwork]. Always returns a fresh copy.
func XLMTotalSupplyStroopsForNetwork(passphrase string) (*big.Int, error) {
	switch passphrase {
	case canonical.PubnetPassphrase:
		return XLMTotalSupplyStroops(), nil
	case canonical.TestnetPassphrase, canonical.FuturenetPassphrase:
		return new(big.Int).Set(xlmTestNetworkTotalSupplyStroops), nil
	}
	return nil, fmt.Errorf("%w: %q", ErrUnknownNetwork, passphrase)
}

// ReserveBalanceReader is the [XLMComputer]'s read side: the summed XLM balance in stroops of
// the given G-strkey accounts as of a ledger, as a non-nil *big.Int (zero is valid). The reader
// owns its caching (one call per Compute). On storage error it returns an error; the computer
// surfaces it rather than fabricating a partial answer.
type ReserveBalanceReader interface {
	ReserveBalanceTotal(ctx context.Context, accounts []string, ledger uint32) (*big.Int, error)
}

// readReserveTotal reads the reserve total and which source answered;
// a reader without [ReserveBalanceSourcedReader] is live.
func readReserveTotal(ctx context.Context, r ReserveBalanceReader, accounts []string, ledger uint32) (*big.Int, ReserveSource, error) {
	if sr, ok := r.(ReserveBalanceSourcedReader); ok {
		return sr.ReserveBalanceTotalSourced(ctx, accounts, ledger)
	}
	v, err := r.ReserveBalanceTotal(ctx, accounts, ledger)
	return v, ReserveSourceLive, err
}

// ReserveBalanceFreshnessReader extends [ReserveBalanceReader] with a freshness anchor for the SDF
// reserve accounts (the XLM leg of the freshness gate). [ConfigReserveBalanceReader] DELIBERATELY does
// not implement it (strict mode refuses static config); zero means unobserved (permissive bypass).
type ReserveBalanceFreshnessReader interface {
	MinReserveAccountLedger(ctx context.Context, accounts []string, asOfLedger uint32) (uint32, error)
}

// XLMComputer derives Algorithm 1 supply for native XLM from [Policy.SDFReserveAccounts] and a
// [ReserveBalanceReader]. Safe for concurrent Compute calls: fields are read-only after
// construction and the reader must be concurrent-safe.
type XLMComputer struct {
	reserveAccounts []string
	reader          ReserveBalanceReader
	// total is the network's native XLM total supply in stroops
	// (see [XLMTotalSupplyStroopsForNetwork]); never mutated after
	// construction, copied on every Compute.
	total *big.Int
}

// NewXLMComputer builds an Algorithm 1 computer for PUBNET (frozen-2019 total); production should
// use [NewXLMComputerForNetwork] so test networks use their own total. reader MAY be nil only
// when reserveAccounts is empty; otherwise ErrNilReader.
func NewXLMComputer(reserveAccounts []string, reader ReserveBalanceReader) (*XLMComputer, error) {
	return NewXLMComputerForNetwork(canonical.PubnetPassphrase, reserveAccounts, reader)
}

// NewXLMComputerForNetwork builds a computer whose total/max supply is the native XLM total of
// the passphrase's network; an unrecognised passphrase is [ErrUnknownNetwork], never a pubnet
// fallback. Same reader contract as [NewXLMComputer].
func NewXLMComputerForNetwork(passphrase string, reserveAccounts []string, reader ReserveBalanceReader) (*XLMComputer, error) {
	total, err := XLMTotalSupplyStroopsForNetwork(passphrase)
	if err != nil {
		return nil, err
	}
	if len(reserveAccounts) > 0 && reader == nil {
		return nil, ErrNilReader
	}
	// Defensive copy so a caller mutating their input slice can't
	// silently change the configured reserve list.
	reserved := append([]string(nil), reserveAccounts...)
	return &XLMComputer{
		reserveAccounts: reserved,
		reader:          reader,
		total:           total,
	}, nil
}

// ErrNilReader is returned by [NewXLMComputer] when the caller
// supplied reserve accounts but no reader. Operator misconfig that
// would silently produce an over-stated circulating supply (no
// exclusion applied) — fail loudly at construction instead.
var ErrNilReader = errors.New("supply: reserve-balance reader is nil but reserve accounts are configured")

// Compute returns the native XLM [Supply] at ledger (Algorithm 1): total = the network's native
// total, max = total (hard-capped), circulating = total - sum(SDF reserve balances). observedAt is
// the ledger close time; wall-clock is not consulted. A reader failure is returned wrapped: falling
// back to "total as circulating" would be indistinguishable on the wire from a healthy zero-reserve state.
func (c *XLMComputer) Compute(ctx context.Context, ledger uint32, observedAt time.Time) (Supply, error) {
	total := new(big.Int).Set(c.total)

	reserved := big.NewInt(0)
	source := ReserveSourceLive
	if len(c.reserveAccounts) > 0 {
		var err error
		reserved, source, err = readReserveTotal(ctx, c.reader, c.reserveAccounts, ledger)
		if err != nil {
			return Supply{}, fmt.Errorf("supply: read SDF reserve balances at ledger %d: %w", ledger, err)
		}
		if reserved == nil {
			// Defence-in-depth: a misbehaving reader returning
			// (nil, nil) would otherwise nil-pointer in the Sub call
			// below. Treat as zero with a wrapped error so the
			// operator gets a clear signal.
			return Supply{}, fmt.Errorf("supply: reserve-balance reader returned nil at ledger %d", ledger)
		}
	}

	circulating := new(big.Int).Sub(total, reserved)
	// Clamp at zero like Algorithm 2 and 3: a reserve total above the hard-capped XLM total (operator
	// misconfig or reader bug) must not put a negative circulating supply, and the negative market
	// cap it implies, on the wire.
	if circulating.Sign() < 0 {
		circulating.SetInt64(0)
	}

	// Claim an SDF-reserve exclusion only when reserves were excluded: with none configured,
	// circulating == total, and the exclusion label would overstate circulating supply and market cap.
	basis := BasisXLMSDFReserveExclusion
	switch {
	case len(c.reserveAccounts) == 0:
		basis = BasisXLMTotalOnly
	case source == ReserveSourceStatic:
		basis = BasisXLMSDFReserveExclusionStatic
	}

	// Probe [ReserveBalanceFreshnessReader] if implemented; a failure is non-fatal and leaves the gate
	// permissive (MinComponentLedger=0), as for classic/SEP41. Never probed for a static-arm balance:
	// the observer watermark says nothing about a hand-entered map, and anchoring it would let strict
	// mode publish it.
	var minLedger uint32
	if fr, ok := c.reader.(ReserveBalanceFreshnessReader); ok && len(c.reserveAccounts) > 0 && source == ReserveSourceLive {
		if got, ferr := fr.MinReserveAccountLedger(ctx, c.reserveAccounts, ledger); ferr == nil {
			minLedger = got
		}
	}

	return Supply{
		AssetKey:           xlmAssetKey,
		TotalSupply:        total,
		CirculatingSupply:  circulating,
		MaxSupply:          new(big.Int).Set(total),
		Basis:              basis,
		LedgerSequence:     ledger,
		ObservedAt:         observedAt.UTC(),
		MinComponentLedger: minLedger,
	}, nil
}
