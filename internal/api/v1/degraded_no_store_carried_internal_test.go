package v1

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type (
	nilAssets        struct{ AssetsReader }
	nilOracleHistory struct{ RWAOracleHistoryReader }
)

func closedFlight() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func TestRWAHistory_CarriedForwardAssemblyIsNoStore(t *testing.T) {
	s := &Server{Options: Options{AssetsReader: nilAssets{}, OracleHistory: nilOracleHistory{}}}
	s.rwaHistCache = &rwaValueHistory{available: true, builtAt: time.Now().Add(-48 * time.Hour)}
	s.rwaHistAt = time.Now().Add(-48 * time.Hour)
	s.rwaHistFlight = closedFlight()
	rec := httptest.NewRecorder()
	s.handleRWAHistory(rec, httptest.NewRequest(http.MethodGet, "/v1/rwa/history", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("status=%d Cache-Control=%q, want 200 no-store", rec.Code, rec.Header().Get("Cache-Control"))
	}
}

func TestIngestionFlags_PartialSnapshotIsDegraded(t *testing.T) {
	var snap IngestionDiagnostics
	snap.Ledger.LatestLedger = 1
	if f := ingestionFlags(snap); f.Degraded {
		t.Error("healthy snapshot flagged degraded")
	}
	snap.degraded = true
	if f := ingestionFlags(snap); !f.Degraded || !f.Stale {
		t.Errorf("flags = %+v, want stale and degraded", f)
	}
}

type nilMarketHistory struct{ RWAMarketHistoryReader }

func TestRWAPremiumHistory_CarriedForwardAssemblyIsNoStore(t *testing.T) {
	s := &Server{Options: Options{AssetsReader: nilAssets{}, OracleHistory: nilOracleHistory{}, MarketHistory: nilMarketHistory{}}}
	s.rwaPremCache = &rwaPremiumHistory{available: true, builtAt: time.Now().Add(-48 * time.Hour)}
	s.rwaPremAt = time.Now().Add(-48 * time.Hour)
	s.rwaPremFlight = closedFlight()
	rec := httptest.NewRecorder()
	s.handleRWAPremiumHistory(rec, httptest.NewRequest(http.MethodGet, "/v1/rwa/premium-history", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("status=%d Cache-Control=%q, want 200 no-store", rec.Code, rec.Header().Get("Cache-Control"))
	}
}
