//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/classicmovements"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestAccountMovements_AssetFilterScopesPostgresTail executes the
// contract-scoped sep41_transfers query against real Postgres and the
// full /movements stack over HTTP. The account's newest transfers are all
// another token; its USDC transfers sit below them. ?asset=USDC must fill
// the page from USDC rows and page through all of them — the filter
// applied after the LIMIT served an empty page with no cursor.
func TestAccountMovements_AssetFilterScopesPostgresTail(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)
	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	g := gAccountFromSeed(t, 0x51)
	issuer := gAccountFromSeed(t, 0x52)
	counterparty := gAccountFromSeed(t, 0x53)
	usdc, err := canonical.NewClassicAsset("USDC", issuer)
	if err != nil {
		t.Fatal(err)
	}
	eurc, err := canonical.NewClassicAsset("EURC", issuer)
	if err != nil {
		t.Fatal(err)
	}
	usdcSAC, err := usdc.SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	otherContract, err := eurc.SacContractID()
	if err != nil {
		t.Fatal(err)
	}

	base := classicmovements.P23StartLedger + 1000
	var rows []timescale.SEP41TransferRow
	add := func(ledger uint32, contract, from, to string) {
		rows = append(rows, timescale.SEP41TransferRow{
			ObservedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC).Add(time.Duration(ledger-base) * time.Second),
			Ledger:     ledger,
			TxHash:     fmt.Sprintf("%064x", ledger),
			ContractID: contract,
			Kind:       timescale.SEP41Transfer,
			FromAddr:   from,
			ToAddr:     to,
			Amount:     big.NewInt(int64(ledger - base + 1)),
		})
	}
	for l := base + 100; l < base+105; l++ { // newest: another token
		add(l, otherContract, g, counterparty)
	}
	for l := base + 10; l < base+14; l++ { // older: USDC, both directions
		if (l-base)%2 == 0 {
			add(l, usdcSAC, g, counterparty)
		} else {
			add(l, usdcSAC, counterparty, g)
		}
	}
	if err := store.InsertSEP41TransferBatch(ctx, rows); err != nil {
		t.Fatalf("InsertSEP41TransferBatch: %v", err)
	}

	t.Run("store", func(t *testing.T) {
		page, err := store.ListSEP41TransfersByAddress(ctx, g, 3, timescale.SEP41TransferCursor{}, "", usdcSAC, 0)
		if err != nil {
			t.Fatalf("page 1: %v", err)
		}
		assertTransferLedgers(t, "page 1", page, base+13, base+12, base+11)
		last := page[len(page)-1]
		cur := timescale.SEP41TransferCursor{Ledger: last.Ledger, TxHash: last.TxHash, OpIndex: last.OpIndex, EventIndex: last.EventIndex}
		page, err = store.ListSEP41TransfersByAddress(ctx, g, 3, cur, "", usdcSAC, 0)
		if err != nil {
			t.Fatalf("page 2: %v", err)
		}
		assertTransferLedgers(t, "page 2", page, base+10)
		sent, err := store.ListSEP41TransfersByAddress(ctx, g, 3, timescale.SEP41TransferCursor{}, "sent", usdcSAC, 0)
		if err != nil {
			t.Fatalf("sent: %v", err)
		}
		assertTransferLedgers(t, "sent", sent, base+12, base+10)
	})

	t.Run("http", func(t *testing.T) {
		chAddr := clickhouseAddr(t)
		if err := clickhouse.EnsureAccountMovementsTable(ctx, chAddr); err != nil {
			t.Fatalf("EnsureAccountMovementsTable: %v", err)
		}
		er, err := clickhouse.NewExplorerReader(ctx, chAddr)
		if err != nil {
			t.Fatalf("NewExplorerReader: %v", err)
		}
		t.Cleanup(func() { _ = er.Close() })
		ts := httptest.NewServer(v1.New(v1.Options{Explorer: er, SEP41Movements: store}).Handler())
		t.Cleanup(ts.Close)

		q := url.Values{"asset": {usdc.String()}, "limit": {"3"}}
		page1 := getMovements(t, ts.URL, g, q)
		assertMovementLedgers(t, "page 1", page1, usdc.String(), base+13, base+12, base+11)
		if page1.NextCursor == "" {
			t.Fatal("page 1 is full but next_cursor is empty — the older USDC transfer is unreachable")
		}
		q.Set("cursor", page1.NextCursor)
		page2 := getMovements(t, ts.URL, g, q)
		assertMovementLedgers(t, "page 2", page2, usdc.String(), base+10)
	})
}

func getMovements(t *testing.T, base, g string, q url.Values) v1.AccountMovementsView {
	t.Helper()
	resp, err := http.Get(base + "/v1/accounts/" + g + "/movements?" + q.Encode())
	if err != nil {
		t.Fatalf("GET movements: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data v1.AccountMovementsView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Data
}

func assertTransferLedgers(t *testing.T, label string, got []timescale.SEP41TransferRow, want ...uint32) {
	t.Helper()
	ledgers := make([]uint32, len(got))
	for i, r := range got {
		ledgers[i] = r.Ledger
	}
	if fmt.Sprint(ledgers) != fmt.Sprint(want) {
		t.Fatalf("%s ledgers = %v, want %v", label, ledgers, want)
	}
}

func assertMovementLedgers(t *testing.T, label string, v v1.AccountMovementsView, asset string, want ...uint32) {
	t.Helper()
	ledgers := make([]uint32, len(v.Movements))
	for i, m := range v.Movements {
		ledgers[i] = m.Ledger
		if m.Asset != asset {
			t.Errorf("%s ledger %d asset = %q, want %q", label, m.Ledger, m.Asset, asset)
		}
	}
	if fmt.Sprint(ledgers) != fmt.Sprint(want) {
		t.Fatalf("%s ledgers = %v, want %v", label, ledgers, want)
	}
}
