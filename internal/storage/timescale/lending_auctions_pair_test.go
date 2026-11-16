package timescale

import "testing"

func lifeEv(ledger uint32, tx, kind, pct string) auctionLifecycleEvent {
	return auctionLifecycleEvent{LendingEventPos: LendingEventPos{Ledger: ledger, TxHash: tx}, kind: kind, user: "U", fillPercent: pct}
}

// TestPairAuctionLifecycle_SameLedgerDelete — a delete in the ledger of a new auction goes to the earlier auction while it is
// open, and to the new one once the earlier is closed (deleted or filled to 100%).
func TestPairAuctionLifecycle_SameLedgerDelete(t *testing.T) {
	for name, tc := range map[string]struct {
		events                   []auctionLifecycleEvent
		firstDeleted, newDeleted bool
	}{
		"earlier open": {[]auctionLifecycleEvent{lifeEv(1, "a", "new", ""), lifeEv(5, "b", "delete", ""), lifeEv(5, "c", "new", "")}, true, false},
		"earlier filled": {[]auctionLifecycleEvent{
			lifeEv(1, "a", "new", ""), lifeEv(3, "b", "fill", "100"), lifeEv(5, "d", "delete", ""),
			lifeEv(5, "c", "new", ""),
		}, false, true},
		"earlier deleted": {[]auctionLifecycleEvent{
			lifeEv(1, "a", "new", ""), lifeEv(5, "b", "delete", ""), lifeEv(5, "d", "delete", ""),
			lifeEv(5, "c", "new", ""),
		}, true, true},
	} {
		auctions := []LendingAuction{{LendingEventPos: tc.events[len(tc.events)-1].LendingEventPos, User: "U"}, {LendingEventPos: tc.events[0].LendingEventPos, User: "U"}}
		pairAuctionLifecycle(auctions, tc.events)
		if (auctions[1].Deleted != nil) != tc.firstDeleted || (auctions[0].Deleted != nil) != tc.newDeleted {
			t.Errorf("%s: first deleted=%v new deleted=%v", name, auctions[1].Deleted, auctions[0].Deleted)
		}
	}
}
