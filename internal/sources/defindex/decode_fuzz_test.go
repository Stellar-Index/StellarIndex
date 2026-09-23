package defindex

import (
	"errors"
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// i128Want is the exact value an Int128Parts wire value carries,
// computed independently of the decoder: (hi << 64) + lo, two's
// complement.
func i128Want(hi int64, lo uint64) *big.Int {
	v := new(big.Int).Lsh(big.NewInt(hi), 64)
	return v.Add(v, new(big.Int).SetUint64(lo))
}

func fuzzI128(hi int64, lo uint64) sdkxdr.ScVal {
	return sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvI128, I128: &sdkxdr.Int128Parts{Hi: sdkxdr.Int64(hi), Lo: sdkxdr.Uint64(lo)}}
}

// fuzzAddr returns an Address ScVal whose 32 key bytes are all fill, and
// the strkey the decoder must produce for it.
func fuzzAddr(t *testing.T, contract bool, fill byte) (sdkxdr.ScVal, string) {
	t.Helper()
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = fill
	}
	vb, addr := strkey.VersionByteAccountID, makeAccountAddress(t, fill)
	if contract {
		vb, addr = strkey.VersionByteContract, makeContractAddress(t, fill)
	}
	s, err := strkey.Encode(vb, raw)
	if err != nil {
		t.Fatal(err)
	}
	return addrSCVal(addr), s
}

// rotated builds a Map with its entries rotated by rot — decode is by
// field NAME, so every rotation must decode identically.
func rotated(t *testing.T, rot uint8, entries ...sdkxdr.ScMapEntry) sdkxdr.ScVal {
	t.Helper()
	out := make([]sdkxdr.ScMapEntry, len(entries))
	for i := range entries {
		out[i] = entries[(i+int(rot))%len(entries)]
	}
	return mapSCVal(t, out...)
}

func fuzzEvent(contract string, topics []string, value string, op, ev uint16) events.Event {
	return events.Event{
		Type:           "contract",
		ContractID:     contract,
		Ledger:         58_000_000,
		LedgerClosedAt: "2025-08-01T00:00:00Z",
		TxHash:         "aa",
		OperationIndex: int(op),
		EventIndex:     int(ev),
		Topic:          topics,
		Value:          value,
	}
}

// FuzzVaultFlowAmountsByName drives DeFindexVault deposit/withdraw through
// the production gate + Decode over arbitrary i128 values and asserts the
// VaultFlow is exact: every Vec<i128> element in wire order and at full
// width (ADR-0003), the share delta from its OWN direction's field name,
// the user from its own field name — regardless of Map field order. A
// body carrying the OTHER direction's field names is a foreign shape and
// must be refused, not decoded into the wrong columns.
//
// Generative run:
// go test -run=^$ -fuzz=^FuzzVaultFlowAmountsByName$ -fuzztime=60s -parallel=2 ./internal/sources/defindex/
func FuzzVaultFlowAmountsByName(f *testing.F) {
	f.Add(uint8(1), int64(0), uint64(10_000_000), int64(0), uint64(0), int64(0), uint64(0), int64(0), uint64(9_990_000), false, uint8(0), uint16(0), uint16(3))
	f.Add(uint8(2), int64(0), uint64(1)<<63, int64(1), uint64(0), int64(0), uint64(0), int64(0), uint64(1), true, uint8(1), uint16(1), uint16(0))
	f.Add(uint8(3), int64(-1), ^uint64(0), int64(^uint64(0)>>1), ^uint64(0), int64(-1)<<63, uint64(0), int64(2), uint64(3), false, uint8(4), uint16(0), uint16(7))
	f.Add(uint8(0), int64(0), uint64(0), int64(0), uint64(0), int64(0), uint64(0), int64(0), uint64(0), true, uint8(2), uint16(2), uint16(2))

	f.Fuzz(func(t *testing.T, n uint8, h1 int64, l1 uint64, h2 int64, l2 uint64, h3 int64, l3 uint64,
		th int64, tl uint64, withdraw bool, rot uint8, op, evIdx uint16,
	) {
		his, los := []int64{h1, h2, h3}, []uint64{l1, l2, l3}
		k := int(n % 4)
		vec := make([]sdkxdr.ScVal, 0, k)
		want := make([]*big.Int, 0, k)
		for i := 0; i < k; i++ {
			vec = append(vec, fuzzI128(his[i], los[i]))
			want = append(want, i128Want(his[i], los[i]))
		}
		userSv, user := fuzzAddr(t, false, 0x21)

		sym, dir := TopicSymbolDeposit, DirectionDeposit
		uF, aF, tF := "depositor", "amounts", "df_tokens_minted"
		xuF, xaF, xtF := "withdrawer", "amounts_withdrawn", "df_tokens_burned"
		if withdraw {
			sym, dir = TopicSymbolWithdraw, DirectionWithdraw
			uF, aF, tF, xuF, xaF, xtF = xuF, xaF, xtF, uF, aF, tF
		}
		body := func(u, a, tk string) string {
			return mustB64(t, rotated(t, rot,
				mapEntry(t, u, userSv),
				mapEntry(t, a, vecSCVal(t, vec...)),
				mapEntry(t, tk, fuzzI128(th, tl)),
				mapEntry(t, "total_managed_funds_before", vecSCVal(t)),
				mapEntry(t, "total_supply_before", fuzzI128(0, 1)),
			))
		}

		d := NewDecoder()
		ev := fuzzEvent(MainnetVaults[0], []string{TopicPrefixVault, sym}, body(uF, aF, tF), op, evIdx)
		if !d.Matches(ev) {
			t.Fatal("vault event from a curated vault not matched")
		}
		out, err := d.Decode(ev)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if len(out) != 1 {
			t.Fatalf("Decode returned %d events, want 1", len(out))
		}
		flow := out[0].(VaultEvent).Flow
		if flow.Direction != dir || flow.User != user {
			t.Fatalf("direction/user = %s/%s, want %s/%s", flow.Direction, flow.User, dir, user)
		}
		if len(flow.Amounts) != k {
			t.Fatalf("len(Amounts) = %d, want %d", len(flow.Amounts), k)
		}
		for i := range want {
			if flow.Amounts[i].BigInt().Cmp(want[i]) != 0 {
				t.Fatalf("Amounts[%d] = %s, want %s", i, flow.Amounts[i], want[i])
			}
		}
		if flow.DfTokens.BigInt().Cmp(i128Want(th, tl)) != 0 {
			t.Fatalf("DfTokens = %s, want %s", flow.DfTokens, i128Want(th, tl))
		}
		if flow.OpIndex != int(op) || flow.EventIndex != uint32(evIdx) {
			t.Fatalf("op/event index = %d/%d, want %d/%d", flow.OpIndex, flow.EventIndex, op, evIdx)
		}

		// The same event carrying the other direction's field names.
		foreign := fuzzEvent(MainnetVaults[0], []string{TopicPrefixVault, sym}, body(xuF, xaF, xtF), op, evIdx)
		if got, err := d.Decode(foreign); !errors.Is(err, ErrMalformedPayload) {
			t.Fatalf("%s topic with %s/%s/%s body: got %v, err %v; want ErrMalformedPayload", sym, xuF, xaF, xtF, got, err)
		}
	})
}

// FuzzDFeesEntries asserts a `dfees` event fans out into exactly one
// DFeesEvent per distributed_fees tuple, in Vec order, with FeeIndex =
// position (the per-entry PK discriminator), the event's own EventIndex
// on every entry, and each (token, i128 amount) exact at full width. An
// empty Vec is zero rows and no error.
//
// Generative run:
// go test -run=^$ -fuzz=^FuzzDFeesEntries$ -fuzztime=60s -parallel=2 ./internal/sources/defindex/
func FuzzDFeesEntries(f *testing.F) {
	f.Add(uint8(0), int64(0), uint64(0), int64(0), uint64(0), uint16(0), uint16(0))
	f.Add(uint8(1), int64(0), uint64(12_345), int64(0), uint64(0), uint16(0), uint16(4))
	f.Add(uint8(2), int64(0), uint64(1)<<63, int64(-1), uint64(1), uint16(1), uint16(2))
	f.Add(uint8(4), int64(^uint64(0)>>1), ^uint64(0), int64(-1)<<63, uint64(0), uint16(3), uint16(11))

	f.Fuzz(func(t *testing.T, n uint8, h1 int64, l1 uint64, h2 int64, l2 uint64, op, evIdx uint16) {
		k := int(n % 5)
		elems := make([]sdkxdr.ScVal, 0, k)
		wantTok := make([]string, 0, k)
		wantAmt := make([]*big.Int, 0, k)
		for i := 0; i < k; i++ {
			tokSv, tok := fuzzAddr(t, true, byte(0x40+i))
			hi, lo := h1, l1
			if i%2 == 1 {
				hi, lo = h2, l2
			}
			elems = append(elems, vecSCVal(t, tokSv, fuzzI128(hi, lo)))
			wantTok = append(wantTok, tok)
			wantAmt = append(wantAmt, i128Want(hi, lo))
		}
		body := mustB64(t, mapSCVal(t, mapEntry(t, "distributed_fees", vecSCVal(t, elems...))))

		d := NewDecoder()
		ev := fuzzEvent(MainnetVaults[0], []string{TopicPrefixVault, TopicSymbolDFees}, body, op, evIdx)
		if !d.Matches(ev) {
			t.Fatal("dfees from a curated vault not matched")
		}
		out, err := d.Decode(ev)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if len(out) != k {
			t.Fatalf("Decode returned %d events for %d distributed_fees entries", len(out), k)
		}
		for i := range out {
			fee := out[i].(DFeesEvent).Fee
			if fee.FeeIndex != i || fee.Token != wantTok[i] || fee.Amount.BigInt().Cmp(wantAmt[i]) != 0 {
				t.Fatalf("entry %d = {idx %d, %s, %s}, want {idx %d, %s, %s}",
					i, fee.FeeIndex, fee.Token, fee.Amount, i, wantTok[i], wantAmt[i])
			}
			if fee.EventIndex != uint32(evIdx) || fee.OpIndex != int(op) || fee.Vault != MainnetVaults[0] {
				t.Fatalf("entry %d identity = {ev %d, op %d, %s}, want {ev %d, op %d, %s}",
					i, fee.EventIndex, fee.OpIndex, fee.Vault, evIdx, op, MainnetVaults[0])
			}
		}
	})
}

// FuzzStrategyFlowAmount asserts every BlendStrategy flow kind decodes
// its own Direction and the exact i128 `amount` (never a truncated low
// limb, never price_per_share on a harvest), from any field order.
//
// Generative run:
// go test -run=^$ -fuzz=^FuzzStrategyFlowAmount$ -fuzztime=60s -parallel=2 ./internal/sources/defindex/
func FuzzStrategyFlowAmount(f *testing.F) {
	for kind := uint8(0); kind < 3; kind++ {
		f.Add(kind, int64(0), uint64(50_000_000), int64(0), uint64(1_000_000), uint8(kind), uint16(kind))
		f.Add(kind, int64(1), uint64(1)<<63, int64(-1), uint64(3), uint8(kind+1), uint16(0))
	}
	f.Fuzz(func(t *testing.T, kind uint8, h int64, l uint64, ph int64, pl uint64, rot uint8, evIdx uint16) {
		syms := []string{TopicSymbolDeposit, TopicSymbolWithdraw, TopicSymbolHarvest}
		dirs := []Direction{DirectionDeposit, DirectionWithdraw, DirectionHarvest}
		k := int(kind % 3)
		fromSv, from := fuzzAddr(t, true, 0x77)
		entries := []sdkxdr.ScMapEntry{mapEntry(t, "from", fromSv), mapEntry(t, "amount", fuzzI128(h, l))}
		if k == 2 {
			entries = append(entries, mapEntry(t, "price_per_share", fuzzI128(ph, pl)))
		}
		d := NewDecoder()
		ev := fuzzEvent(MainnetStrategies[0], []string{TopicPrefixStrategy, syms[k]}, mustB64(t, rotated(t, rot, entries...)), 0, evIdx)
		if !d.Matches(ev) {
			t.Fatal("strategy event from a curated strategy not matched")
		}
		out, err := d.Decode(ev)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if len(out) != 1 {
			t.Fatalf("Decode returned %d events, want 1", len(out))
		}
		flow := out[0].(Event).Flow
		if flow.Direction != dirs[k] || flow.From != from || flow.EventIndex != uint32(evIdx) {
			t.Fatalf("direction/from/ev = %s/%s/%d, want %s/%s/%d", flow.Direction, flow.From, flow.EventIndex, dirs[k], from, evIdx)
		}
		if flow.Amount.BigInt().Cmp(i128Want(h, l)) != 0 {
			t.Fatalf("amount = %s, want %s", flow.Amount, i128Want(h, l))
		}
	})
}
