package v1

import "testing"

// A batch is answered 200 when at least one id has a known answer and every lost one is named: a row the deadline never
// let start, or nothing but lost rows, is a 503.
func TestBatchUnanswered(t *testing.T) {
	ok := batchRowResult{ok: true}
	miss := batchRowResult{skip: true}
	failed := batchRowResult{skip: true, readFailed: true}
	unstarted := batchRowResult{skip: true, readFailed: true, unstarted: true}
	cases := []struct {
		name string
		rows []batchRowResult
		want bool
	}{
		{"all served", []batchRowResult{ok, ok}, false},
		{"a miss beside a served row", []batchRowResult{ok, miss}, false},
		{"only misses", []batchRowResult{miss, miss}, false},
		{"a failed read beside a served row", []batchRowResult{ok, failed}, false},
		{"a failed read beside a known miss", []batchRowResult{failed, miss}, false},
		{"only failed reads", []batchRowResult{failed, failed}, true},
		{"only rows lost to a panic", []batchRowResult{{}, {}}, true},
		{"a row the deadline cut off", []batchRowResult{ok, unstarted}, true},
	}
	for _, c := range cases {
		if got := batchUnanswered(c.rows); got != c.want {
			t.Errorf("%s: unanswered = %v, want %v", c.name, got, c.want)
		}
	}

	env := batchEnvelope([]string{"a", "b", "c"}, []batchRowResult{miss, failed, {}})
	if len(env.Unread) != 2 || env.Unread[0] != "b" || env.Unread[1] != "c" {
		t.Errorf("unread = %v, want the failed read and the lost row [b c]", env.Unread)
	}
}
