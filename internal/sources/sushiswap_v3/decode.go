package sushiswap_v3

import (
	"fmt"
	"math/big"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// SwapFields is the decoded body of a pool `swap` event.
//
// Amount0 / Amount1 are SIGNED deltas from the POOL's point of view, which
// is what makes the direction derivable from the body alone: a positive
// delta is a token the pool RECEIVED (the trader sold it) and a negative
// delta a token the pool PAID OUT (the trader bought it). Exactly one of
// each is what a price-forming swap looks like.
type SwapFields struct {
	Amount0      canonical.Amount // i128 — signed pool delta of token0
	Amount1      canonical.Amount // i128 — signed pool delta of token1
	Liquidity    canonical.Amount // u128 — in-range liquidity after the swap
	SqrtPriceX96 canonical.Amount // u256 — post-swap sqrt price, Q64.96
	Tick         int32            // i32  — post-swap tick
	Sender       string           // Address — the caller that drove the swap
	Recipient    string           // Address — who received the output leg
}

// PoolCreatedFields is the decoded body of a factory `pool_created` event —
// the only on-chain statement of a pool's token identities.
type PoolCreatedFields struct {
	Pool        string // Address — the new pool contract
	Token0      canonical.Asset
	Token1      canonical.Asset
	FeePips     uint32
	TickSpacing int32
}

// ─── Real SCVal decoders ────────────────────────────────────────
// Tests swap these via the package-level vars.

var (
	decodeSwapFields  = sdkDecodeSwapFields
	decodePoolCreated = sdkDecodePoolCreated
)

// sdkDecodeSwapFields decodes a pool `swap` body. Every field is pulled by
// NAME from the top-level Map, never by position: the pools have already
// been through two WASM upgrades (ledgers 61,594,973 and 62,898,378) and a
// positional decode would break silently the next time one lands a field
// in a different slot (docs/architecture/contract-schema-evolution.md).
// Both deployed versions emit the same seven names, so one decoder covers
// the whole history.
func sdkDecodeSwapFields(valueB64 string) (SwapFields, error) {
	body, err := scval.Parse(valueB64)
	if err != nil {
		return SwapFields{}, fmt.Errorf("parse body: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return SwapFields{}, fmt.Errorf("body not a Map: %w", err)
	}

	var out SwapFields
	for _, field := range []struct {
		name string
		dst  *canonical.Amount
	}{
		{"amount0", &out.Amount0},
		{"amount1", &out.Amount1},
	} {
		sv, err := scval.MustMapField(entries, field.name)
		if err != nil {
			return SwapFields{}, fmt.Errorf("SwapEvent.%s: %w", field.name, err)
		}
		amt, err := scval.AsAmountFromI128(sv)
		if err != nil {
			return SwapFields{}, fmt.Errorf("SwapEvent.%s: %w", field.name, err)
		}
		*field.dst = amt
	}

	liqSv, err := scval.MustMapField(entries, "liquidity")
	if err != nil {
		return SwapFields{}, fmt.Errorf("SwapEvent.liquidity: %w", err)
	}
	if out.Liquidity, err = scval.AsAmountFromU128(liqSv); err != nil {
		return SwapFields{}, fmt.Errorf("SwapEvent.liquidity: %w", err)
	}

	// sqrt_price_x96 is a U256 (a Q64.96 fixed-point square root of the
	// price), not an i128 — it routinely exceeds 2^96 and must keep full
	// width. Carried, never floated.
	sqrtSv, err := scval.MustMapField(entries, "sqrt_price_x96")
	if err != nil {
		return SwapFields{}, fmt.Errorf("SwapEvent.sqrt_price_x96: %w", err)
	}
	if out.SqrtPriceX96, err = scval.AsAmountFromU256(sqrtSv); err != nil {
		return SwapFields{}, fmt.Errorf("SwapEvent.sqrt_price_x96: %w", err)
	}

	tickSv, err := scval.MustMapField(entries, "tick")
	if err != nil {
		return SwapFields{}, fmt.Errorf("SwapEvent.tick: %w", err)
	}
	if out.Tick, err = scval.AsI32(tickSv); err != nil {
		return SwapFields{}, fmt.Errorf("SwapEvent.tick: %w", err)
	}

	for _, field := range []struct {
		name string
		dst  *string
	}{
		{"sender", &out.Sender},
		{"recipient", &out.Recipient},
	} {
		sv, err := scval.MustMapField(entries, field.name)
		if err != nil {
			return SwapFields{}, fmt.Errorf("SwapEvent.%s: %w", field.name, err)
		}
		addr, err := scval.AsAddressStrkey(sv)
		if err != nil {
			return SwapFields{}, fmt.Errorf("SwapEvent.%s: %w", field.name, err)
		}
		*field.dst = addr
	}

	return out, nil
}

// sdkDecodePoolCreated decodes a factory `pool_created` body. Same
// decode-by-name path as the swap body. The `sender` field is present on
// the wire but not surfaced: on every event in history it is the factory
// itself, so it names no counterparty.
func sdkDecodePoolCreated(valueB64 string) (PoolCreatedFields, error) {
	body, err := scval.Parse(valueB64)
	if err != nil {
		return PoolCreatedFields{}, fmt.Errorf("parse body: %w", err)
	}
	entries, err := scval.AsMap(body)
	if err != nil {
		return PoolCreatedFields{}, fmt.Errorf("body not a Map: %w", err)
	}

	addrField := func(name string) (string, error) {
		sv, err := scval.MustMapField(entries, name)
		if err != nil {
			return "", err
		}
		return scval.AsAddressStrkey(sv)
	}

	var out PoolCreatedFields
	if out.Pool, err = addrField("pool_address"); err != nil {
		return PoolCreatedFields{}, fmt.Errorf("PoolCreatedEvent.pool_address: %w", err)
	}
	for _, field := range []struct {
		name string
		dst  *canonical.Asset
	}{
		{"token0", &out.Token0},
		{"token1", &out.Token1},
	} {
		strkey, err := addrField(field.name)
		if err != nil {
			return PoolCreatedFields{}, fmt.Errorf("PoolCreatedEvent.%s: %w", field.name, err)
		}
		// A pool token is a contract, always — a Soroban token or a
		// classic asset's Stellar Asset Contract. Asset identity is the
		// contract id; a bare code is never an asset here.
		asset, err := canonical.NewSorobanAsset(strkey)
		if err != nil {
			return PoolCreatedFields{}, fmt.Errorf("PoolCreatedEvent.%s %s: %w", field.name, strkey, err)
		}
		*field.dst = asset
	}

	feeSv, err := scval.MustMapField(entries, "fee")
	if err != nil {
		return PoolCreatedFields{}, fmt.Errorf("PoolCreatedEvent.fee: %w", err)
	}
	if out.FeePips, err = scval.AsU32(feeSv); err != nil {
		return PoolCreatedFields{}, fmt.Errorf("PoolCreatedEvent.fee: %w", err)
	}

	spacingSv, err := scval.MustMapField(entries, "tick_spacing")
	if err != nil {
		return PoolCreatedFields{}, fmt.Errorf("PoolCreatedEvent.tick_spacing: %w", err)
	}
	if out.TickSpacing, err = scval.AsI32(spacingSv); err != nil {
		return PoolCreatedFields{}, fmt.Errorf("PoolCreatedEvent.tick_spacing: %w", err)
	}

	return out, nil
}

// negate returns -a as a new Amount. canonical.Amount wraps a *big.Int and
// callers must never mutate the underlying value, so this allocates.
func negate(a canonical.Amount) canonical.Amount {
	return canonical.NewAmount(new(big.Int).Neg(a.BigInt()))
}

// decodeSwap turns one decoded swap body into a canonical.Trade.
//
// Direction comes from the two signed pool deltas and nothing else:
//
//	amount0 > 0 && amount1 < 0 → the trader SOLD token0 and BOUGHT token1
//	amount1 > 0 && amount0 < 0 → the trader SOLD token1 and BOUGHT token0
//
// so the base leg is the positive (pool-received) delta and the quote leg
// is the magnitude of the negative (pool-paid) delta — the same
// base=sold / quote=bought orientation every other AMM source in this
// repo records, and the same one canonical.Trade.PriceRatio assumes.
// Amounts stay exact i128 magnitudes in each token's own smallest unit;
// no decimals are applied and no price is computed here.
//
// Any other sign combination is refused as [ErrNonDirectionalSwap]. That
// includes a zero on either side: a swap that took one unit in and paid
// nothing out has no price, and reporting it as a trade would feed a
// fabricated ratio into VWAP and OHLC.
func decodeSwap(
	fields SwapFields,
	ledger uint32,
	txHash string,
	opIndex, eventIndex int,
	closedAt time.Time,
	tok0, tok1 canonical.Asset,
) (canonical.Trade, error) {
	var base, quote canonical.Asset
	var baseAmt, quoteAmt canonical.Amount

	switch s0, s1 := fields.Amount0.Sign(), fields.Amount1.Sign(); {
	case s0 > 0 && s1 < 0:
		base, baseAmt = tok0, fields.Amount0
		quote, quoteAmt = tok1, negate(fields.Amount1)
	case s1 > 0 && s0 < 0:
		base, baseAmt = tok1, fields.Amount1
		quote, quoteAmt = tok0, negate(fields.Amount0)
	default:
		return canonical.Trade{}, fmt.Errorf("%w: amount0=%s amount1=%s",
			ErrNonDirectionalSwap, fields.Amount0, fields.Amount1)
	}

	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		return canonical.Trade{}, fmt.Errorf("pair: %w", err)
	}

	return canonical.Trade{
		Source: SourceName,
		Ledger: ledger,
		TxHash: txHash,
		// Fan out by the swap event's own index. A router multi-hop and a
		// split route both invoke SEVERAL pools inside ONE operation, so
		// every one of those swaps shares op_index and would collide on the
		// trades primary key, leaving all but one silently dropped by the
		// writer's ON CONFLICT. The lake proves the shape is real here: the
		// swaps in tx f6fb00ef…c308 land at event indices 2 and 14 of the
		// same operation.
		OpIndex:     canonical.FanoutOpIndex(opIndex, eventIndex),
		Timestamp:   closedAt,
		Pair:        pair,
		BaseAmount:  baseAmt,
		QuoteAmount: quoteAmt,
		// Taker is the account the output leg was paid to. Maker is left
		// empty: an AMM pool has no resting counterparty, which is the same
		// contract soroswap / aquarius / phoenix / comet record and why the
		// source belongs in timescale.AMMSignerSources.
		Taker: fields.Recipient,
	}, nil
}
