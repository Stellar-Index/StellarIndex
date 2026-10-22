package clickhouse

import (
	"strconv"
	"testing"
)

func spArr(i int, events ...string) SponsorshipArrangement {
	a := SponsorshipArrangement{SponsorshipKey: SponsorshipKey{Sponsor: "S", Kind: "offer", KeyID: strconv.Itoa(100 + i)}}
	for n, e := range events {
		a.Events = append(a.Events, SponsorshipEventRow{Ledger: uint32(n + 1), Event: e})
	}
	return a
}

// spWindows serves arrangements as windows of at most rows events, cutting the last arrangement short the way a
// LIMIT on events would.
func spWindows(all []SponsorshipArrangement, rows int, calls *int) func(SponsorshipKey) ([]SponsorshipArrangement, int, error) {
	return func(after SponsorshipKey) ([]SponsorshipArrangement, int, error) {
		*calls++
		var out []SponsorshipArrangement
		n := 0
		for _, a := range all {
			if after.IsSet() && a.KeyID <= after.KeyID {
				continue
			}
			take := min(len(a.Events), rows-n)
			if take == 0 {
				break
			}
			cut := a
			cut.Events = a.Events[:take]
			out = append(out, cut)
			n += take
		}
		return out, n, nil
	}
}

func spWhole(all []SponsorshipArrangement) func(SponsorshipKey) ([]SponsorshipArrangement, error) {
	return func(k SponsorshipKey) ([]SponsorshipArrangement, error) {
		for _, a := range all {
			if a.SponsorshipKey == k {
				return []SponsorshipArrangement{a}, nil
			}
		}
		return nil, nil
	}
}

func TestPageSponsorships_InForceAcrossWindowsWithCutArrangements(t *testing.T) {
	all := []SponsorshipArrangement{
		spArr(0, "started", "ended"), spArr(1, "started"), spArr(2, "started", "revoked", "started"),
		spArr(3, "started", "ended"), spArr(4, "started", "ended", "started"),
	}
	var calls int
	page, resume, err := pageSponsorships(SponsorshipKey{}, 10, 4, 10, true, spWindows(all, 4, &calls), spWhole(all))
	if err != nil || resume.IsSet() {
		t.Fatalf("err %v resume %+v", err, resume)
	}
	if len(page) != 3 || page[0].KeyID != "101" || page[1].KeyID != "102" || page[2].KeyID != "104" {
		t.Fatalf("page = %+v, want 101, 102, 104", page)
	}
	if len(page[1].Events) != 3 || len(page[2].Events) != 3 {
		t.Fatalf("an arrangement cut by a window was served short: %+v", page)
	}
}

func TestPageSponsorships_BudgetSpentReturnsTheResumeKey(t *testing.T) {
	var all []SponsorshipArrangement
	for i := range 20 {
		all = append(all, spArr(i, "started", "ended"))
	}
	all = append(all, spArr(20, "started"))
	var calls int
	page, resume, err := pageSponsorships(SponsorshipKey{}, 5, 6, 2, true, spWindows(all, 6, &calls), spWhole(all))
	if err != nil || len(page) != 0 || calls != 2 {
		t.Fatalf("page %+v calls %d err %v, want empty after two windows", page, calls, err)
	}
	// Two windows of six rows hold two whole arrangements each; the third is cut and left to the next page.
	if resume.KeyID != "103" {
		t.Fatalf("resume = %+v, want the last whole arrangement read (103)", resume)
	}
	page, resume, _ = pageSponsorships(resume, 5, 6, 20, true, spWindows(all, 6, &calls), spWhole(all))
	if len(page) != 1 || page[0].KeyID != "120" || resume.IsSet() {
		t.Fatalf("resumed page = %+v resume %+v, want the one in force", page, resume)
	}
}

func TestPageSponsorships_OneArrangementFillingTheWindowIsReadWhole(t *testing.T) {
	all := []SponsorshipArrangement{spArr(0, "started", "ended", "started", "ended", "started"), spArr(1, "started")}
	var calls int
	page, _, err := pageSponsorships(SponsorshipKey{}, 10, 3, 10, false, spWindows(all, 3, &calls), spWhole(all))
	if err != nil || len(page) != 2 || len(page[0].Events) != 5 {
		t.Fatalf("page = %+v (%v), want the long arrangement whole, then the next", page, err)
	}
}

func TestPageSponsorships_StopsAtLimit(t *testing.T) {
	all := []SponsorshipArrangement{spArr(0, "started"), spArr(1, "started"), spArr(2, "started")}
	var calls int
	page, resume, _ := pageSponsorships(SponsorshipKey{}, 2, 100, 10, false, spWindows(all, 100, &calls), spWhole(all))
	if len(page) != 2 || resume.IsSet() || calls != 1 {
		t.Fatalf("page %d resume %+v calls %d", len(page), resume, calls)
	}
}
