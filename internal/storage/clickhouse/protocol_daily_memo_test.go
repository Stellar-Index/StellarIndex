package clickhouse

import (
	"errors"
	"testing"
	"time"
)

func TestMemoProtocolDaily_HoldsUntilDayTurnsOrMaxAge(t *testing.T) {
	clock := time.Date(2026, 10, 22, 20, 0, 0, 0, time.UTC)
	m := &protocolDailyMemo{now: func() time.Time { return clock }}
	since := clock.AddDate(0, 0, -90)
	reads := 0
	read := func() ([]ProtocolDailyPoint, error) {
		reads++
		return []ProtocolDailyPoint{{Date: "2026-10-21", Events: uint64(reads)}}, nil
	}
	get := func(ids ...string) []ProtocolDailyPoint {
		t.Helper()
		v, err := memoProtocolDaily(m, "series", ids, since, read)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	get("CA", "CB")[0].Events = 99
	if v := get("CB", "CA"); reads != 1 || v[0].Events != 1 {
		t.Fatalf("same id set in another order: reads=%d events=%d, want 1 read and the unmutated memo", reads, v[0].Events)
	}
	if get("CA"); reads != 2 {
		t.Fatalf("a different id set must read, reads=%d", reads)
	}
	if _, err := memoProtocolDaily(m, "breakdown", []string{"CA", "CB"}, since, read); err != nil || reads != 3 {
		t.Fatalf("a different read kind must not share an entry, reads=%d err=%v", reads, err)
	}

	clock = clock.Add(3*time.Hour + time.Minute)
	if get("CA", "CB"); reads != 4 {
		t.Fatalf("the UTC day turned; want a re-read, reads=%d", reads)
	}
	clock = clock.Add(protocolDailyMaxAge)
	if get("CA", "CB"); reads != 5 {
		t.Fatalf("past protocolDailyMaxAge; want a re-read, reads=%d", reads)
	}
}

func TestMemoProtocolDaily_DoesNotMemoiseErrors(t *testing.T) {
	m := &protocolDailyMemo{}
	reads := 0
	fail := func() ([]ProtocolDailyPoint, error) { reads++; return nil, errors.New("boom") }
	for range 2 {
		if _, err := memoProtocolDaily(m, "series", []string{"CA"}, time.Time{}, fail); err == nil {
			t.Fatal("error swallowed")
		}
	}
	if reads != 2 {
		t.Errorf("an error must not be memoised, reads=%d", reads)
	}
	if v, err := memoProtocolDaily(m, "series", nil, time.Time{}, fail); v != nil || err != nil || reads != 2 {
		t.Errorf("no ids must not read: v=%v err=%v reads=%d", v, err, reads)
	}
}
