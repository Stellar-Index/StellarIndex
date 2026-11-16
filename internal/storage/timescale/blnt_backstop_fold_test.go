package timescale

import (
	"slices"
	"testing"
	"time"
)

func bsEv(ledger int64, tx, kind, amount2, exp string) blntBackstopEvent {
	return blntBackstopEvent{
		pool: "P", tier: "SecondLoss", kind: kind, amount2: amount2, expiration: exp, txHash: tx,
		ts: time.Unix(ledger, 0), ledger: ledger,
	}
}

// TestFoldBLNTBackstop_SameLedgerOrderIsNotPoison — a queue and a dequeue in one ledger, stored in hash order with the dequeue
// first, replays queues-first instead of breaking the position; the share and queued totals equal those of every ordering of the
// ledger's events, and the queue's composition is marked unknown until the queue empties.
func TestFoldBLNTBackstop_SameLedgerOrderIsNotPoison(t *testing.T) {
	prefix := []blntBackstopEvent{bsEv(1, "a", "deposit", "18446744073709551621", ""), bsEv(2, "b", "queue_withdrawal", "5", "100")}
	ledger3 := []blntBackstopEvent{
		bsEv(3, "c", "dequeue_withdrawal", "30", ""),
		bsEv(3, "d", "withdraw", "4", ""),
		bsEv(3, "e", "deposit", "9", ""),
		bsEv(3, "f", "queue_withdrawal", "40", "300"),
	}
	perms := [][]int{{0, 1, 2, 3}, {3, 2, 1, 0}, {3, 0, 1, 2}, {2, 3, 1, 0}, {1, 3, 0, 2}}
	for _, p := range perms {
		evs := slices.Clone(prefix)
		for _, i := range p {
			evs = append(evs, ledger3[i])
		}
		f := foldBLNTBackstop(evs)[0]
		if f.Shares != "18446744073709551626" || !f.ExpirationsUnknown || f.QueuedShares != "11" || f.Queued != nil {
			t.Fatalf("order %v: fold = %+v, want 2^64+5+9-4 shares, 5+40-30-4 queued, composition unknown", p, f)
		}
	}
	drained := foldBLNTBackstop(append(slices.Clone(prefix), append(ledger3, bsEv(4, "g", "withdraw", "11", ""))...))[0]
	if drained.ExpirationsUnknown || drained.QueuedShares != "" || len(drained.Queued) != 0 || drained.Shares != "18446744073709551615" {
		t.Fatalf("drained fold = %+v, want a known empty queue", drained)
	}
}

// TestFoldBLNTBackstop_OneTransactionKeepsItsOrder — events of one transaction run in stored order, so an underflow there is a
// real inconsistency and no composition is ambiguous.
func TestFoldBLNTBackstop_OneTransactionKeepsItsOrder(t *testing.T) {
	ok := foldBLNTBackstop([]blntBackstopEvent{
		bsEv(1, "a", "deposit", "50", ""), bsEv(2, "b", "queue_withdrawal", "20", "100"), bsEv(2, "b", "dequeue_withdrawal", "5", ""),
	})[0]
	if ok.ExpirationsUnknown || len(ok.Queued) != 1 || ok.Queued[0].Shares != "15" || ok.Queued[0].Expiration.Unix() != 100 {
		t.Fatalf("fold = %+v", ok)
	}
	broken := foldBLNTBackstop([]blntBackstopEvent{
		bsEv(1, "a", "deposit", "50", ""), bsEv(2, "b", "dequeue_withdrawal", "5", ""), bsEv(2, "b", "queue_withdrawal", "20", "100"),
	})[0]
	if broken.Shares != "" {
		t.Fatalf("fold = %+v, want unknown", broken)
	}
}
