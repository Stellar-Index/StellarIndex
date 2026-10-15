package redact

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"https://eth-mainnet.g.alchemy.com/v2/SECRETKEY": "https://eth-mainnet.g.alchemy.com/<redacted>",
		"http://localhost:8000?key=SECRET":               "http://localhost:8000/<redacted>",
		"SECRET-not-a-url":                               "[redacted-endpoint]",
		"%zz":                                            "[redacted-endpoint]",
	} {
		if got := Endpoint(in); got != want {
			t.Errorf("Endpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestURLError(t *testing.T) {
	ep := "https://rpc.example.com/v2/SECRETKEY"
	err := &url.Error{Op: "Post", URL: ep, Err: errors.New("dial tcp: timeout")}
	got := URLError(err, ep)
	if strings.Contains(got, "SECRETKEY") || !strings.Contains(got, "rpc.example.com") || !strings.Contains(got, "dial tcp: timeout") {
		t.Errorf("URLError = %q", got)
	}
	if got := URLError(errors.New("plain"), ep); got != "plain" {
		t.Errorf("non-url error = %q, want unchanged", got)
	}
}
