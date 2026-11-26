package timescale

import (
	"strings"
	"testing"
	"time"
)

// TestValidateSushiswapV3ManagerEvent: each kind carries exactly its columns; a missing or a foreign one is refused before SQL.
func TestValidateSushiswapV3ManagerEvent(t *testing.T) {
	base := SushiswapV3ManagerEvent{
		Manager: "CARTUL5AWDZYBSN7HUUJZSKCAKCIAKM7M54Z76G6KRYCK4XPR3OHUQZ4", Ledger: 1,
		LedgerCloseTime: time.Unix(1, 0), TxHash: make([]byte, 32), TokenID: 7,
	}
	with := func(f func(*SushiswapV3ManagerEvent)) SushiswapV3ManagerEvent {
		e := base
		f(&e)
		return e
	}
	ok := []SushiswapV3ManagerEvent{
		with(func(e *SushiswapV3ManagerEvent) { e.Kind, e.Owner = "mint", "G1" }),
		with(func(e *SushiswapV3ManagerEvent) { e.Kind, e.Owner = "burn", "G1" }),
		with(func(e *SushiswapV3ManagerEvent) {
			e.Kind, e.Liquidity, e.Amount0, e.Amount1 = "increase_liquidity", "1", "2", "3"
		}),
		with(func(e *SushiswapV3ManagerEvent) {
			e.Kind, e.Liquidity, e.Amount0, e.Amount1 = "decrease_liquidity", "1", "0", "0"
		}),
		with(func(e *SushiswapV3ManagerEvent) {
			e.Kind, e.Recipient, e.Amount0, e.Amount1 = "collect", "G1", "0", "5"
		}),
	}
	for _, e := range ok {
		if err := validateSushiswapV3ManagerEvent(e); err != nil {
			t.Errorf("%s: %v", e.Kind, err)
		}
	}
	bad := map[string]SushiswapV3ManagerEvent{
		"owner":    with(func(e *SushiswapV3ManagerEvent) { e.Kind = "mint" }),
		"amount_0": with(func(e *SushiswapV3ManagerEvent) { e.Kind, e.Owner, e.Amount0 = "burn", "G1", "1" }),
		"liquidity": with(func(e *SushiswapV3ManagerEvent) {
			e.Kind, e.Recipient, e.Liquidity, e.Amount0, e.Amount1 = "collect", "G1", "1", "0", "0"
		}),
		"amount_1":  with(func(e *SushiswapV3ManagerEvent) { e.Kind, e.Liquidity, e.Amount0 = "increase_liquidity", "1", "2" }),
		"recipient": with(func(e *SushiswapV3ManagerEvent) { e.Kind, e.Amount0, e.Amount1 = "collect", "0", "0" }),
		"Kind":      with(func(e *SushiswapV3ManagerEvent) { e.Kind, e.Owner = "swap", "G1" }),
		"TxHash":    with(func(e *SushiswapV3ManagerEvent) { e.Kind, e.Owner, e.TxHash = "mint", "G1", nil }),
	}
	for want, e := range bad {
		err := validateSushiswapV3ManagerEvent(e)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s case: err = %v, want one naming %s", want, err, want)
		}
	}
}
