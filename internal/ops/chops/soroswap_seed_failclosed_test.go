package chops

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
)

// The soroswap pair seed must fail closed: a CONFIGURED seed whose lake read fails or finds no new_pair event returns an
// error. A DISABLED seed (empty factory_contract, the default) is not a failure, so hosts without a seed still get verdicts.

// seedFixtureStrkey returns a deterministic C-strkey.
func seedFixtureStrkey(t *testing.T, b byte) string {
	t.Helper()
	var raw [32]byte
	raw[0] = b
	s, err := strkey.Encode(strkey.VersionByteContract, raw[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	return s
}

func seedFixtureAddr(t *testing.T, strk string) xdr.ScVal {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, strk)
	if err != nil {
		t.Fatalf("strkey.Decode(%q): %v", strk, err)
	}
	var cid xdr.ContractId
	copy(cid[:], raw)
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr}
}

func seedFixtureB64(t *testing.T, sv xdr.ScVal) string {
	t.Helper()
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func seedFixtureSym(t *testing.T, name string) xdr.ScVal {
	t.Helper()
	sym := xdr.ScSymbol(name)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
}

// newPairEvent is a factory new_pair event as the lake stores it.
func newPairEvent(t *testing.T, factory, pair, token0, token1 string) events.Event {
	t.Helper()
	npL := xdr.Uint32(1)
	m := xdr.ScMap{
		{Key: seedFixtureSym(t, "new_pairs_length"), Val: xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &npL}},
		{Key: seedFixtureSym(t, "pair"), Val: seedFixtureAddr(t, pair)},
		{Key: seedFixtureSym(t, "token_0"), Val: seedFixtureAddr(t, token0)},
		{Key: seedFixtureSym(t, "token_1"), Val: seedFixtureAddr(t, token1)},
	}
	mp := &m
	return events.Event{
		ContractID:     factory,
		Topic:          []string{soroswap.TopicPrefixFactory, soroswap.TopicSymbolNewPair},
		Value:          seedFixtureB64(t, xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}),
		Ledger:         52_000_000,
		TxHash:         "factorytx0",
		LedgerClosedAt: "2026-04-23T12:00:00Z",
	}
}

func pairSyncEvent(pairID string) events.Event {
	return events.Event{
		ContractID: pairID,
		Topic:      []string{soroswap.TopicPrefixPair, soroswap.TopicSymbolSync},
	}
}

func streamOf(evs ...events.Event) func(func(events.Event) error) error {
	return func(fn func(events.Event) error) error {
		for _, ev := range evs {
			if err := fn(ev); err != nil {
				return err
			}
		}
		return nil
	}
}

// An empty factory is the documented disable and the zero-value default. It must NOT be an error: both callers return
// every error, so an error here aborts the whole nightly pass on a seed-less host. It must not touch the lake either.
func TestSeedSoroswapForRecon_DisabledSeedIsNotAFailure(t *testing.T) {
	var err error
	out := captureStderr(t, func() {
		err = seedSoroswapForRecon(context.Background(), config.Config{}, "127.0.0.1:1", soroswap.NewDecoder())
	})
	if err != nil {
		t.Fatalf("seed with oracle.soroswap.factory_contract empty returned %q; want nil", err)
	}
	if !strings.Contains(out, "soroswap pair seed: disabled") {
		t.Errorf("stderr %q is missing the disabled-seed notice", out)
	}
	for _, cmd := range []string{"verify-reconciliation", "compute-completeness"} {
		if strings.Contains(out, cmd) {
			t.Errorf("seed notice names the command %q, but the helper is shared by both: %q", cmd, out)
		}
	}
}

// A configured seed whose lake is unreachable fails; it must not fall through to an empty registry.
func TestSeedSoroswapForRecon_LakeErrorFails(t *testing.T) {
	var cfg config.Config
	cfg.Oracle.Soroswap.FactoryContract = soroswap.MainnetFactory
	if err := seedSoroswapForRecon(context.Background(), cfg, "127.0.0.1:1", soroswap.NewDecoder()); err == nil {
		t.Fatal("seed against an unreachable lake returned nil; want an error")
	}
}

// The lake seed registers each verified factory's pairs on the decoder the caller passed in, and ignores a new_pair
// from a foreign contract (ADR-0035).
func TestSeedSoroswapPairs_SeedsVerifiedFactoriesOnly(t *testing.T) {
	pair := seedFixtureStrkey(t, 0x10)
	foreignPair := seedFixtureStrkey(t, 0x11)
	tok0, tok1 := seedFixtureStrkey(t, 0x20), seedFixtureStrkey(t, 0x21)
	dec := soroswap.NewDecoder()
	if dec.Matches(pairSyncEvent(pair)) {
		t.Fatal("unseeded decoder matched a pair event; the test would pass vacuously")
	}
	n, err := seedSoroswapPairs(dec, streamOf(
		newPairEvent(t, soroswap.MainnetFactories[1], pair, tok0, tok1),
		newPairEvent(t, seedFixtureStrkey(t, 0x99), foreignPair, tok0, tok1),
	))
	if err != nil || n != 1 {
		t.Fatalf("seedSoroswapPairs = %d, %v; want 1, nil", n, err)
	}
	if !dec.Matches(pairSyncEvent(pair)) {
		t.Error("seeded pair from a verified factory is not matched")
	}
	if dec.Matches(pairSyncEvent(foreignPair)) {
		t.Error("pair from a foreign factory was seeded")
	}
}

// No new_pair event, a stream error, or an undecodable new_pair each fail the seed.
func TestSeedSoroswapPairs_FailsClosed(t *testing.T) {
	bad := newPairEvent(t, soroswap.MainnetFactory, seedFixtureStrkey(t, 0x10), seedFixtureStrkey(t, 0x20), seedFixtureStrkey(t, 0x21))
	bad.Value = "AAAA"
	streamErr := errors.New("lake read failed")
	cases := map[string]func(func(events.Event) error) error{
		"empty":       streamOf(),
		"stream":      func(func(events.Event) error) error { return streamErr },
		"undecodable": streamOf(bad),
	}
	for name, stream := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := seedSoroswapPairs(soroswap.NewDecoder(), stream); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = orig })
	fn()
	_ = w.Close()
	os.Stderr = orig
	out, _ := io.ReadAll(r)
	return string(out)
}

// The recognition census must seed the soroswap decoder with the pair registry (the same set attribution folds into
// ownerOf); unseeded, pairTokens is empty and Matches() rejects every real pair event as a false gap. Both directions
// run on the same event: unseeded rejects, seeded recognizes.
func TestRecognitionCensus_SoroswapPairSeedRequired(t *testing.T) {
	// A real registered soroswap pair contract (from the r1 pair registry).
	const pairID = "CB46LMGJC7SYSH4C7SBNLV635OX5BSNQDGRR32NRXAV7N2AVNZMQUJ3A"
	// A pair "sync" flow event — topic[0]=String("SoroswapPair"), topic[1]=Symbol("sync").
	pairEvent := events.Event{
		ContractID: pairID,
		Topic:      []string{soroswap.TopicPrefixPair, soroswap.TopicSymbolSync},
	}

	// Control: an UNSEEDED census dispatcher must NOT recognize the pair event.
	unseeded, err := pipeline.BuildDispatcher([]string{soroswap.SourceName}, config.OracleConfig{}, nil)
	if err != nil {
		t.Fatalf("build unseeded dispatcher: %v", err)
	}
	if _, ok := unseeded.Recognize(pairEvent); ok {
		t.Fatal("unseeded census recognized a soroswap pair event — the bug is not reproduced; " +
			"this test would pass vacuously and prove nothing")
	}

	// The fix: a census dispatcher SEEDED with the pair registry recognizes it.
	seed := map[string]soroswap.PairTokens{pairID: {}} // Matches() needs only key presence
	seeded, err := pipeline.BuildDispatcher(
		[]string{soroswap.SourceName}, config.OracleConfig{}, nil,
		soroswap.WithSeededPairTokensDecoder(seed),
	)
	if err != nil {
		t.Fatalf("build seeded dispatcher: %v", err)
	}
	if _, ok := seeded.Recognize(pairEvent); !ok {
		t.Fatal("seeded census did NOT recognize a real soroswap pair event — the fix is ineffective")
	}
}
