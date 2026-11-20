import { FactList, Fact } from '@/components/ui/Facts';
import { rateCurve, type CurvePoint, type RateModel } from '@/lib/blendRate';
import { formatReadable } from '@/lib/format';
import { fixedDecimal, fixedPct, supplyCapText } from '../poolConfig';
import { ReserveHistory } from './ReserveHistory';
import type { ReserveRow } from './usePoolReserves';

const RATE_DECIMALS = 7;

function big(raw: string | null | undefined): bigint | null {
  return raw != null && /^\d+$/.test(raw) ? BigInt(raw) : null;
}

/** The reserve's rate model as exact integers, or null when any part is missing. */
export function rateModelOf(rv: ReserveRow): RateModel | null {
  const c = rv.config;
  if (!c) return null;
  const parts = [
    big(c.target_utilization),
    big(c.r_base),
    big(c.r_one),
    big(c.r_two),
    big(c.r_three),
  ];
  if (parts.some((p) => p == null)) return null;
  const [target, rBase, rOne, rTwo, rThree] = parts as bigint[];
  return { target, rBase, rOne, rTwo, rThree };
}

/** The curve's rate at a utilisation (1e7 scale), linear between samples; plot only. */
export function curveRateAt(curve: CurvePoint[], util: number): number | null {
  for (let i = 1; i < curve.length; i++) {
    const u0 = Number(curve[i - 1].util);
    const u1 = Number(curve[i].util);
    if (util >= u0 && util <= u1) {
      const r0 = Number(curve[i - 1].rate);
      const r1 = Number(curve[i].rate);
      return u1 === u0 ? r0 : r0 + ((util - u0) / (u1 - u0)) * (r1 - r0);
    }
  }
  return null;
}

const W = 320;
const H = 140;
const PAD = { l: 40, r: 8, t: 8, b: 22 };

function RateChart({
  curve,
  currentUtil,
}: {
  curve: CurvePoint[];
  currentUtil: number | null;
}) {
  const maxRate = Number(curve.reduce((m, p) => (p.rate > m ? p.rate : m), 1n));
  const x = (u: number) => PAD.l + (u / 1e7) * (W - PAD.l - PAD.r);
  const y = (r: number) => H - PAD.b - (r / maxRate) * (H - PAD.t - PAD.b);
  const pts = curve
    .map(
      (p) => `${x(Number(p.util)).toFixed(1)},${y(Number(p.rate)).toFixed(1)}`,
    )
    .join(' ');
  const top = fixedPct(String(maxRate), RATE_DECIMALS);
  const now = currentUtil == null ? null : curveRateAt(curve, currentUtil);
  return (
    <svg
      viewBox={`0 0 ${W} ${H}`}
      className="h-auto w-full max-w-md"
      role="img"
      aria-label={`Borrow APR from 0% to 100% utilisation, rising to ${top}${
        currentUtil != null
          ? `; current utilisation ${(currentUtil / 1e5).toFixed(1)}%`
          : ''
      }`}
    >
      <line
        x1={PAD.l}
        y1={H - PAD.b}
        x2={W - PAD.r}
        y2={H - PAD.b}
        stroke="var(--color-line-strong)"
      />
      <line
        x1={PAD.l}
        y1={PAD.t}
        x2={PAD.l}
        y2={H - PAD.b}
        stroke="var(--color-line-strong)"
      />
      <polyline
        points={pts}
        fill="none"
        stroke="var(--color-brand-500)"
        strokeWidth={1.5}
      />
      {currentUtil != null && (
        <line
          x1={x(currentUtil)}
          y1={PAD.t}
          x2={x(currentUtil)}
          y2={H - PAD.b}
          stroke="var(--color-ink-muted)"
          strokeDasharray="3 3"
        />
      )}
      {now != null && currentUtil != null && (
        <circle
          data-testid="curve-now"
          cx={x(currentUtil)}
          cy={y(now)}
          r={4}
          fill="var(--color-brand-500)"
          stroke="var(--color-surface)"
          strokeWidth={2}
        >
          <title>{`Now: ${(currentUtil / 1e5).toFixed(1)}% utilisation, borrow APR ${fixedPct(String(Math.round(now)), RATE_DECIMALS)}`}</title>
        </circle>
      )}
      <g fill="var(--color-ink-muted)" fontSize={10}>
        <text x={PAD.l - 4} y={PAD.t + 8} textAnchor="end">
          {top}
        </text>
        <text x={PAD.l - 4} y={H - PAD.b} textAnchor="end">
          0%
        </text>
        <text x={PAD.l} y={H - 6}>
          0%
        </text>
        <text x={W - PAD.r} y={H - 6} textAnchor="end">
          100% utilisation
        </text>
      </g>
    </svg>
  );
}

export function ReserveDetail({
  reserve: rv,
  pool,
  configDecimals,
}: {
  reserve: ReserveRow;
  pool: string;
  configDecimals: number | null | undefined;
}) {
  const c = rv.config;
  const model = rateModelOf(rv);
  const irMod = big(rv.ir_mod);
  // The curve port is the 7-decimal (V2 / BLNT) model; a 9-decimal modifier has none.
  const curveOk =
    model != null && irMod != null && rv.ir_mod_decimals === RATE_DECIMALS;
  const curve = curveOk ? rateCurve(model, irMod) : null;
  const currentUtil = Number.isFinite(rv.utilization_pct)
    ? Math.round(Math.min(100, Math.max(0, rv.utilization_pct)) * 1e5)
    : null;
  const pctOf = (raw: string | undefined) => (
    <span title={raw}>{fixedPct(raw, configDecimals)}</span>
  );

  return (
    <div className="grid gap-6 py-2 md:grid-cols-2">
      <div className="space-y-4">
        <h3 className="text-ink text-sm font-medium">Risk parameters</h3>
        <FactList columns={2}>
          <Fact label="Collateral factor">{pctOf(c?.collateral_factor)}</Fact>
          <Fact label="Liability factor">{pctOf(c?.liability_factor)}</Fact>
          <Fact label="Price (USD)" title={rv.price_usd ?? undefined}>
            {formatReadable(rv.price_usd, true) ?? '—'}
            {rv.price_usd != null && rv.price_stale ? ' (stale)' : ''}
          </Fact>
          <Fact label="Supply cap" title={c?.supply_cap ?? undefined}>
            {supplyCapText(c?.supply_cap, rv.decimals ?? c?.decimals)}
          </Fact>
        </FactList>
        <h3 className="text-ink text-sm font-medium">Interest-rate model</h3>
        <FactList columns={2}>
          <Fact label="Target utilisation">{pctOf(c?.target_utilization)}</Fact>
          <Fact label="Max utilisation">{pctOf(c?.max_utilization)}</Fact>
          <Fact label="Reactivity" title={c?.reactivity}>
            {fixedDecimal(c?.reactivity, configDecimals)}
          </Fact>
          <Fact label="Rate modifier" title={rv.ir_mod ?? undefined}>
            {fixedDecimal(rv.ir_mod, rv.ir_mod_decimals)}
          </Fact>
          <Fact label="Base rate">{pctOf(c?.r_base)}</Fact>
          <Fact label="Slope below target">{pctOf(c?.r_one)}</Fact>
          <Fact label="Slope target to 95%">{pctOf(c?.r_two)}</Fact>
          <Fact label="Slope above 95%">{pctOf(c?.r_three)}</Fact>
        </FactList>
      </div>
      <div className="space-y-2">
        <h3 className="text-ink text-sm font-medium">
          Borrow APR by utilisation
        </h3>
        {curve ? (
          <>
            <RateChart curve={curve} currentUtil={currentUtil} />
            <p className="text-ink-muted text-xs">
              Computed from the reserve&apos;s rate model and current modifier.
              The dashed line and dot mark current utilisation.
            </p>
          </>
        ) : (
          <p className="text-ink-muted text-sm">
            The rate curve is unavailable: this reserve&apos;s rate model is not
            captured or uses a different scale.
          </p>
        )}
      </div>
      <ReserveHistory pool={pool} asset={rv.asset} />
    </div>
  );
}
