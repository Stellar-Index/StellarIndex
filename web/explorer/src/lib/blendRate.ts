// Blend reserve borrow-rate curve in exact integer maths, mirroring the
// pool contract's calc_accrual (internal/sources/blend/interest.go BorrowRate).
// All inputs are 7-decimal fixed-point.

const SCALAR_7 = 10_000_000n;
const UTIL_95 = 9_500_000n;
const UTIL_05 = 500_000n;

export interface RateModel {
  target: bigint;
  rBase: bigint;
  rOne: bigint;
  rTwo: bigint;
  rThree: bigint;
}

function ceilDiv(n: bigint, d: bigint): bigint {
  return (n + d - 1n) / d;
}
const mulCeil = (a: bigint, b: bigint) => ceilDiv(a * b, SCALAR_7);
const divCeil = (a: bigint, b: bigint) => ceilDiv(a * SCALAR_7, b);

/** Annual borrow rate (7 decimals) at `util`, scaled by the live `irMod`. */
export function borrowRate(m: RateModel, util: bigint, irMod: bigint): bigint {
  if (util <= m.target) {
    // A zero target only reaches here with util 0; the scalar is then 0.
    const scalar = m.target === 0n ? 0n : divCeil(util, m.target);
    return mulCeil(mulCeil(scalar, m.rOne) + m.rBase, irMod);
  }
  if (util <= UTIL_95) {
    const scalar = divCeil(util - m.target, UTIL_95 - m.target);
    const base = mulCeil(scalar, m.rTwo) + m.rOne + m.rBase;
    return mulCeil(base, irMod);
  }
  const scalar = divCeil(util - UTIL_95, UTIL_05);
  const extra = mulCeil(scalar, m.rThree);
  return extra + mulCeil(irMod, m.rTwo + m.rOne + m.rBase);
}

export interface CurvePoint {
  util: bigint;
  rate: bigint;
}

/** The curve sampled every `stepPct` percent from 0 to 100, plus the kinks. */
export function rateCurve(
  m: RateModel,
  irMod: bigint,
  stepPct = 1,
): CurvePoint[] {
  const utils = new Set<bigint>([m.target, UTIL_95]);
  for (let p = 0; p <= 100; p += stepPct) utils.add(BigInt(p) * 100_000n);
  return [...utils]
    .filter((u) => u >= 0n && u <= SCALAR_7)
    .sort((a, b) => (a < b ? -1 : a > b ? 1 : 0))
    .map((util) => ({ util, rate: borrowRate(m, util, irMod) }));
}
