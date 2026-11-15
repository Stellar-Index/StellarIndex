package v1

import "testing"

// A batch is answered 200 only when the ids left out are known: a row the deadline never let start, or failed reads
// with nothing served, would read as "no price".
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
		{"failed reads and nothing served", []batchRowResult{failed, miss}, true},
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
