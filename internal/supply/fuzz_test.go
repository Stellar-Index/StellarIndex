package supply_test

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// fuzzBig decodes an unbounded magnitude, so the targets exercise values
// past 2^63 and 2^128 where an int64 or i128 shortcut would truncate.
func fuzzBig(b []byte, neg bool) *big.Int {
	n := new(big.Int).SetBytes(b)
	if neg {
		n.Neg(n)
	}
	return n
}

// refMaxZero is max(0, a-b) by comparison, not by subtract-then-clamp.
func refMaxZero(a, b *big.Int) *big.Int {
	if a.Cmp(b) <= 0 {
		return big.NewInt(0)
	}
	return new(big.Int).Sub(a, b)
}

func refAbsDiff(a, b *big.Int) *big.Int {
	if a.Cmp(b) >= 0 {
		return new(big.Int).Sub(a, b)
	}
	return new(big.Int).Sub(b, a)
}

func eqBig(a, b *big.Int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Cmp(b) == 0
}

var (
	twoPow64  = new(big.Int).Lsh(big.NewInt(1), 64)
	twoPow128 = new(big.Int).Lsh(big.NewInt(1), 128)
)

func FuzzCrossCheck(f *testing.F) {
	f.Add([]byte{100}, false, []byte{100}, false, []byte{}, false, "")
	f.Add([]byte{100}, false, []byte{101}, false, []byte{102}, true, "partial_wrap")
	f.Add([]byte{100}, false, []byte{98}, false, []byte{99}, true, "full_wrap")
	f.Add(twoPow128.Bytes(), false, twoPow64.Bytes(), false, twoPow128.Bytes(), true, "FULL_WRAP")
	f.Add([]byte{1}, true, []byte{1}, false, []byte{2}, true, "full_wrap")
	f.Fuzz(func(t *testing.T, cb []byte, cneg bool, sb []byte, sneg bool, wb []byte, hasWrapped bool, class string) {
		classicTotal, sacTotal := fuzzBig(cb, cneg), fuzzBig(sb, sneg)
		classic := supply.Supply{AssetKey: "C:G", TotalSupply: new(big.Int).Set(classicTotal)}
		sac := supply.Supply{AssetKey: "CSAC", TotalSupply: new(big.Int).Set(sacTotal)}
		var wrapped *big.Int
		if hasWrapped {
			wrapped = fuzzBig(wb, false)
			classic.SACWrappedStroops = new(big.Int).Set(wrapped)
		}

		full, err := supply.CrossCheck(classic, sac)
		if err != nil {
			t.Fatal(err)
		}
		wantAbs := refAbsDiff(classicTotal, sacTotal)
		if !eqBig(full.DivergenceStroops, wantAbs) {
			t.Fatalf("full divergence = %s, want |%s-%s| = %s", full.DivergenceStroops, classicTotal, sacTotal, wantAbs)
		}
		if full.WithinTolerance != (wantAbs.Cmp(big.NewInt(1)) <= 0) {
			t.Fatalf("full within=%v for divergence %s", full.WithinTolerance, wantAbs)
		}
		if full.WrapClass != supply.WrapClassFull || full.SubsetBoundChecked ||
			full.OverMintStroops != nil || full.EscrowExcessStroops != nil || full.SACWrapped != nil {
			t.Fatalf("full compare leaked partial-wrap fields: %+v", full)
		}
		if !eqBig(full.ClassicTotal, classicTotal) || !eqBig(full.SACTotal, sacTotal) ||
			full.ClassicKey != "C:G" || full.SACKey != "CSAC" {
			t.Fatalf("full compare did not carry its inputs: %+v", full)
		}

		part, err := supply.CrossCheckSubsetBound(classic, sac)
		if err != nil {
			t.Fatal(err)
		}
		if !eqBig(part.OverMintStroops, refMaxZero(sacTotal, classicTotal)) {
			t.Fatalf("over-mint = %s, want max(0, %s-%s)", part.OverMintStroops, sacTotal, classicTotal)
		}
		wantDiv := big.NewInt(0)
		if hasWrapped {
			wantEscrow := refMaxZero(wrapped, sacTotal)
			if !part.SubsetBoundChecked || !eqBig(part.EscrowExcessStroops, wantEscrow) || !eqBig(part.SACWrapped, wrapped) {
				t.Fatalf("escrow leg = %+v, want excess %s", part, wantEscrow)
			}
			wantDiv = wantEscrow
		} else if part.SubsetBoundChecked || part.EscrowExcessStroops != nil || part.SACWrapped != nil {
			t.Fatalf("escrow leg evaluated without a SACWrapped component: %+v", part)
		}
		if !eqBig(part.DivergenceStroops, wantDiv) {
			t.Fatalf("partial divergence = %s, want %s (over-mint is diagnostic only)", part.DivergenceStroops, wantDiv)
		}
		if part.WithinTolerance != (wantDiv.Cmp(big.NewInt(1)) <= 0) || part.WrapClass != supply.WrapClassPartial {
			t.Fatalf("partial within=%v class=%q for divergence %s", part.WithinTolerance, part.WrapClass, wantDiv)
		}

		// No result field may alias an input.
		for _, r := range []*big.Int{full.ClassicTotal, full.SACTotal, full.DivergenceStroops, part.ClassicTotal, part.SACTotal, part.DivergenceStroops, part.SACWrapped} {
			if r != nil {
				r.Add(r, big.NewInt(7))
			}
		}
		if !eqBig(classic.TotalSupply, classicTotal) || !eqBig(sac.TotalSupply, sacTotal) ||
			(hasWrapped && !eqBig(classic.SACWrappedStroops, wrapped)) {
			t.Fatal("a cross-check result aliases its input")
		}

		// Dispatch: only the exact full_wrap class takes the strict compare.
		got, err := supply.CrossCheckForClass(classic, sac, supply.WrapClass(class))
		if err != nil {
			t.Fatal(err)
		}
		wantClass := supply.WrapClassPartial
		if class == string(supply.WrapClassFull) {
			wantClass = supply.WrapClassFull
		}
		if got.WrapClass != wantClass {
			t.Fatalf("class %q dispatched to %q, want %q", class, got.WrapClass, wantClass)
		}
	})
}

// classicFuzzReader returns fixed components.
type classicFuzzReader struct {
	comps supply.ClassicSupplyComponents
}

func (r classicFuzzReader) ClassicSupplyAt(context.Context, canonical.Asset, supply.LockedSet, uint32) (supply.ClassicSupplyComponents, error) {
	return r.comps, nil
}

func FuzzClassicCompute(f *testing.F) {
	f.Add([]byte{0x03, 0xe8}, []byte{10}, []byte{20}, []byte{30}, []byte{100}, []byte{5}, []byte{5}, uint8(0), "", false, uint32(100), uint32(99))
	f.Add(twoPow128.Bytes(), twoPow64.Bytes(), []byte{}, []byte{1}, twoPow128.Bytes(), []byte{}, []byte{}, uint8(0), "1000", true, uint32(7), uint32(0))
	f.Add([]byte{1}, []byte{}, []byte{}, []byte{}, []byte{2}, []byte{3}, []byte{}, uint8(0), "", true, uint32(1), uint32(1))
	f.Add([]byte{1}, []byte{1}, []byte{1}, []byte{1}, []byte{1}, []byte{1}, []byte{1}, uint8(0x10), "-5", false, uint32(1), uint32(1))
	f.Add([]byte{1}, []byte{}, []byte{}, []byte{}, []byte{}, []byte{}, []byte{}, uint8(0), "x", false, uint32(1), uint32(1))
	f.Add([]byte{1}, []byte{}, []byte{}, []byte{7}, []byte{}, []byte{}, []byte{}, uint8(0x80), "", false, uint32(1), uint32(1))
	f.Fuzz(func(t *testing.T, trust, claim, lp, sacw, issuerB, lockA, lockC []byte, negMask uint8, override string, lockedNonEmpty bool, ledger, minLedger uint32) {
		vals := []*big.Int{
			fuzzBig(trust, negMask&1 != 0), fuzzBig(claim, negMask&2 != 0), fuzzBig(lp, negMask&4 != 0),
			fuzzBig(sacw, negMask&8 != 0), fuzzBig(issuerB, negMask&16 != 0), fuzzBig(lockA, negMask&32 != 0),
			fuzzBig(lockC, negMask&64 != 0),
		}
		anyNeg := false
		for _, v := range vals {
			anyNeg = anyNeg || v.Sign() < 0
		}
		comps := supply.ClassicSupplyComponents{
			Trustline: new(big.Int).Set(vals[0]), Claimable: new(big.Int).Set(vals[1]),
			LPReserve: new(big.Int).Set(vals[2]), SACWrapped: new(big.Int).Set(vals[3]),
			IssuerBalance: new(big.Int).Set(vals[4]), LockedAccountBalances: new(big.Int).Set(vals[5]),
			LockedContractBalances: new(big.Int).Set(vals[6]), MinComponentLedger: minLedger,
			// negMask's spare high bit drives the CS-087 gate: an unobserved SAC must yield nil.
			SACObserved: negMask&0x80 == 0,
		}
		asset, err := canonical.NewClassicAsset("USDC", validIssuer)
		if err != nil {
			t.Fatal(err)
		}
		key := "USDC:" + validIssuer
		pol := supply.Policy{MaxSupplyOverrides: map[string]string{key: override}}
		if lockedNonEmpty {
			pol.PerAsset = map[string]supply.LockedSet{key: {Accounts: []string{"GLOCKED"}}}
		}
		c, err := supply.NewClassicComputer(pol, classicFuzzReader{comps: comps})
		if err != nil {
			t.Fatal(err)
		}
		at := time.Unix(1_700_000_000, 0)
		got, err := c.Compute(context.Background(), asset, ledger, at)
		wantOverride, overrideOK := new(big.Int).SetString(override, 10)
		badOverride := override != "" && (!overrideOK || wantOverride.Sign() < 0)
		if anyNeg || badOverride {
			if err == nil {
				t.Fatalf("negative component or bad override %q accepted: %+v", override, got)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}

		total := new(big.Int)
		for _, v := range []*big.Int{vals[3], vals[2], vals[1], vals[0]} {
			total.Add(total, v)
		}
		excluded := new(big.Int).Add(vals[4], new(big.Int).Add(vals[5], vals[6]))
		if !eqBig(got.TotalSupply, total) {
			t.Fatalf("total = %s, want sum of the four holding domains %s", got.TotalSupply, total)
		}
		if !eqBig(got.CirculatingSupply, refMaxZero(total, excluded)) {
			t.Fatalf("circulating = %s, want max(0, %s-%s)", got.CirculatingSupply, total, excluded)
		}
		switch {
		case !comps.SACObserved:
			if got.SACWrappedStroops != nil {
				t.Fatalf("SACWrappedStroops = %s with no SAC observation, want nil", got.SACWrappedStroops)
			}
		case !eqBig(got.SACWrappedStroops, vals[3]):
			t.Fatalf("SACWrappedStroops = %v, want %s", got.SACWrappedStroops, vals[3])
		default:
			got.SACWrappedStroops.Add(got.SACWrappedStroops, big.NewInt(1))
			if !eqBig(comps.SACWrapped, vals[3]) {
				t.Fatal("SACWrappedStroops aliases the reader's component")
			}
		}

		wantBasis := supply.BasisIssuerExclusion
		if override != "" {
			if !eqBig(got.MaxSupply, wantOverride) {
				t.Fatalf("max = %v, want override %s", got.MaxSupply, wantOverride)
			}
			wantBasis = supply.BasisOverride
		} else if got.MaxSupply != nil {
			t.Fatalf("max = %s with no override", got.MaxSupply)
		}
		if lockedNonEmpty {
			wantBasis = supply.BasisOverride
		}
		if got.Basis != wantBasis {
			t.Fatalf("basis = %q, want %q", got.Basis, wantBasis)
		}
		if got.AssetKey != key || got.LedgerSequence != ledger || got.MinComponentLedger != minLedger || !got.ObservedAt.Equal(at) {
			t.Fatalf("identity fields not carried: %+v", got)
		}
	})
}

type sep41FuzzReader struct{ comps supply.SEP41SupplyComponents }

func (r sep41FuzzReader) SEP41SupplyAt(context.Context, canonical.Asset, supply.LockedSet, uint32) (supply.SEP41SupplyComponents, error) {
	return r.comps, nil
}

func FuzzSEP41Compute(f *testing.F) {
	f.Add([]byte{100}, []byte{10}, []byte{5}, []byte{0}, []byte{3}, []byte{2}, uint8(0), false, "", false)
	f.Add([]byte{100}, []byte{10}, []byte{5}, []byte{7}, []byte{}, []byte{}, uint8(0), true, "", false)
	f.Add([]byte{5}, []byte{10}, []byte{}, []byte{}, []byte{}, []byte{}, uint8(0), false, "", false)
	f.Add([]byte{5}, []byte{10}, []byte{}, []byte{}, []byte{}, []byte{}, uint8(0), true, "", false)
	f.Add(twoPow128.Bytes(), twoPow64.Bytes(), []byte{1}, []byte{}, twoPow128.Bytes(), []byte{}, uint8(0), true, "42", true)
	f.Add([]byte{10}, []byte{10}, []byte{}, []byte{}, []byte{}, []byte{}, uint8(0), true, "", false)
	f.Fuzz(func(t *testing.T, mintB, burnB, clawB, adminB, lockA, lockC []byte, negMask uint8, seeded bool, override string, lockedNonEmpty bool) {
		vals := []*big.Int{
			fuzzBig(mintB, negMask&1 != 0), fuzzBig(burnB, negMask&2 != 0), fuzzBig(clawB, negMask&4 != 0),
			fuzzBig(adminB, negMask&8 != 0), fuzzBig(lockA, negMask&16 != 0), fuzzBig(lockC, negMask&32 != 0),
		}
		anyNeg := false
		for _, v := range vals {
			anyNeg = anyNeg || v.Sign() < 0
		}
		comps := supply.SEP41SupplyComponents{
			MintTotal: new(big.Int).Set(vals[0]), BurnTotal: new(big.Int).Set(vals[1]),
			ClawbackTotal: new(big.Int).Set(vals[2]), AdminBalance: new(big.Int).Set(vals[3]),
			LockedAccountBalances: new(big.Int).Set(vals[4]), LockedContractBalances: new(big.Int).Set(vals[5]),
			GenesisBaselineSeeded: seeded, MinComponentLedger: 9,
		}
		asset, err := canonical.NewSorobanAsset(validContractID)
		if err != nil {
			t.Fatal(err)
		}
		pol := supply.Policy{MaxSupplyOverrides: map[string]string{validContractID: override}}
		if lockedNonEmpty {
			pol.PerAsset = map[string]supply.LockedSet{validContractID: {Contracts: []string{"CLOCKED"}}}
		}
		c, err := supply.NewSEP41Computer(pol, sep41FuzzReader{comps: comps})
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.Compute(context.Background(), asset, 10, time.Unix(1_700_000_000, 0))
		if anyNeg {
			if err == nil {
				t.Fatalf("negative component accepted: %+v", got)
			}
			return
		}
		removed := new(big.Int).Add(vals[1], vals[2])
		if vals[0].Cmp(removed) < 0 {
			want := supply.ErrNegativeTotalMissingBaseline
			if seeded {
				want = supply.ErrNegativeTotalSupply
			}
			if !errors.Is(err, want) {
				t.Fatalf("mint %s < burn+clawback %s (seeded=%v): err = %v, want %v", vals[0], removed, seeded, err, want)
			}
			return
		}
		wantOverride, overrideOK := new(big.Int).SetString(override, 10)
		if override != "" && (!overrideOK || wantOverride.Sign() < 0) {
			if err == nil {
				t.Fatalf("bad override %q accepted", override)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		total := new(big.Int).Sub(vals[0], removed)
		excluded := new(big.Int).Add(vals[3], new(big.Int).Add(vals[4], vals[5]))
		if !eqBig(got.TotalSupply, total) {
			t.Fatalf("total = %s, want mint-burn-clawback %s", got.TotalSupply, total)
		}
		if !eqBig(got.CirculatingSupply, refMaxZero(total, excluded)) {
			t.Fatalf("circulating = %s, want max(0, %s-%s)", got.CirculatingSupply, total, excluded)
		}
		var wantBasis supply.Basis
		switch {
		case override != "" || lockedNonEmpty:
			wantBasis = supply.BasisOverride
		case vals[3].Sign() > 0:
			wantBasis = supply.BasisAdminExclusion
		default:
			wantBasis = supply.BasisSEP41TotalOnly
		}
		if got.Basis != wantBasis {
			t.Fatalf("basis = %q, want %q (admin=%s override=%q locked=%v)", got.Basis, wantBasis, vals[3], override, lockedNonEmpty)
		}
		if override != "" && !eqBig(got.MaxSupply, wantOverride) {
			t.Fatalf("max = %v, want %s", got.MaxSupply, wantOverride)
		}
		if got.SACWrappedStroops != nil || got.MinComponentLedger != 9 || got.AssetKey != validContractID {
			t.Fatalf("SEP-41 snapshot fields: %+v", got)
		}
	})
}

func FuzzXLMCompute(f *testing.F) {
	f.Add("41080000000000000", "10", uint8(2), uint8(0))
	f.Add("0", "0", uint8(0), uint8(1))
	f.Add("500018068120000000", "1", uint8(2), uint8(0))
	f.Add("1000000000000000000", "0", uint8(1), uint8(2))
	f.Add("340282366920938463463374607431768211456", "1", uint8(2), uint8(1))
	f.Add("-1", "1", uint8(2), uint8(0))
	f.Fuzz(func(t *testing.T, balA, balB string, nAccounts, network uint8) {
		accounts := []string{"GA", "GB"}[:nAccounts%3]
		reader, err := supply.NewConfigReserveBalanceReader(map[string]string{"GA": balA, "GB": balB}, time.Now(), time.Hour)
		a, aok := new(big.Int).SetString(balA, 10)
		b, bok := new(big.Int).SetString(balB, 10)
		if !aok || !bok || a.Sign() < 0 || b.Sign() < 0 {
			if err == nil {
				t.Fatalf("reader accepted %q/%q", balA, balB)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		passphrase := []string{canonical.PubnetPassphrase, canonical.TestnetPassphrase, canonical.FuturenetPassphrase}[network%3]
		c, err := supply.NewXLMComputerForNetwork(passphrase, accounts, reader)
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.Compute(context.Background(), 5, time.Unix(1_700_000_000, 0))
		if err != nil {
			t.Fatal(err)
		}
		total, err := supply.XLMTotalSupplyStroopsForNetwork(passphrase)
		if err != nil {
			t.Fatal(err)
		}
		reserved := big.NewInt(0)
		for _, acc := range accounts {
			reserved.Add(reserved, map[string]*big.Int{"GA": a, "GB": b}[acc])
		}
		if !eqBig(got.TotalSupply, total) || !eqBig(got.MaxSupply, total) {
			t.Fatalf("total/max = %s/%s, want %s", got.TotalSupply, got.MaxSupply, total)
		}
		if !eqBig(got.CirculatingSupply, refMaxZero(total, reserved)) {
			t.Fatalf("circulating = %s, want max(0, %s-%s)", got.CirculatingSupply, total, reserved)
		}
		wantBasis := supply.BasisXLMSDFReserveExclusionStatic // the reader under test is the static map
		if len(accounts) == 0 {
			wantBasis = supply.BasisXLMTotalOnly
		}
		if got.Basis != wantBasis {
			t.Fatalf("basis = %q with %d reserve accounts", got.Basis, len(accounts))
		}
		got.MaxSupply.Add(got.MaxSupply, big.NewInt(1))
		if !eqBig(got.TotalSupply, total) {
			t.Fatal("MaxSupply aliases TotalSupply")
		}
	})
}

func FuzzMaxSupplyOverride(f *testing.F) {
	for _, s := range []string{"", "0", "1000", "-1", "-0", "+5", "0x10", "1_000", " 1", "1e9", "340282366920938463463374607431768211456"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		pol := supply.Policy{MaxSupplyOverrides: map[string]string{validContractID: raw}}
		got, ok, err := pol.MaxSupplyOverride(validContractID)
		vErr := pol.Validate()
		if raw == "" {
			if got != nil || ok || err != nil || vErr != nil {
				t.Fatalf("empty override must fall through: %v %v %v %v", got, ok, err, vErr)
			}
			return
		}
		want, parsed := new(big.Int).SetString(raw, 10)
		if !parsed || want.Sign() < 0 {
			if err == nil || ok || got != nil || vErr == nil {
				t.Fatalf("override %q: got %v ok=%v err=%v validate=%v, want rejection", raw, got, ok, err, vErr)
			}
			return
		}
		if err != nil || !ok || vErr != nil || !eqBig(got, want) {
			t.Fatalf("override %q: got %v ok=%v err=%v validate=%v, want %s", raw, got, ok, err, vErr, want)
		}
		if again, _ := new(big.Int).SetString(got.String(), 10); !eqBig(again, got) {
			t.Fatalf("override %q does not round-trip through its decimal string", raw)
		}
	})
}

// FuzzCanonicalizeWatchedClassic pins that a dash-form entry is accepted
// only when it names a real classic asset, and maps to exactly the
// CODE:ISSUER key the observers decode.
func FuzzCanonicalizeWatchedClassic(f *testing.F) {
	for _, s := range []string{
		"USDC-" + validIssuer, "USDC:" + validIssuer, "native", "", "not-an-asset",
		"crypto:XLM", "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, entry string) {
		out, err := supply.CanonicalizeWatchedClassic([]string{entry})
		if err != nil {
			return
		}
		if len(out) != 1 {
			t.Fatalf("one entry in, %d out", len(out))
		}
		if strings.Contains(entry, ":") {
			if out[0] != entry {
				t.Fatalf("colon-form %q rewritten to %q", entry, out[0])
			}
			return
		}
		a, perr := canonical.ParseAsset(entry)
		if perr != nil || a.Type != canonical.AssetClassic {
			t.Fatalf("accepted %q, which is not a classic asset (%v)", entry, perr)
		}
		if want, _ := supply.AssetKey(a); out[0] != want {
			t.Fatalf("%q -> %q, want the supply key %q", entry, out[0], want)
		}
	})
}
