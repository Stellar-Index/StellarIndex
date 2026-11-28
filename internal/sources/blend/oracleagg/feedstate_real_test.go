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
		// 52 listed assets, 49 index entries: the last three were added without one.
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
			s.Base != c.base || len(s.Assets) != c.assets || len(s.Indexed) != c.index || len(s.Cache) != 2 {
			t.Fatalf("%s state = last %d res %d dp %d protocol %d base %s, %d assets, %d indexed, %d cached",
				c.feed, s.LastTimestampMs, s.ResolutionMs, s.Decimals, s.Protocol, s.Base, len(s.Assets), len(s.Indexed), len(s.Cache))
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
		if _, indexed, listed := s.AssetIndex(c.probe); !indexed || !listed {
			t.Fatalf("%s does not index %s", c.feed, c.probe)
		}
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
		"unknown string key":        with(xdr.ScMapEntry{Key: str("paused"), Val: scval.NewU32(1)}),
		"index entry off the list":  with(xdr.ScMapEntry{Key: symbolKey("ZZZ"), Val: scval.NewU32(3)}),
		"index entry past the list": with(xdr.ScMapEntry{Key: symbolKey("ZZZ"), Val: scval.NewU32(99)}),
		"resolution missing":        without("resolution"),
		"non-String non-asset key":  with(xdr.ScMapEntry{Key: scval.NewU32(7), Val: scval.NewU32(1)}),
	} {
		if _, err := DecodeFeedState(st); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: err = %v, want ErrConfig", name, err)
		}
	}
}
