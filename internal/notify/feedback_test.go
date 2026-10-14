package notify_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/notify"
)

func TestFeedbackHashes(t *testing.T) {
	bounce := func(typ string) string {
		return `{"type":"email.bounced","data":{"to":["Bounced@Example.com"],"bounce":{"type":"` + typ + `","subType":"General"}}}`
	}
	want := notify.RecipientHash("bounced@example.com")
	cases := []struct {
		name, payload string
		suppress      bool
	}{
		{"permanent bounce", bounce("Permanent"), true},
		{"bounce without type", bounce(""), true},
		{"temporary bounce", bounce("Temporary"), false},
		{"complaint", `{"type":"email.complained","data":{"to":["bounced@example.com"]}}`, true},
		{"delivered", `{"type":"email.delivered","data":{"to":["bounced@example.com"]}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := notify.FeedbackHashes([]byte(tc.payload))
			if err != nil {
				t.Fatal(err)
			}
			ok := len(got) == 0
			if tc.suppress {
				ok = len(got) == 1 && got[0] == want
			}
			if !ok {
				t.Fatalf("got %v, suppress=%v", got, tc.suppress)
			}
		})
	}
}

func TestFeedbackHashes_FeedSuppressionList(t *testing.T) {
	hashes, err := notify.FeedbackHashes([]byte(`{"type":"email.complained","data":{"to":["a@example.com"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	inner := &notify.NoopSender{}
	s, err := notify.WithSuppression(inner, hashes)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Send(context.Background(), notify.Message{From: "x <hello@stellarindex.io>", To: []string{"a@example.com"}, Subject: "s", Text: "t"})
	if !errors.Is(err, notify.ErrSuppressed) {
		t.Fatalf("err = %v, want ErrSuppressed", err)
	}
	if inner.SentCount() != 0 {
		t.Fatalf("inner sent %d", inner.SentCount())
	}
}

func TestFeedbackHashes_RejectsBadInput(t *testing.T) {
	for _, p := range []string{`not json`, `{"type":"email.complained","data":{"to":["not an address"]}}`} {
		if _, err := notify.FeedbackHashes([]byte(p)); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
}
