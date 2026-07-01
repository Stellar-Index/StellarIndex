package supply

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// xlmAssetKey is the single canonical key for native XLM in
// asset_supply_history + on the API surface. Every XLM-related
// computation produces this constant, never "native" / "XLM:" /
// other variants.
const xlmAssetKey = "XLM"

// XLMTotalSupplyStroops is the frozen-2019 native XLM total supply
// in stroops: 50_001_806_812 XLM × 10^7 stroops/XLM. The figure
// comprises the 50 B genesis lumens plus the inflation pool that
// was frozen by network vote in October 2019. Per ADR-0011 it does
// not move; only circulating changes (when SDF reserve account
// balances change).
//
// Constructed once at package init via init(); exposed as a function
// rather than a `var` to prevent accidental mutation by callers.
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

// ReserveBalanceReader is the read-side interface the [XLMComputer]
// needs: given a list of G-strkey account addresses, return their
// summed XLM balance in stroops as observed at LedgerSequence.
//
// Production implementation: a Postgres-backed reader against the
// trustline-delta indexer's per-account running totals (the same
// hypertable Algorithm 2 will read for classic asset supply). The
// reader is responsible for its own caching; the computer makes one
// call per Compute() invocation.
//
// Returns the summed balance as a non-nil *big.Int (zero is a valid
// answer — the SDF reserves could be empty in a hypothetical
// future). Returns an error when the storage layer can't satisfy
// the request; the computer surfaces that error rather than
// fabricating a partial answer.
type ReserveBalanceReader interface {
	ReserveBalanceTotal(ctx context.Context, accounts []string, ledger uint32) (*big.Int, error)
}

// ReserveBalanceFreshnessReader is the optional extension to
// [ReserveBalanceReader] that reports the lowest observation
// ledger across the configured SDF reserve accounts at or
// before the snapshot ledger.
//
// F-1236 (codex audit-2026-05-12): closes the third leg of the
// supply-snapshot freshness gate (the classic + SEP41 legs were
// shipped in waves 17 + 18). Without an XLM freshness signal,
// the Refresher's stale-component gate stays permissive on
// every native-XLM snapshot — a backfilled-reserve observer
// that drifts hours behind tip would still produce snapshots
// stamped at the fresh ledger.
//
// Implementations:
//   - [LCMReserveBalanceReader] iterates the per-account
//     observation rows and returns MIN(row.Ledger) across all
//     non-removal accounts.
//   - [ConfigReserveBalanceReader] DELIBERATELY does NOT
//     implement this — the static config has no per-ledger
//     freshness concept, so the legacy permissive posture is
//     preserved when the static fallback fires.
//
// The XLM computer probes for this interface via a type
// assertion; if not satisfied, MinComponentLedger stays 0 and
// the Refresher's gate falls back to legacy-permissive — same
// shape as classic/SEP41 when their MinComponentLedger is 0.
//
// Zero return value means "no observation found for at least
// one account at-or-before `asOfLedger`" and is the gate's
// permissive bypass signal. A non-zero return is the actual
// MIN across observed accounts.
type ReserveBalanceFreshnessReader interface {
	MinReserveAccountLedger(ctx context.Context, accounts []string, asOfLedger uint32) (uint32, error)
}

// XLMComputer derives Algorithm 1 supply for native XLM. Wraps a
// configured reserve-account list (from [Policy.SDFReserveAccounts])
// + a [ReserveBalanceReader] that resolves balances on demand.
//
// Safe for concurrent Compute() calls — fields are read-only after
// construction; the underlying ReserveBalanceReader is required to
// be concurrent-safe by contract.
type XLMComputer struct {
	reserveAccounts []string
	reader          ReserveBalanceReader
}

// NewXLMComputer constructs an Algorithm 1 computer.
//
// reader MAY be nil when reserveAccounts is empty — Compute() then
// short-circuits the lookup. A nil reader with a non-empty
// reserveAccounts list is a configuration error and returns ErrNilReader.
func NewXLMComputer(reserveAccounts []string, reader ReserveBalanceReader) (*XLMComputer, error) {
	if len(reserveAccounts) > 0 && reader == nil {
		return nil, ErrNilReader
	}
	// Defensive copy so a caller mutating their input slice can't
	// silently change the configured reserve list.
	reserved := append([]string(nil), reserveAccounts...)
	return &XLMComputer{
		reserveAccounts: reserved,
		reader:          reader,
	}, nil
}

// ErrNilReader is returned by [NewXLMComputer] when the caller
// supplied reserve accounts but no reader. Operator misconfig that
// would silently produce an over-stated circulating supply (no
// exclusion applied) — fail loudly at construction instead.
var ErrNilReader = errors.New("supply: reserve-balance reader is nil but reserve accounts are configured")

// Compute returns the [Supply] for native XLM at the supplied
// ledger. Per Algorithm 1:
//
//   - total_supply = XLMTotalSupplyStroops (constant).
//   - max_supply = total_supply (XLM is hard-capped).
//   - circulating_supply = total_supply − Σ(SDF reserve balances).
//
// observedAt should be the close time of `ledger` in UTC; callers
// pass the ledger-meta timestamp directly. Compute does NOT consult
// wall-clock time.
//
// Returns the underlying error (wrapped) when the
// [ReserveBalanceReader] fails. The computer does NOT fall back to
// "publish total as circulating" — the partial answer would be
// indistinguishable on the wire from a healthy zero-reserve state,
// so we surface the error and let the caller decide whether to skip
// this snapshot or retry.
func (c *XLMComputer) Compute(ctx context.Context, ledger uint32, observedAt time.Time) (Supply, error) {
	total := XLMTotalSupplyStroops()

	reserved := big.NewInt(0)
	if len(c.reserveAccounts) > 0 {
		var err error
		reserved, err = c.reader.ReserveBalanceTotal(ctx, c.reserveAccounts, ledger)
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

	// CS-010: only claim an SDF-reserve exclusion when we actually
	// excluded reserves. With no reserve accounts configured,
	// circulating == total; labelling that "xlm_sdf_reserve_exclusion"
	// would tell every consumer an exclusion happened when it didn't,
	// overstating circulating supply + market cap. Emit an honest basis
	// instead so the misconfiguration is self-evident on the API.
	basis := BasisXLMSDFReserveExclusion
	if len(c.reserveAccounts) == 0 {
		basis = BasisXLMTotalOnly
	}

	// F-1236 (codex audit-2026-05-12): if the reader implements
	// [ReserveBalanceFreshnessReader], probe it for the
	// per-account observation freshness signal. A failure here
	// is non-fatal — the gate falls back to legacy permissive
	// (MinComponentLedger=0) the same way classic/SEP41 do on
	// transient freshness-query errors. The reader's WARN log
	// path is the operator-facing signal; we don't surface it
	// here to keep the supply hot path lean.
	var minLedger uint32
	if fr, ok := c.reader.(ReserveBalanceFreshnessReader); ok && len(c.reserveAccounts) > 0 {
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
