package classicmovements

import (
	"errors"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
)

// Real pre-P23 mainnet bytes, pulled read-only from r1's ClickHouse
// lake (HTTP :8123, stellar.operations JOIN stellar.operation_results,
// windowed + LIMIT'd queries per CLAUDE.md's heavy-job discipline;
// see the ADR-0047 implementation session for the exact SELECTs) —
// NOT synthetic fixtures. Each case pins the decoder against actual
// on-chain data around ledger 40,000,000 (2022-03-12), long before
// the P23 boundary. decode_test.go covers the synthetic edge cases
// this real data doesn't happen to exercise (native-asset payment,
// malformed-amount defensive path, etc).
func TestRealBytes_payment_success(t *testing.T) {
	// ledger 40000000, tx 0761af68c3cd6f5fc9a94b5ffca8129ccd6a3a6faa515fe1169706a8521ba248, op_index 2.
	// source_account (from) = GA2QXW7YFAIR35LGKM2TDCQQZFR33XJCWF4N6SMRLOKX3HL76JKKPA62
	// decodes to: dest GBWOI5542H5LPMFIIZV3CLYZ2EE7I5JPA6VITN4OUU7OBNCKI4T4HMJP,
	// asset XXA-GC4HS4CQCZULIOTGLLPGRAAMSBDLFRR6Y7HCUQG66LNQDISXKIXXADIM, amount 10972566552.
	const (
		bodyB64   = "AAAAAQAAAABs5He80fq3sKhGa7EvGdEJ9HUvB6qJt46lPuC0SkcnwwAAAAFYWEEAAAAAALh5cFAWaLQ6ZlreaIAMkEayxj7HzipA3vLbAaJXUi9wAAAAAo4EFBg="
		resultB64 = "AAAAAAAAAAEAAAAA"
	)
	movements := decodeRealBytes(t, bodyB64, resultB64, 40_000_000,
		"0761af68c3cd6f5fc9a94b5ffca8129ccd6a3a6faa515fe1169706a8521ba248", 2,
		"GA2QXW7YFAIR35LGKM2TDCQQZFR33XJCWF4N6SMRLOKX3HL76JKKPA62",
		time.Date(2022, 3, 12, 19, 32, 55, 0, time.UTC))

	if len(movements) != 1 {
		t.Fatalf("got %d movements, want 1", len(movements))
	}
	want := Movement{
		Kind:            KindPayment,
		Provenance:      ProvenanceClassicDerived,
		Ledger:          40_000_000,
		LedgerCloseTime: time.Date(2022, 3, 12, 19, 32, 55, 0, time.UTC),
		TxHash:          "0761af68c3cd6f5fc9a94b5ffca8129ccd6a3a6faa515fe1169706a8521ba248",
		OpIndex:         2,
		LegIndex:        0,
		Asset:           "XXA-GC4HS4CQCZULIOTGLLPGRAAMSBDLFRR6Y7HCUQG66LNQDISXKIXXADIM",
		FromAddress:     "GA2QXW7YFAIR35LGKM2TDCQQZFR33XJCWF4N6SMRLOKX3HL76JKKPA62",
		ToAddress:       "GBWOI5542H5LPMFIIZV3CLYZ2EE7I5JPA6VITN4OUU7OBNCKI4T4HMJP",
	}
	assertMovementEqual(t, movements[0], want, "10972566552")
}

func TestRealBytes_payment_success_secondLeg(t *testing.T) {
	// Same tx as above, op_index 3 — a second, unrelated payment in the
	// same transaction (source account overrides the tx source), USDC.
	const (
		bodyB64   = "AAAAAQAAAACGM7BaSUMQn9EXPK0RmuUNAVBUgmUpCQKCpCXwq5gaBAAAAAFVU0RDAAAAADuZETgO/piLoKiQDrHP5E82b32+lGvtB3JA9/Yk3xXFAAAAAC7HpUY="
		resultB64 = "AAAAAAAAAAEAAAAA"
	)
	movements := decodeRealBytes(t, bodyB64, resultB64, 40_000_000,
		"0761af68c3cd6f5fc9a94b5ffca8129ccd6a3a6faa515fe1169706a8521ba248", 3,
		"GAVGTMN7MYHPVF7S363PSZCTGMSQ64PEOHRDK7AVWO2VGRHV7U2SD3OA",
		time.Date(2022, 3, 12, 19, 32, 55, 0, time.UTC))

	if len(movements) != 1 {
		t.Fatalf("got %d movements, want 1", len(movements))
	}
	want := Movement{
		Kind:            KindPayment,
		Provenance:      ProvenanceClassicDerived,
		Ledger:          40_000_000,
		LedgerCloseTime: time.Date(2022, 3, 12, 19, 32, 55, 0, time.UTC),
		TxHash:          "0761af68c3cd6f5fc9a94b5ffca8129ccd6a3a6faa515fe1169706a8521ba248",
		OpIndex:         3,
		LegIndex:        0,
		Asset:           "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
		FromAddress:     "GAVGTMN7MYHPVF7S363PSZCTGMSQ64PEOHRDK7AVWO2VGRHV7U2SD3OA",
		ToAddress:       "GCDDHMC2JFBRBH6RC46K2EM24UGQCUCUQJSSSCICQKSCL4FLTANAIK74",
	}
	assertMovementEqual(t, movements[0], want, "784835910")
}

func TestRealBytes_payment_success_nativeAsset(t *testing.T) {
	// ledger 40000000, tx 17eb3729b315c23eb8bec282a4d00ec4d095cea5969a374576d8b33763bad4e3, op_index 0.
	// A tiny (10-stroop) native XLM payment — real coverage of the
	// "native" asset shape end to end.
	const (
		bodyB64   = "AAAAAQAAAABjDz9pTvtUpLGFEobNwdCiPL/fSI9lFaS0EGC05did6QAAAAAAAAAAAAAACg=="
		resultB64 = "AAAAAAAAAAEAAAAA"
	)
	movements := decodeRealBytes(t, bodyB64, resultB64, 40_000_000,
		"17eb3729b315c23eb8bec282a4d00ec4d095cea5969a374576d8b33763bad4e3", 0,
		"GD7OWGDKSNWAEHROWGYIWNDYZSG54EPARUFQQG2C4UEHHZ6WFRJJR3ZA",
		time.Date(2022, 3, 12, 19, 32, 55, 0, time.UTC))

	if len(movements) != 1 {
		t.Fatalf("got %d movements, want 1", len(movements))
	}
	want := Movement{
		Kind:            KindPayment,
		Provenance:      ProvenanceClassicDerived,
		Ledger:          40_000_000,
		LedgerCloseTime: time.Date(2022, 3, 12, 19, 32, 55, 0, time.UTC),
		TxHash:          "17eb3729b315c23eb8bec282a4d00ec4d095cea5969a374576d8b33763bad4e3",
		OpIndex:         0,
		LegIndex:        0,
		Asset:           "native",
		FromAddress:     "GD7OWGDKSNWAEHROWGYIWNDYZSG54EPARUFQQG2C4UEHHZ6WFRJJR3ZA",
		ToAddress:       "GBRQ6P3JJ35VJJFRQUJINTOB2CRDZP67JCHWKFNEWQIGBNHF3CO6SJUX",
	}
	assertMovementEqual(t, movements[0], want, "10")
}

func TestRealBytes_createAccount_success(t *testing.T) {
	// ledger 40000000, tx 79a7f7bca0d9a520c32c3e03ea2a4a33ecee696ef656ca4d4f7be3635d3ee9a6, op_index 0.
	const (
		bodyB64   = "AAAAAAAAAABEhNo2pKcX+rr5g64sjcqJtM316fADqGjbpQ4fEgr+uAAAAACi2GcH"
		resultB64 = "AAAAAAAAAAAAAAAA"
	)
	movements := decodeRealBytes(t, bodyB64, resultB64, 40_000_000,
		"79a7f7bca0d9a520c32c3e03ea2a4a33ecee696ef656ca4d4f7be3635d3ee9a6", 0,
		"GBW5AENWI5PFJRYEIAIRYDB62MVEHDYHEBXKFN3TI64RSL2L6GYOYFG4",
		time.Date(2022, 3, 12, 19, 32, 55, 0, time.UTC))

	if len(movements) != 1 {
		t.Fatalf("got %d movements, want 1", len(movements))
	}
	want := Movement{
		Kind:            KindCreateAccount,
		Provenance:      ProvenanceClassicDerived,
		Ledger:          40_000_000,
		LedgerCloseTime: time.Date(2022, 3, 12, 19, 32, 55, 0, time.UTC),
		TxHash:          "79a7f7bca0d9a520c32c3e03ea2a4a33ecee696ef656ca4d4f7be3635d3ee9a6",
		OpIndex:         0,
		LegIndex:        0,
		Asset:           "native",
		FromAddress:     "GBW5AENWI5PFJRYEIAIRYDB62MVEHDYHEBXKFN3TI64RSL2L6GYOYFG4",
		ToAddress:       "GBCIJWRWUSTRP6V27GB24LENZKE3JTPV5HYAHKDI3OSQ4HYSBL7LQWBZ",
	}
	assertMovementEqual(t, movements[0], want, "2732091143")
}

// TestRealBytes_payment_failed_sourceNoAccount is the path-negative
// case: a REAL failed payment (ledger 40035852, tx
// 8cca530c735f7bff37a587db8c48082739c12f06f6d3a9fa0ec0f4771e1dbbd1,
// op_index 2) whose outer OperationResultCode is opNO_ACCOUNT (-2,
// "source account was not found") — the op never reached its own
// PaymentResult union at all. Must decode to ZERO movements, not an
// error: this is routine on-chain failure, indistinguishable at the
// decoder layer from "offer didn't cross" in SDEX's failed-op tests.
func TestRealBytes_payment_failed_sourceNoAccount(t *testing.T) {
	const (
		bodyB64   = "AAAAAQAAAAA3mZB7bnHFoxwyZpTTMRdQvzdJKQrlJgLjpW6jCNCEtwAAAAJSQU5ESTEAAAAAAAAAAAAAjNPgGB5OHDkXLDlbk4XOaCVSZtwsVbgQoA84G19p0YkAAAAAAAAAAQ=="
		resultB64 = "/////g=="
	)
	movements := decodeRealBytes(t, bodyB64, resultB64, 40_035_852,
		"8cca530c735f7bff37a587db8c48082739c12f06f6d3a9fa0ec0f4771e1dbbd1", 2,
		"GCGNHYAYDZHBYOIXFQ4VXE4FZZUCKUTG3QWFLOAQUAHTQG27NHIYSU3D",
		time.Date(2022, 3, 15, 7, 16, 7, 0, time.UTC))

	if len(movements) != 0 {
		t.Fatalf("got %d movements from a failed (opNO_ACCOUNT) payment, want 0: %+v", len(movements), movements)
	}
}

// TestRealBytes_resultCodeIsOpNoAccount pins the exact outer result
// code the "failed" fixture above decodes to, independent of this
// package's Decode logic — a canary against a future go-stellar-sdk
// upgrade silently renumbering the enum.
func TestRealBytes_resultCodeIsOpNoAccount(t *testing.T) {
	var res xdr.OperationResult
	if err := xdr.SafeUnmarshalBase64("/////g==", &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if res.Code != xdr.OperationResultCodeOpNoAccount {
		t.Errorf("result code = %s, want OperationResultCodeOpNoAccount", res.Code)
	}
}

// ─── Phase 2: PathPaymentStrictReceive / PathPaymentStrictSend ────

// TestRealBytes_pathPaymentStrictReceive_singleHop is a real
// pre-P23 mainnet PathPaymentStrictReceive: source spends native
// through ONE order-book offer to deliver an exact SONY amount.
// Confirms the destination leg (Asset/Amount) is read from the
// result's Last, not the body's DestAsset/DestAmount (though they
// agree here, as they always should for a successful op), and that
// the source leg is the single offer's AmountBought.
func TestRealBytes_pathPaymentStrictReceive_singleHop(t *testing.T) {
	// ledger 40000001, tx 32696e52909644d21ff1e36afbb6379cb8a555cbf658e8c7ae66a0c9c5b417b0, op_index 0.
	// source GALHA4OVKNZ555C7VJFIXVXM4F2R7PPJNO7IZHIYE2SMKFYY5U7V254G pays
	// native (SendMax=12120000) through one order-book offer to receive
	// exactly 900000000000000 stroops of SONY at its own account.
	const (
		bodyB64   = "AAAAAgAAAAAAAAAAALjvwAAAAAAWcHHVU3Pe9F+qSovW7OF1H73pa76MnRgmpMUXGO0/XQAAAAFTT05ZAAAAALsLcXbIOISuH+pMVlQ3U/ziOgkUcfkQQHnKR+iFALP4AAMyi5RMQAAAAAAA"
		resultB64 = "AAAAAAAAAAIAAAAAAAAAAQAAAAEAAAAAtBJMX3t5P0oPc7eVIwj3MXMLwgQZNa6roDUwTYg44x8AAAAAOJxqdAAAAAFTT05ZAAAAALsLcXbIOISuH+pMVlQ3U/ziOgkUcfkQQHnKR+iFALP4AAMyi5RMQAAAAAAAAAAAAAC3GwAAAAAAFnBx1VNz3vRfqkqL1uzhdR+96Wu+jJ0YJqTFFxjtP10AAAABU09OWQAAAAC7C3F2yDiErh/qTFZUN1P84joJFHH5EEB5ykfohQCz+AADMouUTEAA"
	)
	movements := decodeRealBytes(t, bodyB64, resultB64, 40_000_001,
		"32696e52909644d21ff1e36afbb6379cb8a555cbf658e8c7ae66a0c9c5b417b0", 0,
		"GALHA4OVKNZ555C7VJFIXVXM4F2R7PPJNO7IZHIYE2SMKFYY5U7V254G",
		time.Date(2022, 3, 12, 19, 33, 2, 0, time.UTC))

	if len(movements) != 1 {
		t.Fatalf("got %d movements, want 1", len(movements))
	}
	m := movements[0]
	if m.Kind != KindPathPayment {
		t.Errorf("Kind = %q, want %q", m.Kind, KindPathPayment)
	}
	wantDestAsset := "SONY-GC5QW4LWZA4IJLQ75JGFMVBXKP6OEOQJCRY7SECAPHFEP2EFACZ7QZW5"
	if m.Asset != wantDestAsset || m.Amount.String() != "900000000000000" {
		t.Errorf("dest leg = %s %s, want %s 900000000000000", m.Amount.String(), m.Asset, wantDestAsset)
	}
	if m.FromAddress != "GALHA4OVKNZ555C7VJFIXVXM4F2R7PPJNO7IZHIYE2SMKFYY5U7V254G" {
		t.Errorf("FromAddress = %q", m.FromAddress)
	}
	if m.ToAddress != "GALHA4OVKNZ555C7VJFIXVXM4F2R7PPJNO7IZHIYE2SMKFYY5U7V254G" {
		t.Errorf("ToAddress = %q, want same as FromAddress (self-pay via path)", m.ToAddress)
	}
	if m.Attributes["send_asset"] != "native" || m.Attributes["send_amount"] != "12000000" {
		t.Errorf("Attributes = %+v, want send_asset=native send_amount=12000000", m.Attributes)
	}
}

// TestRealBytes_pathPaymentStrictReceive_twoHop is a real pre-P23
// mainnet PathPaymentStrictReceive routed native→SHIB→native (an
// arbitrage-shaped path with SendAsset==DestAsset but two real
// hops) — the fixture pathPaymentStrictReceiveSourceAmount's doc
// comment cites directly. Confirms the derivation sums ONLY the
// first hop (Offers[0], AssetBought==native) and stops before the
// second hop (Offers[1], AssetBought==SHIB) even though both offers
// are present in the result.
func TestRealBytes_pathPaymentStrictReceive_twoHop(t *testing.T) {
	// ledger 40000003, tx 49203432aa0b5da1a3f621e093cdde6a116064f969efe6f3b6162691d1afb84b, op_index 0.
	const (
		bodyB64   = "AAAAAgAAAAAAAAAABPtnBgAAAAAYyCYed5ULPCCZ1wtggxYUtoK6Hu5uyhKp0+DNkFyZ1gAAAAAAAAAABPtuGAAAAAEAAAABU0hJQgAAAABa7upQ7YJtt/jfTu+F1mmJZbUiSrjeJ9cNliPnYMns5w=="
		resultB64 = "AAAAAAAAAAIAAAAAAAAAAgAAAAEAAAAAbaHTfp0wRYC9BeJP51OYornphxPI+aozmlv3trm4KbAAAAAAOKJmcAAAAAFTSElCAAAAAFru6lDtgm23+N9O74XWaYlltSJKuN4n1w2WI+dgyeznAAAAjC6sEZoAAAAAAAAAAAT7J2kAAAACd3xyt7p6rXDgES6e0Vie5PU5pKZvgUqA2rAbTcL4mHEAAAAAAAAAAAT7bhgAAAABU0hJQgAAAABa7upQ7YJtt/jfTu+F1mmJZbUiSrjeJ9cNliPnYMns5wAAAIwurBGaAAAAABjIJh53lQs8IJnXC2CDFhS2groe7m7KEqnT4M2QXJnWAAAAAAAAAAAE+24Y"
	)
	movements := decodeRealBytes(t, bodyB64, resultB64, 40_000_003,
		"49203432aa0b5da1a3f621e093cdde6a116064f969efe6f3b6162691d1afb84b", 0,
		"GAMMQJQ6O6KQWPBATHLQWYEDCYKLNAV2D3XG5SQSVHJ6BTMQLSM5MSLE",
		time.Date(2022, 3, 12, 19, 33, 16, 0, time.UTC))

	if len(movements) != 1 {
		t.Fatalf("got %d movements, want 1", len(movements))
	}
	m := movements[0]
	if m.Asset != "native" || m.Amount.String() != "83586584" {
		t.Errorf("dest leg = %s %s, want native 83586584", m.Amount.String(), m.Asset)
	}
	// The hop-0-only source amount (83568489), NOT SendMax (83584774)
	// and NOT the hop-1 leg amount (83586584) — this is the whole
	// point of the fixture.
	if m.Attributes["send_asset"] != "native" || m.Attributes["send_amount"] != "83568489" {
		t.Errorf("Attributes = %+v, want send_asset=native send_amount=83568489", m.Attributes)
	}
}

// TestRealBytes_pathPaymentStrictSend_success is a real pre-P23
// mainnet PathPaymentStrictSend: exact aiXDOGE SendAmount from the
// body, AQUA delivered per the result's Last (below DestMin's floor
// check, already enforced by core — Last.Amount is what actually
// landed).
func TestRealBytes_pathPaymentStrictSend_success(t *testing.T) {
	// ledger 40000000, tx 04f7f85101dd3d9c3d370f65ddeb619b93058f5d8d55d1499932fdf8747a6a40, op_index 2.
	const (
		bodyB64   = "AAAADQAAAAJhaVhET0dFAAAAAAAAAAAAHmZ99WHIvNYnad6AHqEYtIx8rynNCdIrpMMan93+ee8AAAAAC+vCAAAAAAAbwSApPmwboWhG14u1quvJh4f0t3hW09pLOIa1MPeGVwAAAAFBUVVBAAAAAFuULlOsM8j9CoDMfBsahdfYOKnEGXeq0Ys68Ff44z3wAAAAAAAAbpIAAAABAAAAAA=="
		resultB64 = "AAAAAAAAAA0AAAAAAAAAAgAAAAEAAAAAQJzSfng2B5F/ARo1w+R7fYQ1nuZE0ILRfdDZNwTNIh8AAAAAOK4wUAAAAAAAAAAAAAAETAAAAAJhaVhET0dFAAAAAAAAAAAAHmZ99WHIvNYnad6AHqEYtIx8rynNCdIrpMMan93+ee8AAAAAC+vCAAAAAAEAAAAAQ5f/457bn13BXKa5Mccm5n80F2Y9HiOh0k2x4c4FIV4AAAAAOK47NAAAAAFBUVVBAAAAAFuULlOsM8j9CoDMfBsahdfYOKnEGXeq0Ys68Ff44z3wAAAAAAAA+DkAAAAAAAAAAAAABEwAAAAAG8EgKT5sG6FoRteLtarryYeH9Ld4VtPaSziGtTD3hlcAAAABQVFVQQAAAABblC5TrDPI/QqAzHwbGoXX2DipxBl3qtGLOvBX+OM98AAAAAAAAPg5"
	)
	movements := decodeRealBytes(t, bodyB64, resultB64, 40_000_000,
		"04f7f85101dd3d9c3d370f65ddeb619b93058f5d8d55d1499932fdf8747a6a40", 2,
		"GAN4CIBJHZWBXILII3LYXNNK5PEYPB7UW54FNU62JM4INNJQ66DFPWWG",
		time.Date(2022, 3, 12, 19, 32, 55, 0, time.UTC))

	if len(movements) != 1 {
		t.Fatalf("got %d movements, want 1", len(movements))
	}
	m := movements[0]
	wantDestAsset := "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
	if m.Asset != wantDestAsset || m.Amount.String() != "63545" {
		t.Errorf("dest leg = %s %s, want %s 63545", m.Amount.String(), m.Asset, wantDestAsset)
	}
	wantSendAsset := "aiXDOGE-GAPGM7PVMHELZVRHNHPIAHVBDC2IY7FPFHGQTURLUTBRVH657Z466RAI"
	if m.Attributes["send_asset"] != wantSendAsset || m.Attributes["send_amount"] != "200000000" {
		t.Errorf("Attributes = %+v, want send_asset=%s send_amount=200000000", m.Attributes, wantSendAsset)
	}
}

// ─── Phase 3: ClaimableBalance create/claim/clawback + Clawback ───

// TestRealBytes_createClaimableBalance_success is a real pre-P23
// mainnet CreateClaimableBalance: GALA funded into escrow with two
// claimants. Pins the generated BalanceId (from the RESULT, not
// derivable from the body) and the claimants list.
func TestRealBytes_createClaimableBalance_success(t *testing.T) {
	// ledger 40000000, tx 05fd6eea40b036204d6817d4ba945663e3071e2331385de78f38a9b9bed661cc, op_index 0.
	const (
		bodyB64   = "AAAADgAAAAFHQUxBAAAAADV6FeHCtmgz8V4u/kE1liwByrbw7SK8T5xatMG3j16DAAAAAAAF57gAAAACAAAAAAAAAACEKCUFXS4fGHiDDkqG2fsp03cR+Rd9bRJKybyy/rBgjgAAAAMAAAABAAAABAAAAABiLPP5AAAAAAAAAAA+CSgxOgl5fsc6se+nt2OaXHhgjaBK88g+wvrbZ7xZUgAAAAUAAAAAAAk6gA=="
		resultB64 = "AAAAAAAAAA4AAAAAAAAAAAZiRenTfqxyI9zYHNhROvZuqvaj0+hcn77xY+YUzgCd"
	)
	movements := decodeRealBytes(t, bodyB64, resultB64, 40_000_000,
		"05fd6eea40b036204d6817d4ba945663e3071e2331385de78f38a9b9bed661cc", 0,
		"GCCCQJIFLUXB6GDYQMHEVBWZ7MU5G5YR7ELX23ISJLE3ZMX6WBQI4HFW",
		time.Date(2022, 3, 12, 19, 32, 55, 0, time.UTC))

	if len(movements) != 1 {
		t.Fatalf("got %d movements, want 1", len(movements))
	}
	m := movements[0]
	if m.Kind != KindClaimableBalanceCreate {
		t.Errorf("Kind = %q, want %q", m.Kind, KindClaimableBalanceCreate)
	}
	wantAsset := "GALA-GA2XUFPBYK3GQM7RLYXP4QJVSYWADSVW6DWSFPCPTRNLJQNXR5PIGALA"
	if m.Asset != wantAsset || m.Amount.String() != "387000" {
		t.Errorf("asset/amount = %s %s, want %s 387000", m.Amount.String(), m.Asset, wantAsset)
	}
	if m.FromAddress != "GCCCQJIFLUXB6GDYQMHEVBWZ7MU5G5YR7ELX23ISJLE3ZMX6WBQI4HFW" || m.ToAddress != "" {
		t.Errorf("From/To = %q/%q, want the source / empty", m.FromAddress, m.ToAddress)
	}
	wantID := "066245e9d37eac7223dcd81cd8513af66eaaf6a3d3e85c9fbef163e614ce009d"
	if m.Attributes["balance_id"] != wantID {
		t.Errorf("balance_id = %v, want %v", m.Attributes["balance_id"], wantID)
	}
	claimants, ok := m.Attributes["claimants"].([]string)
	if !ok || len(claimants) != 2 {
		t.Fatalf("claimants = %v, want a 2-element []string", m.Attributes["claimants"])
	}
	if claimants[0] != "GCCCQJIFLUXB6GDYQMHEVBWZ7MU5G5YR7ELX23ISJLE3ZMX6WBQI4HFW" ||
		claimants[1] != "GA7ASKBRHIEXS7WHHKY67J5XMONFY6DARWQEV46IH3BPVW3HXRMVFIOP" {
		t.Errorf("claimants = %v", claimants)
	}
}

// TestRealBytes_claimClaimableBalance_success is a real pre-P23
// mainnet ClaimClaimableBalance. Its create is NOT in this test's
// window, so the in-run index can't resolve it — this decodes to
// zero movements plus one PendingClaimableBalanceRef, exercising the
// exact "create outside this run" path production traffic hits
// constantly (§ decode.go's Decoder doc).
func TestRealBytes_claimClaimableBalance_unresolved(t *testing.T) {
	// ledger 40000000, tx 04f7f85101dd3d9c3d370f65ddeb619b93058f5d8d55d1499932fdf8747a6a40, op_index 1.
	const (
		bodyB64   = "AAAADwAAAAD5qm+e1LhKIbwH2WwTRW1z21a8SyVBB6JhPsbYJ17Asg=="
		resultB64 = "AAAAAAAAAA8AAAAA"
	)
	d := NewDecoder()
	var body xdr.OperationBody
	if err := xdr.SafeUnmarshalBase64(bodyB64, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	var result xdr.OperationResult
	if err := xdr.SafeUnmarshalBase64(resultB64, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	op := xdr.Operation{Body: body}
	outs, err := d.Decode(dispatcher.OpContext{
		Ledger:   40_000_000,
		TxHash:   "04f7f85101dd3d9c3d370f65ddeb619b93058f5d8d55d1499932fdf8747a6a40",
		TxSource: "GAN4CIBJHZWBXILII3LYXNNK5PEYPB7UW54FNU62JM4INNJQ66DFPWWG",
		OpIndex:  1, Op: op, OpResult: result,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(outs) != 0 {
		t.Fatalf("got %d movements, want 0 (unresolved create)", len(outs))
	}
	pending := d.TakePendingClaimableBalances()
	if len(pending) != 1 {
		t.Fatalf("got %d pending refs, want 1", len(pending))
	}
	if pending[0].Kind != KindClaimableBalanceClaim {
		t.Errorf("Kind = %q, want %q", pending[0].Kind, KindClaimableBalanceClaim)
	}
	if pending[0].ToAddress != "GAN4CIBJHZWBXILII3LYXNNK5PEYPB7UW54FNU62JM4INNJQ66DFPWWG" {
		t.Errorf("ToAddress = %q", pending[0].ToAddress)
	}
	wantID := "f9aa6f9ed4b84a21bc07d96c13456d73db56bc4b254107a2613ec6d8275ec0b2"
	if pending[0].BalanceIDHex != wantID {
		t.Errorf("BalanceIDHex = %q, want %q", pending[0].BalanceIDHex, wantID)
	}
}

// TestRealBytes_clawback_success is a real pre-P23 mainnet Clawback:
// the holder (op.From) and the issuer (the op's resolved source
// account) are DIFFERENT real addresses — confirms FromAddress /
// ToAddress are not accidentally swapped or both set to the same
// value.
func TestRealBytes_clawback_success(t *testing.T) {
	// ledger 45000001, tx 9a3e58558762d50ba9b90a0d1ce934325fc9f1af1c2b4e646e1a4368890bfe84, op_index 2.
	const (
		bodyB64   = "AAAAEwAAAAJJcmFxaURpbmFyAAAAAAAAQqpSc3O4BsnF00z3DCgHZslFSsG9q8vhnVv8SUCYlBIAAAAAixcHcr3R/h+JDHbDxqHVbjXTcvfNj79TgUAVVq6QOfwAAAAFpDh1gA=="
		resultB64 = "AAAAAAAAABMAAAAA"
	)
	movements := decodeRealBytes(t, bodyB64, resultB64, 45_000_001,
		"9a3e58558762d50ba9b90a0d1ce934325fc9f1af1c2b4e646e1a4368890bfe84", 2,
		"GBBKUUTTOO4ANSOF2NGPODBIA5TMSRKKYG62XS7BTVN7YSKATCKBFTCW",
		time.Date(2023, 2, 18, 7, 3, 24, 0, time.UTC))

	if len(movements) != 1 {
		t.Fatalf("got %d movements, want 1", len(movements))
	}
	m := movements[0]
	if m.Kind != KindClawback {
		t.Errorf("Kind = %q, want %q", m.Kind, KindClawback)
	}
	if m.Amount.String() != "24230000000" {
		t.Errorf("Amount = %q, want 24230000000", m.Amount.String())
	}
	if m.FromAddress != "GCFROB3SXXI74H4JBR3MHRVB2VXDLU3S67GY7P2TQFABKVVOSA47Z6CS" {
		t.Errorf("FromAddress (holder) = %q", m.FromAddress)
	}
	if m.ToAddress != "GBBKUUTTOO4ANSOF2NGPODBIA5TMSRKKYG62XS7BTVN7YSKATCKBFTCW" {
		t.Errorf("ToAddress (issuer) = %q", m.ToAddress)
	}
}

// TestRealBytes_clawbackClaimableBalance_failed pins a REAL failed
// ClawbackClaimableBalance (ClawbackClaimableBalanceNotClawbackEnabled)
// — zero movements, no pending ref recorded (the op never resolved
// to a real value-moving attempt, so there's nothing to correlate).
func TestRealBytes_clawbackClaimableBalance_failed(t *testing.T) {
	// ledger 40086192, tx 5a1b65b21d6a2db081eb57e2e3075089553bdd36ad78f7dc8fdacaaff5b2ed2b, op_index 0.
	const (
		bodyB64   = "AAAAFAAAAAAndivT9qSWT2XUT+ui1Gnkzi+2ve7uG05Gx6EErpxlWw=="
		resultB64 = "AAAAAAAAABT////9"
	)
	d := NewDecoder()
	var body xdr.OperationBody
	if err := xdr.SafeUnmarshalBase64(bodyB64, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	var result xdr.OperationResult
	if err := xdr.SafeUnmarshalBase64(resultB64, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	outs, err := d.Decode(dispatcher.OpContext{
		Ledger: 40_086_192, TxHash: "5a1b65b21d6a2db081eb57e2e3075089553bdd36ad78f7dc8fdacaaff5b2ed2b",
		TxSource: "GAOGRZQLNAD7GJAEBJ6AC6LA3X5I3Y5DP2LJWZWFT6KLLQU3YGATY5TF", OpIndex: 0,
		Op: xdr.Operation{Body: body}, OpResult: result,
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(outs) != 0 {
		t.Errorf("got %d movements from a failed clawback-claimable-balance, want 0", len(outs))
	}
	if pending := d.TakePendingClaimableBalances(); len(pending) != 0 {
		t.Errorf("got %d pending refs from a failed op, want 0", len(pending))
	}
}

// ─── Phase 4 (op-only half): AccountMerge ──────────────────────────

// TestRealBytes_accountMerge_success is a real pre-P23 mainnet
// AccountMerge: pins the exact XLM amount from
// AccountMergeResult.SourceAccountBalance (249999900 — NOT derivable
// from the body, which carries only the destination).
func TestRealBytes_accountMerge_success(t *testing.T) {
	// ledger 40000173, tx 8503c256346c2cd6c6864d4c33f8e502f87ec3f1413688210017563e4e0b2117, op_index 0.
	const (
		bodyB64   = "AAAACAAAAAA0/LLInLnVygX6rN/I+sRIih1i5JrUYv0elzaRi/8EBg=="
		resultB64 = "AAAAAAAAAAgAAAAAAAAAAA7mshw="
	)
	movements := decodeRealBytes(t, bodyB64, resultB64, 40_000_173,
		"8503c256346c2cd6c6864d4c33f8e502f87ec3f1413688210017563e4e0b2117", 0,
		"GDICQTPPKCM7WABLJS2UGZIYF3BFET4MGQLN2GB4BKDKAGSXYYAPVO46",
		time.Date(2022, 3, 12, 19, 50, 15, 0, time.UTC))

	if len(movements) != 1 {
		t.Fatalf("got %d movements, want 1", len(movements))
	}
	m := movements[0]
	if m.Kind != KindAccountMerge {
		t.Errorf("Kind = %q, want %q", m.Kind, KindAccountMerge)
	}
	if m.Asset != "native" || m.Amount.String() != "249999900" {
		t.Errorf("asset/amount = %s %s, want native 249999900", m.Amount.String(), m.Asset)
	}
	if m.FromAddress != "GDICQTPPKCM7WABLJS2UGZIYF3BFET4MGQLN2GB4BKDKAGSXYYAPVO46" {
		t.Errorf("FromAddress = %q", m.FromAddress)
	}
	if m.ToAddress != "GA2PZMWITS45LSQF7KWN7SH2YREIUHLC4SNNIYX5D2LTNEML74CANJMO" {
		t.Errorf("ToAddress = %q", m.ToAddress)
	}
}

// ─── Phase 4 (entry-changes half): LiquidityPoolDeposit/Withdraw ──
//
// Real pre-P23 mainnet LP deposit/withdraw op bodies (ledger
// ~50,000,000, well below the P23 boundary AND below
// ledger_entry_changes' current per-op fidelity floor of
// ~61,996,000 — research §3.2). This is exactly the "fidelity
// absent" era every classic-movements-backfill invocation runs in
// TODAY: StreamEntryChanges returns zero rows for these ops, not
// because nothing happened but because Phase 0's ch-backfill hasn't
// reached this range yet. Confirming that DecodeLiquidityPoolOp
// returns ErrEntryChangesUnavailable (never an error, never a
// guessed amount) against REAL op bytes — not just a synthetic empty
// slice — is the strongest evidence the skip-honestly path is wired
// correctly end to end.

func TestRealBytes_liquidityPoolDeposit_entryChangesUnavailable(t *testing.T) {
	// ledger 50000031, tx 294abbd9b62ed717abb6e7008cf86ba7b99c9e5a6a20444350fc418bf2d00071, op_index 0.
	const bodyB64 = "AAAAFkH5xtzrmGR6mhOv/Bea3A1DOfxi8oi0KKH3nNu93YOAAAAAABiEdFwAAAAAAQJiEwAJLHkAAGGoB0/PTwBMS0A="
	const resultB64 = "AAAAAAAAABYAAAAA"

	var body xdr.OperationBody
	if err := xdr.SafeUnmarshalBase64(bodyB64, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	var result xdr.OperationResult
	if err := xdr.SafeUnmarshalBase64(resultB64, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if body.Type != xdr.OperationTypeLiquidityPoolDeposit {
		t.Fatalf("op type = %s, want OperationTypeLiquidityPoolDeposit", body.Type)
	}

	_, err := DecodeLiquidityPoolOp(50_000_031, time.Date(2024, 1, 20, 9, 12, 38, 0, time.UTC),
		"294abbd9b62ed717abb6e7008cf86ba7b99c9e5a6a20444350fc418bf2d00071", 0,
		"GDCTMYJBFGEEBN75GW3ECN2V3QS4LFLIIRGRXYSJJQMMAIIPDCVCOU3T",
		xdr.Operation{Body: body}, result, nil) // nil changes: what StreamEntryChanges actually returns today for this ledger.
	if !errors.Is(err, ErrEntryChangesUnavailable) {
		t.Errorf("err = %v, want errors.Is(err, ErrEntryChangesUnavailable) — this is the real pre-fidelity-floor era every backfill invocation runs in today", err)
	}
}

func TestRealBytes_liquidityPoolWithdraw_entryChangesUnavailable(t *testing.T) {
	// ledger 50000096, tx 9985aa0dce7f8e92b5f8d9bba385375eb3bb19e521c6be33e67420d99cd9ab9f, op_index 0.
	const bodyB64 = "AAAAF0H5xtzrmGR6mhOv/Bea3A1DOfxi8oi0KKH3nNu93YOAAAAAAAAK1OoAAAAAAJcTvgAAAAAABjgq"
	const resultB64 = "AAAAAAAAABcAAAAA"

	var body xdr.OperationBody
	if err := xdr.SafeUnmarshalBase64(bodyB64, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	var result xdr.OperationResult
	if err := xdr.SafeUnmarshalBase64(resultB64, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if body.Type != xdr.OperationTypeLiquidityPoolWithdraw {
		t.Fatalf("op type = %s, want OperationTypeLiquidityPoolWithdraw", body.Type)
	}

	_, err := DecodeLiquidityPoolOp(50_000_096, time.Date(2024, 1, 20, 9, 18, 50, 0, time.UTC),
		"9985aa0dce7f8e92b5f8d9bba385375eb3bb19e521c6be33e67420d99cd9ab9f", 0,
		"GDJTIX3XYRZFQZCES5TI6Z5ZWZZFHUP7KQVTTIFM2Y6M7M6L6RM2WR4B",
		xdr.Operation{Body: body}, result, nil)
	if !errors.Is(err, ErrEntryChangesUnavailable) {
		t.Errorf("err = %v, want errors.Is(err, ErrEntryChangesUnavailable)", err)
	}
}

// ─── helpers ──────────────────────────────────────────────────────

// decodeRealBytes unmarshals real op body/result XDR (base64, exactly
// as stored in stellar.operations.body_xdr / operation_results.result_xdr)
// and runs it through the production Decoder, exactly the way
// clickhouse.StreamClassicOps + the classic-movements-backfill
// command's consumer loop would.
func decodeRealBytes(t *testing.T, bodyB64, resultB64 string, ledger uint32, txHash string, opIndex uint32, fromAddr string, closedAt time.Time) []Movement {
	t.Helper()
	var body xdr.OperationBody
	if err := xdr.SafeUnmarshalBase64(bodyB64, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	var result xdr.OperationResult
	if err := xdr.SafeUnmarshalBase64(resultB64, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	op := xdr.Operation{Body: body}

	if !NewDecoder().Matches(op) {
		t.Fatalf("Matches() = false for op type %s — real fixture is outside Phase 1's scope", body.Type)
	}

	outs, err := NewDecoder().Decode(dispatcher.OpContext{
		Ledger:   ledger,
		ClosedAt: closedAt,
		TxHash:   txHash,
		TxSource: fromAddr,
		OpIndex:  int(opIndex),
		Op:       op,
		OpResult: result,
	})
	if err != nil {
		if errors.Is(err, ErrUnsupportedOpType) {
			t.Fatalf("Decode: unexpected ErrUnsupportedOpType for op type %s", body.Type)
		}
		t.Fatalf("Decode: %v", err)
	}
	movements := make([]Movement, 0, len(outs))
	for _, ev := range outs {
		me, ok := ev.(MovementEvent)
		if !ok {
			t.Fatalf("output is %T, want MovementEvent", ev)
		}
		movements = append(movements, me.Movement)
	}
	return movements
}

// assertMovementEqual compares every field of got against want plus
// the expected decimal-string amount (canonical.Amount has no simple
// == comparison, so the amount check is separate).
func assertMovementEqual(t *testing.T, got, want Movement, wantAmount string) {
	t.Helper()
	if got.Kind != want.Kind {
		t.Errorf("Kind = %q, want %q", got.Kind, want.Kind)
	}
	if got.Provenance != want.Provenance {
		t.Errorf("Provenance = %q, want %q", got.Provenance, want.Provenance)
	}
	if got.Ledger != want.Ledger {
		t.Errorf("Ledger = %d, want %d", got.Ledger, want.Ledger)
	}
	if !got.LedgerCloseTime.Equal(want.LedgerCloseTime) {
		t.Errorf("LedgerCloseTime = %v, want %v", got.LedgerCloseTime, want.LedgerCloseTime)
	}
	if got.TxHash != want.TxHash {
		t.Errorf("TxHash = %q, want %q", got.TxHash, want.TxHash)
	}
	if got.OpIndex != want.OpIndex {
		t.Errorf("OpIndex = %d, want %d", got.OpIndex, want.OpIndex)
	}
	if got.LegIndex != want.LegIndex {
		t.Errorf("LegIndex = %d, want %d", got.LegIndex, want.LegIndex)
	}
	if got.Asset != want.Asset {
		t.Errorf("Asset = %q, want %q", got.Asset, want.Asset)
	}
	if got.FromAddress != want.FromAddress {
		t.Errorf("FromAddress = %q, want %q", got.FromAddress, want.FromAddress)
	}
	if got.ToAddress != want.ToAddress {
		t.Errorf("ToAddress = %q, want %q", got.ToAddress, want.ToAddress)
	}
	if got.Amount.String() != wantAmount {
		t.Errorf("Amount = %q, want %q", got.Amount.String(), wantAmount)
	}
}
