package classicmovements

import (
	"errors"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/StellarIndex/stellar-index/internal/dispatcher"
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
