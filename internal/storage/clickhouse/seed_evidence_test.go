package clickhouse

import (
	"errors"
	"testing"
)

func TestVerifySeedWalk(t *testing.T) {
	clean := func(uint32, uint32) (uint32, bool, string, error) { return 0, false, "", nil }

	t.Run("intact range is verified through its top", func(t *testing.T) {
		ev, err := verifySeedWalk(100, 500, 900, clean)
		if err != nil {
			t.Fatal(err)
		}
		if ev != (SeedEvidence{FromLedger: 100, ToLedger: 500, LakeVerifiedThrough: 500}) {
			t.Errorf("evidence = %+v", ev)
		}
	})

	t.Run("entry changes above the ledgers tip are not reduced", func(t *testing.T) {
		var gotFrom, gotTo uint32
		ev, err := verifySeedWalk(100, 500, 480, func(from, to uint32) (uint32, bool, string, error) {
			gotFrom, gotTo = from, to
			return 0, false, "", nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if gotFrom != 100 || gotTo != 480 || ev.ToLedger != 480 || ev.LakeVerifiedThrough != 480 {
			t.Errorf("checked [%d, %d], evidence %+v; want [100, 480] verified through 480", gotFrom, gotTo, ev)
		}
	})

	t.Run("a hole refuses the walk", func(t *testing.T) {
		ev, err := verifySeedWalk(100, 500, 900, func(uint32, uint32) (uint32, bool, string, error) {
			return 321, true, "substrate: missing ledger at 321", nil
		})
		if !errors.Is(err, ErrSeedLakeIncomplete) {
			t.Fatalf("err = %v, want ErrSeedLakeIncomplete", err)
		}
		if ev.LakeVerifiedThrough != 0 {
			t.Errorf("LakeVerifiedThrough = %d on a refused walk, want 0", ev.LakeVerifiedThrough)
		}
	})

	t.Run("ledgers tip below the entry-change floor refuses", func(t *testing.T) {
		if _, err := verifySeedWalk(100, 500, 50, clean); !errors.Is(err, ErrSeedLakeIncomplete) {
			t.Fatalf("err = %v, want ErrSeedLakeIncomplete", err)
		}
	})

	t.Run("a failed check is returned, not read as intact", func(t *testing.T) {
		boom := errors.New("boom")
		_, err := verifySeedWalk(100, 500, 900, func(uint32, uint32) (uint32, bool, string, error) { return 0, false, "", boom })
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
	})
}

func TestSeedWalkProgressScan(t *testing.T) {
	var heard [][2]uint32
	walk := SeedWalk{Progress: func(from, to uint32) { heard = append(heard, [2]uint32{from, to}) }}
	boom := errors.New("boom")
	scan := walk.progressScan(func(from, _ uint32) error {
		if from == 7 {
			return boom
		}
		return nil
	})
	if err := scan(1, 6); err != nil {
		t.Fatal(err)
	}
	if err := scan(7, 9); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if len(heard) != 1 || heard[0] != [2]uint32{1, 6} {
		t.Errorf("progress heard %v, want only the completed window [1, 6]", heard)
	}
}
