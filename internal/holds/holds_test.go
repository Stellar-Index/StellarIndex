package holds

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func TestParseRejectsBadHolds(t *testing.T) {
	for name, doc := range map[string]string{
		"no reason":    "[[hold]]\nasset = \"native\"\n",
		"no selector":  "[[hold]]\nreason = \"r\"\n",
		"unknown key":  "[[hold]]\nasset = \"native\"\nreason = \"r\"\nassett = \"x\"\n",
		"bad contract": "[[hold]]\ncontract_id = \"GABC\"\nreason = \"r\"\n",
		"bad range":    "[[hold]]\nledger_from = 9\nledger_to = 3\nreason = \"r\"\n",
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestWatchNonPositiveIntervalDoesNotPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "holds.toml")
	if err := os.WriteFile(path, []byte("[[hold]]\nasset = \"native\"\nreason = \"r\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var applied atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		Watch(ctx, path, 0, func([]Hold) { applied.Add(1) }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if applied.Load() != 1 {
		t.Fatalf("applied %d times, want 1", applied.Load())
	}
}

func TestMatchCoversXLMAliasesAndAllSelectors(t *testing.T) {
	list, err := Parse([]byte("[[hold]]\nasset = \"native\"\nledger_from = 10\nledger_to = 20\nreason = \"r\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		sub  Subject
		want bool
	}{
		{Subject{Assets: []string{"native"}, Ledger: 15}, true},
		{Subject{Assets: []string{"crypto:XLM"}, Ledger: 15}, true},
		{Subject{Assets: []string{"native"}, Ledger: 21}, false},
		{Subject{Assets: []string{"native"}}, false},
		{Subject{Assets: []string{"crypto:BTC"}, Ledger: 15}, false},
	}
	for i, c := range cases {
		if _, got := Match(list, c.sub); got != c.want {
			t.Errorf("case %d: got %v want %v", i, got, c.want)
		}
	}
}

func TestMatchFoldsSpellingsAndSAC(t *testing.T) {
	const usdc = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	parsed, err := canonical.ParseAsset(usdc)
	if err != nil {
		t.Fatal(err)
	}
	sac, err := parsed.SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		hold, subject string
		want          bool
	}{
		{`asset = "native"`, "XLM", true},
		{`asset = "native"`, "xlm", true},
		{`asset = "native"`, "Native", true},
		{`asset = "crypto:XLM"`, "xlm", true},
		{`asset = "` + usdc + `"`, sac, true},
		{`contract_id = "` + sac + `"`, usdc, true},
		{`asset = "` + usdc + `"`, "crypto:BTC", false},
	}
	for i, c := range cases {
		list, err := Parse([]byte("[[hold]]\n" + c.hold + "\nreason = \"r\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if _, got := Match(list, Subject{Assets: []string{c.subject}}); got != c.want {
			t.Errorf("case %d (%s vs %s): got %v want %v", i, c.hold, c.subject, got, c.want)
		}
	}
}
