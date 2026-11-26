package sushiswap_v3

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// crossRow is one event of a sampled post-upgrade manager transaction; testdata/manager_crosscheck.tsv is a verbatim
// lake export of every event, in order, of six such transactions.
type crossRow struct {
	contract, txHash, sym, data string
	ledger                      uint32
	eventIndex                  int
	topics                      []string
}

type crossTransfer struct {
	token, from, to string
	amount          canonical.Amount
}

// loadCrossCheck returns the rows grouped by transaction, in file order.
func loadCrossCheck(t *testing.T) [][]crossRow {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "manager_crosscheck.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var txs [][]crossRow
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		c := strings.Split(sc.Text(), "\t")
		if len(c) != 10 {
			t.Fatalf("row has %d columns, want 10: %q", len(c), sc.Text())
		}
		ledger, err1 := strconv.ParseUint(c[2], 10, 32)
		idx, err2 := strconv.Atoi(c[5])
		if err1 != nil || err2 != nil {
			t.Fatalf("bad ledger/event_index in %q", sc.Text())
		}
		r := crossRow{
			contract: c[1], ledger: uint32(ledger), txHash: c[3], eventIndex: idx, sym: c[6], data: c[9],
			topics: strings.Split(strings.TrimSuffix(strings.TrimPrefix(c[8], "['"), "']"), "','"),
		}
		if n := len(txs); n == 0 || txs[n-1][0].txHash != r.txHash {
			txs = append(txs, nil)
		}
		txs[len(txs)-1] = append(txs[len(txs)-1], r)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return txs
}

// TestManagerCrossCheck_PostUpgrade anchors the be969de5 manager's u128 amounts on two sources outside it, exactly:
// the pool event each manager event drove (decrease -> burn, collect -> collect, increase -> mint) and the SEP-41
// transfers that moved the tokens (collect: pool -> recipient; increase: payer -> pool), token0 first, as a V3 pool pays.
func TestManagerCrossCheck_PostUpgrade(t *testing.T) {
	poolKindOf := map[string]string{EventDecreaseLiquidity: EventBurn, EventCollect: EventCollect, EventIncreaseLiquidity: EventMint}
	known := loadManagerFixtures(t)
	d := managerDecoder()
	tokenOrder := map[string][2]string{}
	var pairs, transferPairs int
	for _, rows := range loadCrossCheck(t) {
		for _, r := range rows {
			poolKind, ok := poolKindOf[r.sym]
			if r.contract != MainnetPositionManager || !ok {
				continue
			}
			if !slices.ContainsFunc(known, func(k managerFixture) bool {
				return k.want == "ok" && k.kind == r.sym && k.txHash == r.txHash && k.data == r.data && slices.Equal(k.topics, r.topics)
			}) {
				t.Errorf("%s@%d/%s is not a testdata/manager_events.txt row", r.sym, r.ledger, r.txHash[:8])
			}
			m := decodeCrossManager(t, d, r)
			pool, p := crossPoolEvent(t, rows, poolKind)
			checkCrossPoolSide(t, r, m, p)
			pairs++
			if r.sym != EventDecreaseLiquidity {
				checkCrossTransfers(t, rows, r, m, pool, p.Sender, tokenOrder)
				transferPairs++
			}
		}
	}
	if pairs != 9 || transferPairs != 6 {
		t.Fatalf("checked %d pool pairs and %d transfer pairs, want 9 and 6", pairs, transferPairs)
	}
}

func decodeCrossManager(t *testing.T, d *Decoder, r crossRow) ManagerEvent {
	t.Helper()
	ev := managerEvent(r.contract, r.topics, r.data)
	ev.Ledger, ev.TxHash, ev.EventIndex = r.ledger, r.txHash, r.eventIndex
	out, err := d.Decode(ev)
	if err != nil || len(out) != 1 {
		t.Fatalf("%s@%d: Decode = %v, %v", r.sym, r.ledger, out, err)
	}
	m, ok := out[0].(ManagerEvent)
	if !ok {
		t.Fatalf("%s@%d: Decode emitted %T", r.sym, r.ledger, out[0])
	}
	return m
}

// crossPoolEvent returns the one pool-side event of kind in the transaction, decoded by the attested pool decoder.
func crossPoolEvent(t *testing.T, rows []crossRow, kind string) (string, PositionFields) {
	t.Helper()
	var found []crossRow
	for _, r := range rows {
		if r.contract != MainnetPositionManager && r.sym == kind && len(r.topics) == 1 {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("tx %s: %d pool %s events, want 1", rows[0].txHash[:8], len(found), kind)
	}
	p, err := decodePositionFields(kind, found[0].data)
	if err != nil {
		t.Fatalf("pool %s: %v", kind, err)
	}
	return found[0].contract, p
}

func checkCrossPoolSide(t *testing.T, r crossRow, m ManagerEvent, p PositionFields) {
	t.Helper()
	at := r.sym + "@" + strconv.FormatUint(uint64(r.ledger), 10) + "/" + r.txHash[:8]
	if p.Owner != MainnetPositionManager {
		t.Errorf("%s: pool position owner %s, want the manager", at, p.Owner)
	}
	if m.Amount0.String() != p.Amount0.String() || m.Amount1.String() != p.Amount1.String() {
		t.Errorf("%s: manager amounts %s/%s, pool %s/%s", at, m.Amount0, m.Amount1, p.Amount0, p.Amount1)
	}
	if r.sym == EventCollect {
		if m.Recipient != p.Recipient {
			t.Errorf("%s: manager recipient %s, pool %s", at, m.Recipient, p.Recipient)
		}
		return
	}
	if !m.HasLiquidity || !p.HasLiquidity || m.Liquidity.String() != p.Liquidity.String() {
		t.Errorf("%s: manager liquidity %s, pool %s", at, m.Liquidity, p.Liquidity)
	}
}

// checkCrossTransfers requires exactly the two token transfers of the position change, token0 then token1, and the same
// token order for a pool in every transaction (so the order rule is falsifiable, not assumed).
func checkCrossTransfers(t *testing.T, rows []crossRow, r crossRow, m ManagerEvent, pool, payer string, order map[string][2]string) {
	t.Helper()
	at := r.sym + "@" + strconv.FormatUint(uint64(r.ledger), 10) + "/" + r.txHash[:8]
	from, to := pool, m.Recipient
	if r.sym == EventIncreaseLiquidity {
		from, to = payer, pool
	}
	var ts []crossTransfer
	for _, row := range rows {
		if row.sym == "transfer" && len(row.topics) == 4 {
			ts = append(ts, decodeCrossTransfer(t, row))
		}
	}
	if len(ts) != 2 {
		t.Fatalf("%s: %d transfers, want 2", at, len(ts))
	}
	for i, want := range []canonical.Amount{m.Amount0, m.Amount1} {
		if ts[i].from != from || ts[i].to != to || ts[i].amount.String() != want.String() {
			t.Errorf("%s: transfer %d = %s %s->%s, want %s %s->%s", at, i, ts[i].amount, ts[i].from, ts[i].to, want, from, to)
		}
	}
	got := [2]string{ts[0].token, ts[1].token}
	if got[0] == got[1] {
		t.Errorf("%s: both transfers are of token %s", at, got[0])
	}
	if prev, ok := order[pool]; ok && prev != got {
		t.Errorf("%s: pool %s token order %v, earlier %v", at, pool, got, prev)
	}
	order[pool] = got
}

func decodeCrossTransfer(t *testing.T, r crossRow) crossTransfer {
	t.Helper()
	out := crossTransfer{token: r.contract}
	for i, dst := range []*string{&out.from, &out.to} {
		sv, err := scval.Parse(r.topics[i+1])
		if err == nil {
			*dst, err = scval.AsAddressStrkey(sv)
		}
		if err != nil {
			t.Fatalf("transfer topic %d: %v", i+1, err)
		}
	}
	sv, err := scval.Parse(r.data)
	if err == nil {
		out.amount, err = scval.AsAmountFromI128(sv)
	}
	if err != nil {
		t.Fatalf("transfer amount: %v", err)
	}
	return out
}
