package archive

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"
)

// failingWrites fails the writes numbered in fail (1-based) and records the rest.
type failingWrites struct {
	n    int
	fail map[int]bool
	buf  bytes.Buffer
}

func (w *failingWrites) Write(p []byte) (int, error) {
	w.n++
	if w.fail[w.n] {
		return 0, errors.New("no space left on device")
	}
	return w.buf.Write(p)
}

// A transition the recovery log failed to record must stop the watermark: merge would otherwise read the chunk as scanned
// through and run one WASM hash across the missing upgrade. json.Encoder's sticky error is what holds this today.
func TestTransitionLog_FailedAppendWithholdsTheWatermark(t *testing.T) {
	var contract sdkxdr.Hash
	contract[0] = 0x01
	w := &failingWrites{fail: map[int]bool{2: true}}
	tlog := &transitionLog{enc: json.NewEncoder(w), watch: map[sdkxdr.Hash]string{contract: "CAAA"}}
	state := map[sdkxdr.Hash]*wasmContractState{}

	recordWasmTransition(state, contract, "hash-v1", 100, tlog)
	recordWasmTransition(state, contract, "hash-v2", 200, tlog) // this write fails
	recordWasmTransition(state, contract, "hash-v3", 300, tlog)

	if err := tlog.setExtent(400); err == nil {
		t.Fatal("setExtent after a failed append returned nil, want an error")
	}
	if strings.Contains(w.buf.String(), `"watermark":true`) {
		t.Errorf("watermark written over a gap in the log:\n%s", w.buf.String())
	}
	if got := len(state[contract].ranges); got != 3 {
		t.Errorf("in-memory ranges = %d, want 3: the walk keeps its own state", got)
	}
}
