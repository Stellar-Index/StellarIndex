package explorer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// eventsProbeErrReader has no captured SAC instance, so resolution falls to the events probe, which errors.
type eventsProbeErrReader struct {
	*capReader
	evErr error
}

func (*eventsProbeErrReader) SACClassicAssetName(context.Context, string) (string, bool, error) {
	return "", false, nil
}

func (r *eventsProbeErrReader) SACAssetFromEvents(context.Context, string) (string, bool, error) {
	return "", false, r.evErr
}

// A failed events probe is not proof the contract is a non-SAC: movements filter, asset movements and holders all
// read lookupFailed to degrade (or 503) rather than serve the unfolded C... id's empty answer as clean.
func TestResolveSEP41MovementAssetChecked_EventsProbeErrorIsLookupFailure(t *testing.T) {
	usdc, err := canonical.NewClassicAsset("USDC", validTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	sac, err := usdc.SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	reader := &eventsProbeErrReader{capReader: &capReader{probe: &deadlineProbe{}}, evErr: errors.New("connection reset")}
	h := newProbeHandler(reader, nil)

	name, failed := h.resolveSEP41MovementAssetChecked(context.Background(), sac)
	if name != sac || !failed {
		t.Fatalf("events probe error: got (%q, lookupFailed=%v), want (%q, true)", name, failed, sac)
	}

	reader.evErr = nil
	if _, failed = h.resolveSEP41MovementAssetChecked(context.Background(), sac); failed {
		t.Fatal("a clean not-found probe is a proven non-SAC answer, not a lookup failure")
	}
}

type failingSEP41Tail struct{}

func (failingSEP41Tail) ListSEP41TransfersByAddress(context.Context, string, int, timescale.SEP41TransferCursor, string, string, uint32) ([]timescale.SEP41TransferRow, error) {
	return nil, errors.New("statement timeout")
}

// A failed Postgres tail read on a short (cursorless) page serves the archive alone: the page must be degraded. A full page 503s
// instead (TestAccountMovements_WalkSurvivesArmFailure).
func TestAccountMovements_TailReadFailureIsDegraded(t *testing.T) {
	wm := timescale.MovementsFloor() + 1000
	reader := &movementsArmReader{capReader: &capReader{probe: &deadlineProbe{}}, wm: wm}
	for i := range uint32(3) {
		reader.chRows = append(reader.chRows, clickhouse.AccountMovementRow{Ledger: wm - i, TxHash: "aa"})
	}

	run := func(tail SEP41MovementsReader) bool {
		var degraded, wrote bool
		h := newProbeHandler(reader, nil)
		h.SEP41Movements = tail
		h.WriteJSONAt = func(w http.ResponseWriter, _ any, _, d bool, _ time.Time) {
			degraded, wrote = d, true
			w.WriteHeader(http.StatusOK)
		}
		h.WriteJSON = func(w http.ResponseWriter, _ any, _ bool) {
			wrote = true
			w.WriteHeader(http.StatusOK)
		}
		r := httptest.NewRequest(http.MethodGet, "/v1/accounts/"+validTestAccount+"/movements", nil)
		r.SetPathValue("g_strkey", validTestAccount)
		h.AccountMovements(httptest.NewRecorder(), r)
		if !wrote {
			t.Fatal("handler wrote no JSON body")
		}
		return degraded
	}

	if !run(failingSEP41Tail{}) {
		t.Fatal("tail read failed but the page was served with degraded=false")
	}
	if run(&stubSEP41Tail{}) {
		t.Fatal("healthy tail read must not be degraded")
	}
}
