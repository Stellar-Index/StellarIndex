package coinbase

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func TestDropRejectedProducts(t *testing.T) {
	active := []string{"XLM-USD", "WBTC-USD", "BTC-USD"}
	cases := []struct {
		name, text string
		wantRem    []string
		wantDrop   []string
	}{
		{"exact token only", "coinbase: subscription rejected: Failed to subscribe BTC-USD is not a valid product", []string{"XLM-USD", "WBTC-USD"}, []string{"BTC-USD"}},
		{"case-insensitive", "invalid product btc-usd", []string{"XLM-USD", "WBTC-USD"}, []string{"BTC-USD"}},
		{"no product named", "rate limited", active, nil},
		{"never empties", "XLM-USD WBTC-USD BTC-USD", active, nil},
	}
	for _, c := range cases {
		rem, dropped := dropRejectedProducts(active, c.text)
		if !reflect.DeepEqual(rem, c.wantRem) || !reflect.DeepEqual(dropped, c.wantDrop) {
			t.Errorf("%s: got (%v, %v), want (%v, %v)", c.name, rem, dropped, c.wantRem, c.wantDrop)
		}
	}
}

// One rejected product must not block the rest: the reconnect resubscribes
// without it and trades flow.
func TestStreamer_RejectedProductDroppedOnResubscribe(t *testing.T) {
	pm := mustPairs(t)
	var mu sync.Mutex
	var subs [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		_, data, err := c.Read(r.Context())
		if err != nil {
			return
		}
		var req subscribeReq
		if err := json.Unmarshal(data, &req); err != nil {
			return
		}
		mu.Lock()
		subs = append(subs, req.Channels[0].ProductIDs)
		n := len(subs)
		mu.Unlock()
		if n == 1 {
			_ = c.Write(r.Context(), websocket.MessageText,
				[]byte(`{"type":"error","message":"Failed to subscribe","reason":"BTC-USD is not a valid product"}`))
			time.Sleep(time.Second)
			return
		}
		_ = c.Write(r.Context(), websocket.MessageText, []byte(
			`{"type":"match","trade_id":1,"side":"buy","size":"10","price":"0.5","product_id":"XLM-USD","sequence":1,"time":"2026-04-24T00:00:00Z"}`))
		<-r.Context().Done()
	}))
	defer srv.Close()

	s := NewStreamer(pm)
	s.Endpoint = "ws" + strings.TrimPrefix(srv.URL, "http")
	s.InitialBackoff = 10 * time.Millisecond
	s.MaxBackoff = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := s.Start(ctx, []canonical.Pair{pm["XLM-USD"], pm["BTC-USD"]})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case tr, ok := <-out:
		if !ok || tr.Pair != pm["XLM-USD"] {
			t.Fatalf("unexpected trade %v ok=%v", tr, ok)
		}
	case <-ctx.Done():
		t.Fatal("no trade after resubscribe")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(subs) < 2 || len(subs[0]) != 2 || !reflect.DeepEqual(subs[1], []string{"XLM-USD"}) {
		t.Fatalf("subscriptions = %v, want 2 then [XLM-USD]", subs)
	}
}
