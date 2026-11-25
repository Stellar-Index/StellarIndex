package lendinginterest

import (
	"maps"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
)

// reserveHours groups a dataset's reserve versions by (pool, asset, hour), in read order.
func reserveHours(vs []Version) map[rowKey][]Version {
	out := map[rowKey][]Version{}
	for _, v := range vs {
		if v.Kind == KindReserve {
			k := rowKey{v.Pool, v.Asset, hourOf(v).Unix()}
			out[k] = append(out[k], v)
		}
	}
	return out
}

// On every hour of the mainnet golden slices, the telescoped chain equals the fold's gross, with the value in force
// before the hour as the predecessor (none for a baseline).
func TestTelescopedGross_GoldenHours(t *testing.T) {
	for _, d := range golden {
		t.Run(d.name, func(t *testing.T) {
			vs := d.versions(t)
			rows := fold(t, d.seed(), vs, d.opts(0))
			byHour := reserveHours(vs)
			checked := 0
			for _, r := range rows {
				var prev *blend.ReserveData
				for _, v := range vs {
					if v.Kind == KindReserve && v.Pool == r.Pool && v.Asset == r.Asset && hourOf(v).Before(r.Hour) {
						rd := v.Reserve
						prev = &rd
					}
				}
				got, err := TelescopedGross(prev, byHour[rowKey{r.Pool, r.Asset, r.Hour.Unix()}])
				if err != nil {
					t.Fatal(err)
				}
				eqStr(t, r.Asset+" "+r.Hour.Format(time.RFC3339)+" telescoped", got, r.Gross)
				checked++
			}
			if checked == 0 || checked != len(rows) {
				t.Fatalf("checked %d of %d rows", checked, len(rows))
			}
		})
	}
}

// On every golden hour the independent count equals the fold's Versions; a re-read duplicate and a removal add
// nothing, and a dropped version shows.
func TestReserveVersionCounts_GoldenHours(t *testing.T) {
	for _, d := range golden {
		vs := d.versions(t)
		rows := fold(t, d.seed(), vs, d.opts(0))
		gone := vs[len(vs)-1]
		gone.Removed, gone.ChangeIndex = true, gone.ChangeIndex+1
		counts := ReserveVersionCounts(append(append(append([]Version(nil), vs...), vs[0]), gone))
		for _, r := range rows {
			if got := counts[ReserveKey{r.Pool, r.Asset}][r.Hour.Unix()]; got != r.Versions {
				t.Errorf("%s %s %s: counted %d, the fold %d", d.name, r.Asset, r.Hour.Format(time.RFC3339), got, r.Versions)
			}
		}
		k := ReserveKey{vs[0].Pool, vs[0].Asset}
		if fewer := ReserveVersionCounts(vs[1:]); fewer[k][hourOf(vs[0]).Unix()] != counts[k][hourOf(vs[0]).Unix()]-1 {
			t.Errorf("%s: dropping a version did not lower its hour's count", d.name)
		}
	}
}

// A changed d_rate inside a run moves the telescoped total, so the check is not vacuous; order and duplicates do not.
func TestTelescopedGross_Sensitivity(t *testing.T) {
	vs := v2Repays.versions(t)
	base, err := TelescopedGross(nil, vs)
	if err != nil {
		t.Fatal(err)
	}
	rev := append([]Version(nil), vs...)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	if got, _ := TelescopedGross(nil, append(rev, vs[0])); got.Cmp(base) != 0 {
		t.Fatalf("reordered/duplicated input gives %s, want %s", got, base)
	}
	last := len(vs) - 1
	bumped := append([]Version(nil), vs...)
	bumped[last].Reserve = cloneReserve(bumped[last].Reserve)
	bumped[last].Reserve.DRate.Add(bumped[last].Reserve.DRate, big.NewInt(1_000_000_000))
	if got, _ := TelescopedGross(nil, bumped); got.Cmp(base) == 0 {
		t.Fatalf("a moved final d_rate left the telescoped total at %s", got)
	}
}

// A fee-only transaction (a claim) between two pairs of one hour moves the asset's fee entry outside the pairs: the
// row records it as its fee-only delta, and its pairs still match with no credit residual.
func TestFold_FeeOnlyTransaction(t *testing.T) {
	d := blntCACSQ
	vs := d.versions(t)
	byHour := reserveHours(vs)
	var at Version
	for _, hv := range byHour {
		// The first version of an hour holding two in different ledgers.
		if len(hv) >= 2 && hv[0].Ledger != hv[1].Ledger && (at.Pool == "" || hv[0].Ledger < at.Ledger) {
			at = hv[0]
		}
	}
	if at.Pool == "" {
		t.Fatal("degenerate fixture: no hour with two versions in different ledgers")
	}
	plain := fold(t, d.seed(), vs, d.opts(0))
	claim := big.NewInt(3)
	var latest blnt.ProtocolFees
	for i := range vs {
		if vs[i].Kind != KindFees || vs[i].Pool != at.Pool {
			continue
		}
		if vs[i].Ledger <= at.Ledger {
			latest = vs[i].Fees
			continue
		}
		f := maps.Clone(vs[i].Fees)
		e := f[at.Asset]
		f[at.Asset] = blnt.ProtocolFee{Credit: new(big.Int).Sub(e.Credit, claim), Carry: e.Carry}
		vs[i].Fees = f
	}
	claimed := maps.Clone(latest)
	e := claimed[at.Asset]
	claimed[at.Asset] = blnt.ProtocolFee{Credit: new(big.Int).Sub(e.Credit, claim), Carry: e.Carry}
	vs = append(vs, Version{
		Pool: at.Pool, Kind: KindFees, Ledger: at.Ledger, IntraLedgerSeq: 1_000, TxHash: "claim",
		CloseTime: at.CloseTime, Fees: claimed,
	})
	rows := fold(t, d.seed(), vs, d.opts(0))
	h := hourOf(at)
	r, p := findRow(t, rows, at.Pool, at.Asset, h), findRow(t, plain, at.Pool, at.Asset, h)
	if r.FeeOnlyVersions != 1 || r.FeeOnlyUnknown != 0 || r.FeeMismatches != 0 || r.FeeCreditResidual.Sign() != 0 {
		t.Fatalf("row: fee-only %d unknown %d mismatches %d credit residual %s, want 1, 0, 0, 0",
			r.FeeOnlyVersions, r.FeeOnlyUnknown, r.FeeMismatches, r.FeeCreditResidual)
	}
	eqStr(t, "fee-only credit", r.FeeOnlyCreditDelta, new(big.Int).Neg(claim))
	eqStr(t, "fee-only carry", r.FeeOnlyCarryDelta, new(big.Int))
	eqStr(t, "gross", r.Gross, p.Gross)
	eqStr(t, "protocol fee", r.ProtocolFee, p.ProtocolFee)
	for _, o := range rows {
		if o.Asset != at.Asset && o.FeeOnlyVersions != 0 {
			t.Errorf("asset %s hour %s: %d fee-only moves, the claim touched only %s", o.Asset, o.Hour, o.FeeOnlyVersions, at.Asset)
		}
	}
}
