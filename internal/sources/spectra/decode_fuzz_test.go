package spectra

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// FuzzWrapUnwrapByName asserts wrap/unwrap decode {shares, vault_shares} by
// field NAME at full i128 width, in either Map order, and that a body
// missing a named field is refused rather than zero-filled.
//
// Generative run:
// go test -run=^$ -fuzz=^FuzzWrapUnwrapByName$ -fuzztime=60s -parallel=2 ./internal/sources/spectra/
func FuzzWrapUnwrapByName(f *testing.F) {
	f.Add(int64(0), uint64(1e18), int64(0), uint64(1e18), false, false, uint16(0), uint16(1))
	f.Add(int64(0), uint64(1)<<63, int64(1), uint64(0), true, true, uint16(2), uint16(9))
	f.Add(int64(-1), uint64(5), int64(^uint64(0)>>1), ^uint64(0), false, true, uint16(0), uint16(0))
	f.Add(int64(7), uint64(7), int64(7), uint64(7), true, false, uint16(4), uint16(4))

	f.Fuzz(func(t *testing.T, sh int64, sl uint64, vh int64, vl uint64, reverse, unwrap bool, op, evIdx uint16) {
		shares, vault := i128Want(sh, sl), i128Want(vh, vl)
		caller, cT := fuzzAccount(t, 0x51)
		receiver, rT := fuzzAccount(t, 0x52)
		owner, oT := fuzzAccount(t, 0x53)

		names, vals := []string{"shares", "vault_shares"}, []xdr.ScVal{fuzzI128(sh, sl), fuzzI128(vh, vl)}
		if reverse {
			names, vals = []string{"vault_shares", "shares"}, []xdr.ScVal{fuzzI128(vh, vl), fuzzI128(sh, sl)}
		}
		topics, kind := []string{TopicSymbolWrap, cT, rT}, EventWrap
		if unwrap {
			topics, kind = []string{TopicSymbolUnwrap, cT, rT, oT}, EventUnwrap
		}

		got := decodeOneFuzz(t, fuzzEvent(topics, fuzzB64(t, fuzzMap(names, vals)), op, evIdx))
		if got.Kind != kind {
			t.Fatalf("kind = %s, want %s", got.Kind, kind)
		}
		if got.Shares.BigInt().Cmp(shares) != 0 || got.VaultShares.BigInt().Cmp(vault) != 0 {
			t.Fatalf("shares/vault_shares = %s/%s, want %s/%s", got.Shares, got.VaultShares, shares, vault)
		}
		wantOwner := ""
		if unwrap {
			wantOwner = owner
		}
		if got.Caller != caller || got.Receiver != receiver || got.Owner != wantOwner {
			t.Fatalf("caller/receiver/owner = %s/%s/%s", got.Caller, got.Receiver, got.Owner)
		}
		if got.OpIndex != uint32(op) || got.EventIndex != uint32(evIdx) {
			t.Fatalf("op/event index = %d/%d, want %d/%d", got.OpIndex, got.EventIndex, op, evIdx)
		}

		short := fuzzMap([]string{"shares"}, []xdr.ScVal{fuzzI128(sh, sl)})
		if _, err := NewDecoder().Decode(fuzzEvent(topics, fuzzB64(t, short), op, evIdx)); err == nil {
			t.Fatal("body without vault_shares decoded; must be refused")
		}
	})
}
