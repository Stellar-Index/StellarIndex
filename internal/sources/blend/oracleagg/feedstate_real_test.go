package oracleagg

import (
	"errors"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// lastRound is both feeds' last_timestamp in reflector-instances.tsv (captured at L65712333).
const lastRound = uint64(1_795_847_700_000)

func feedInstance(t *testing.T, contract string) fixtureEntry {
	t.Helper()
	for _, e := range readEntries(t, "reflector-instances.tsv", 3) {
		if e.contract == contract {
			return e
		}
	}
	t.Fatalf("no feed instance for %s", contract)
	return fixtureEntry{}
}

// The two Reflector feeds Blend's adapters read, decoded from their real instance entries. The history mask's
// period-0 bit of every asset must equal the cached last round's update mask: two independent encodings of one round.
func TestDecodeFeedState_RealEntries(t *testing.T) {
	for _, c := range []struct {
		feed          string
		base          Asset
		assets, index int
		probe         Asset
	}{
		{feedCEX, Asset{Symbol: "USD"}, 16, 16, Asset{Symbol: "XLM"}},
		// 52 listed assets, 49 legacy index entries: the last three were added under v7.1.0, which writes none.
		{feedDEX, Stellar(usdcSAC), 52, 49, Stellar(xlmSAC)},
	} {
		e := feedInstance(t, c.feed)
		if e.wasm != ReflectorV71Hash {
			t.Fatalf("%s wasm = %s", c.feed, e.wasm)
		}
		s, err := DecodeFeedState(storageOf(t, e))
		if err != nil {
			t.Fatalf("%s: %v", c.feed, err)
		}
		if s.LastTimestampMs != lastRound || s.ResolutionMs != 300_000 || s.Decimals != 14 || s.Protocol != ReflectorProtocol ||
			s.Base != c.base || len(s.Assets) != c.assets || len(s.LegacyIndex) != c.index || len(s.Cache) != 2 {
			t.Fatalf("%s state = last %d res %d dp %d protocol %d base %s, %d assets, %d legacy, %d cached",
				c.feed, s.LastTimestampMs, s.ResolutionMs, s.Decimals, s.Protocol, s.Base, len(s.Assets), len(s.LegacyIndex), len(s.Cache))
		}
		if s.Cache[0].TimestampMs != lastRound || s.Cache[1].TimestampMs != lastRound-300_000 {
			t.Fatalf("%s cache rounds %d, %d", c.feed, s.Cache[0].TimestampMs, s.Cache[1].TimestampMs)
		}
		priced := 0
		for i := range s.Assets {
			_, cached := s.CachedPrice(uint32(i), lastRound)
			if s.HasPrice(uint32(i), 0) != cached {
				t.Fatalf("%s asset %d (%s): history bit %v, cache mask %v", c.feed, i, s.Assets[i], s.HasPrice(uint32(i), 0), cached)
			}
			if cached {
				priced++
			}
		}
		if priced == 0 || priced != len(s.Cache[0].Prices) {
			t.Fatalf("%s: %d assets priced in the last round, cache carries %d prices", c.feed, priced, len(s.Cache[0].Prices))
		}
		if _, ok := s.AssetIndex(c.probe); !ok {
			t.Fatalf("%s does not list %s", c.feed, c.probe)
		}
	}
}

// Every DEX-feed asset resolves by list position (assets.rs::resolve_asset_index), the three without a legacy entry
// included; every legacy entry that exists agrees with that position; a duplicate resolves to its FIRST position.
func TestFeedAssetIndex_RealDEX(t *testing.T) {
	s, err := DecodeFeedState(storageOf(t, feedInstance(t, feedDEX)))
	if err != nil {
		t.Fatal(err)
	}
	unlisted := 0
	for i, a := range s.Assets {
		idx, ok := s.AssetIndex(a)
		if !ok || int(idx) != i {
			t.Fatalf("asset %d (%s) resolves to %d (ok %v)", i, a, idx, ok)
		}
		if legacy, has := s.LegacyIndex[a]; !has {
			unlisted++
		} else if int(legacy) != i {
			t.Errorf("legacy entry %s = %d, list position %d", a, legacy, i)
		}
	}
	if len(s.Assets) != 52 || unlisted != 3 {
		t.Fatalf("%d assets, %d without a legacy entry; want 52 and 3", len(s.Assets), unlisted)
	}
	for _, i := range []int{49, 50, 51} {
		if _, has := s.LegacyIndex[s.Assets[i]]; has {
			t.Fatalf("position %d unexpectedly has a legacy entry", i)
		}
	}
	dup := s
	dup.Assets = append(append([]Asset{}, s.Assets...), s.Assets[7])
	if idx, ok := dup.AssetIndex(s.Assets[7]); !ok || idx != 7 {
		t.Fatalf("duplicate of position 7 resolves to %d (ok %v), want the first index", idx, ok)
	}
}

// One malformed AssetConfig fails that asset only; the adapter's other assets still decode.
func TestDecodeV2Instance_BadAssetIsolated(t *testing.T) {
	var bad Asset
	var st []xdr.ScMapEntry
	for _, e := range storageOf(t, instanceOf(t, "CCVTVW2CVA7JLH4ROQGP3CU4T3EXVCK66AZGSM4MUQPXAI4QHCZPOATS")) {
		if s, _ := scval.AsSymbol(e.Key); s == "Assets" {
			m := append(xdr.ScMap{}, *e.Val.MustMap()...)
			bad, _ = DecodeAsset(m[0].Key)
			m[0].Val = scval.NewU32(1)
			mp := &m
			e.Val = xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}
		}
		st = append(st, e)
	}
	cfg, err := DecodeV2Instance(st)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Unreadable[bad]; !ok || len(cfg.Unreadable) != 1 || len(cfg.Assets) != 2 {
		t.Fatalf("unreadable %v, %d assets decoded; want only %s unreadable and the other two decoded", cfg.Unreadable, len(cfg.Assets), bad)
	}
	if r := LastPrice(cfg, bad, r, Feed{}); r.Verdict != VerdictUnsupported {
		t.Fatalf("bad asset verdict %q", r.Verdict)
	}
}

func TestDecodeFeedState_Rejects(t *testing.T) {
	base := storageOf(t, feedInstance(t, feedCEX))
	with := func(extra xdr.ScMapEntry) []xdr.ScMapEntry { return append(append([]xdr.ScMapEntry{}, base...), extra) }
	str := func(s string) xdr.ScVal {
		v := xdr.ScString(s)
		return xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &v}
	}
	without := func(key string) []xdr.ScMapEntry {
		var out []xdr.ScMapEntry
		for _, e := range base {
			if s, err := scval.AsString(e.Key); err != nil || s != key {
				out = append(out, e)
			}
		}
		return out
	}
	for name, st := range map[string][]xdr.ScMapEntry{
		"unknown string key":       with(xdr.ScMapEntry{Key: str("paused"), Val: scval.NewU32(1)}),
		"legacy entry not a u32":   with(xdr.ScMapEntry{Key: symbolKey("ZZZ"), Val: symbolKey("x")}),
		"resolution missing":       without("resolution"),
		"non-String non-asset key": with(xdr.ScMapEntry{Key: scval.NewU32(7), Val: scval.NewU32(1)}),
	} {
		if _, err := DecodeFeedState(st); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: err = %v, want ErrConfig", name, err)
		}
	}
}
