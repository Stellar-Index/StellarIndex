//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	"github.com/Stellar-Index/StellarIndex/internal/sources/rozo"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestRozoMemoBytes_HostileMemoLandsInPostgres is the F052 proof against a
// REAL Postgres, through the production path end to end:
//
//	events.Event -> rozo.Decoder.Decode -> pipeline.HandleEvent
//	             -> persistRozoEvent -> Store.InsertRozoEvent -> rozo_events
//
// A Rozo v1 memo is an ScString — the payer picks its BYTES, for one
// stroop. rozo_events.memo is `text`, and Postgres refuses a NUL or an
// invalid UTF-8 sequence there with SQLSTATE 22021. The projector classes
// that as a permanent data error and skips the event for good, so before
// the fix every case below marked hostile failed HandleEvent and left no
// row: an attacker-chosen, un-closable hole in the bridge history.
//
// Asserted per memo: the insert succeeds; exactly one row lands; the stored
// text is the exact deterministic scval.ToText value; the on-chain bytes
// are recoverable both in Go (scval.FromText) and in SQL (the documented
// decode(substr(memo, 3), 'hex') recipe); and a replay of the same event
// rewrites the same single row. Ordinary memos must be stored verbatim.
func TestRozoMemoBytes_HostileMemoLandsInPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cases := []struct {
		name     string
		memo     string
		wantText string
	}{
		{"ordinary tag stored verbatim", "binance-deposit-tag-987654", "binance-deposit-tag-987654"},
		{"empty memo stays empty not NULL", "", ""},
		{"multibyte utf8 stored verbatim", "commande-№42-日本", "commande-№42-日本"},
		{"highest rune and a noncharacter", "a\U0010FFFF￿b", "a\U0010FFFF￿b"},
		{"hostile: NUL byte", "tag\x00tail", `\x746167007461696c`},
		{"hostile: invalid utf8", "tag\xff\xfe", `\x746167fffe`},
		{"hostile: truncated multibyte", "ab\xe2\x82", `\x6162e282`},
		{"hostile: overlong NUL", "\xc0\x80", `\xc080`},
		{"hostile: encoded surrogate", "\xed\xa0\x80", `\xeda080`},
		{"hostile: above U+10FFFF", "\xf4\x90\x80\x80", `\xf4908080`},
		{"literal that mimics an encoding", `\x00`, `\x5c783030`},
	}

	logger := discardLogger()
	decoder := rozo.NewDecoder()
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ledger := uint32(62_800_000 + i)
			txHash := fmt.Sprintf("%064x", 0xf052_0000+i)
			ev := rozoPaymentEventWithMemo(t, ledger, txHash, tc.memo)

			// Twice: the second pass is the re-derive. It must produce the
			// same row, not a second one and not a different memo.
			for pass := 1; pass <= 2; pass++ {
				out, err := decoder.Decode(ev)
				if err != nil || len(out) != 1 {
					t.Fatalf("pass %d: Decode = %d events, err %v — a hostile memo must not cost the event", pass, len(out), err)
				}
				if err := pipeline.HandleEvent(ctx, logger, store, out[0]); err != nil {
					t.Fatalf("pass %d: HandleEvent: %v — Postgres refused the row; in production this is a "+
						"permanent skip and the payment is lost (F052)", pass, err)
				}
			}

			if n := countRows(t, store, `SELECT COUNT(*) FROM rozo_events WHERE ledger = $1 AND tx_hash = $2`, int(ledger), txHash); n != 1 {
				t.Fatalf("rozo_events rows = %d, want exactly 1", n)
			}

			var (
				memo    sql.NullString
				sqlBack []byte
			)
			if err := store.DB().QueryRowContext(ctx, `
				SELECT memo,
				       CASE WHEN left(memo, 2) = '\x'
				            THEN decode(substr(memo, 3), 'hex')
				            ELSE convert_to(memo, 'UTF8') END
				  FROM rozo_events WHERE ledger = $1 AND tx_hash = $2`, int(ledger), txHash).Scan(&memo, &sqlBack); err != nil {
				t.Fatalf("read back: %v", err)
			}
			if !memo.Valid {
				t.Fatal("memo stored as NULL; a payment memo is always a value")
			}
			if memo.String != tc.wantText {
				t.Fatalf("stored memo = %q, want %q", memo.String, tc.wantText)
			}
			goBack, err := scval.FromText(memo.String)
			if err != nil {
				t.Fatalf("FromText(%q): %v", memo.String, err)
			}
			if string(goBack) != tc.memo {
				t.Fatalf("bytes lost (Go): on-chain %q, recovered %q", tc.memo, goBack)
			}
			if !bytes.Equal(sqlBack, []byte(tc.memo)) {
				t.Fatalf("bytes lost (SQL recipe): on-chain %q, recovered %q", tc.memo, sqlBack)
			}
		})
	}
}

// rozoPaymentEventWithMemo builds the live long-topic v1 PaymentEvent
// ({amount, destination, from, memo} ScMap, topic ("payment_event",)) for a
// gated mainnet Rozo contract, with a caller-chosen memo.
func rozoPaymentEventWithMemo(t *testing.T, ledger uint32, txHash, memo string) events.Event {
	t.Helper()
	from := prMakeAccountStrkey(t, 0x21)
	dest := prMakeAccountStrkey(t, 0x31)
	body := prScMap(
		xdr.ScMapEntry{Key: prSymbol("amount"), Val: prI128(big.NewInt(1))},
		xdr.ScMapEntry{Key: prSymbol("destination"), Val: prAccountAddr(t, dest)},
		xdr.ScMapEntry{Key: prSymbol("from"), Val: prAccountAddr(t, from)},
		xdr.ScMapEntry{Key: prSymbol("memo"), Val: prScString(memo)},
	)
	return events.Event{
		Type:                     "contract",
		Ledger:                   ledger,
		LedgerClosedAt:           "2026-09-01T00:00:00Z",
		ContractID:               rozo.MainnetPaymentContract,
		OperationIndex:           0,
		EventIndex:               0,
		TxHash:                   txHash,
		InSuccessfulContractCall: true,
		Topic:                    []string{prB64(t, prSymbol("payment_event"))},
		Value:                    prB64(t, body),
	}
}
