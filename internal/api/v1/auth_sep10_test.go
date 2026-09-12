package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
)

// stubSEP10Validator implements auth.SEP10Validator for tests.
// Each method delegates to a function field so tests can wire
// per-test behaviour without subclassing.
type stubSEP10Validator struct {
	challenge func(ctx context.Context, account string) (auth.Challenge, error)
	verify    func(ctx context.Context, signedXDR string) (auth.Token, error)
	verifyJWT func(ctx context.Context, jwt string) (auth.Subject, error)
}

func (s *stubSEP10Validator) Challenge(ctx context.Context, account string) (auth.Challenge, error) {
	return s.challenge(ctx, account)
}

func (s *stubSEP10Validator) Verify(ctx context.Context, signedXDR string) (auth.Token, error) {
	return s.verify(ctx, signedXDR)
}

func (s *stubSEP10Validator) VerifyJWT(ctx context.Context, jwt string) (auth.Subject, error) {
	if s.verifyJWT == nil {
		return auth.Subject{}, errors.New("not used in this test")
	}
	return s.verifyJWT(ctx, jwt)
}

// TestSEP10Challenge_NoValidator_503 — without a validator wired
// the endpoint returns a 503 with the expected error type.
func TestSEP10Challenge_NoValidator_503(t *testing.T) {
	srv := v1.New(v1.Options{}) // no SEP10
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/auth/sep10/challenge?account=GBLAH")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, "sep10-unavailable") {
		t.Errorf("error type missing: %s", body)
	}
}

// TestSEP10Challenge_MissingAccount_400 — empty account query
// returns 400.
func TestSEP10Challenge_MissingAccount_400(t *testing.T) {
	srv := v1.New(v1.Options{
		SEP10: &stubSEP10Validator{
			challenge: func(_ context.Context, _ string) (auth.Challenge, error) {
				t.Error("Challenge should not be invoked without an account")
				return auth.Challenge{}, nil
			},
		},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/auth/sep10/challenge")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestSEP10Challenge_InvalidAccount_400 — validator returns
// ErrUnauthorized; handler maps to 400.
func TestSEP10Challenge_InvalidAccount_400(t *testing.T) {
	srv := v1.New(v1.Options{
		SEP10: &stubSEP10Validator{
			challenge: func(_ context.Context, _ string) (auth.Challenge, error) {
				return auth.Challenge{}, auth.ErrUnauthorized
			},
		},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/auth/sep10/challenge?account=not-a-strkey")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestSEP10Challenge_HappyPath — body carries the SEP-10 wire fields
// (transaction + network_passphrase) plus convenience timestamps.
func TestSEP10Challenge_HappyPath(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	srv := v1.New(v1.Options{
		SEP10: &stubSEP10Validator{
			challenge: func(_ context.Context, _ string) (auth.Challenge, error) {
				return auth.Challenge{
					TransactionXDR:    "FAKE-XDR-BASE64==",
					NetworkPassphrase: "Test SDF Network ; September 2015",
					IssuedAt:          now,
					ValidUntil:        now.Add(15 * time.Minute),
				}, nil
			},
		},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/auth/sep10/challenge?account=GAIN")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := readAll(resp)
	for _, want := range []string{
		`"transaction":"FAKE-XDR-BASE64=="`,
		`"network_passphrase":"Test SDF Network ; September 2015"`,
		`"issued_at":"2026-04-28T12:00:00Z"`,
		`"valid_until":"2026-04-28T12:15:00Z"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n  body=%s", want, body)
		}
	}
}

// TestSEP10Token_NoValidator_503 — without a validator wired the
// endpoint returns 503.
func TestSEP10Token_NoValidator_503(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustPostJSON(t, ts.URL+"/v1/auth/sep10/token", `{"transaction":"X"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

// TestSEP10Token_MissingBody_400 — empty body returns 400.
func TestSEP10Token_MissingBody_400(t *testing.T) {
	srv := v1.New(v1.Options{
		SEP10: &stubSEP10Validator{
			verify: func(_ context.Context, _ string) (auth.Token, error) {
				t.Error("Verify should not be invoked with empty body")
				return auth.Token{}, nil
			},
		},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustPostJSON(t, ts.URL+"/v1/auth/sep10/token", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestSEP10Token_MalformedJSON_400 — bad JSON returns 400.
func TestSEP10Token_MalformedJSON_400(t *testing.T) {
	srv := v1.New(v1.Options{
		SEP10: &stubSEP10Validator{
			verify: func(_ context.Context, _ string) (auth.Token, error) {
				return auth.Token{}, nil
			},
		},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustPostJSON(t, ts.URL+"/v1/auth/sep10/token", "{not-json")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestSEP10Token_MissingTransaction_400 — body present but no
// transaction field returns 400.
func TestSEP10Token_MissingTransaction_400(t *testing.T) {
	srv := v1.New(v1.Options{
		SEP10: &stubSEP10Validator{
			verify: func(_ context.Context, _ string) (auth.Token, error) {
				return auth.Token{}, nil
			},
		},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustPostJSON(t, ts.URL+"/v1/auth/sep10/token", `{}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestSEP10Token_HappyPath — Validator succeeds; response carries
// token + expires_at + account.
func TestSEP10Token_HappyPath(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	srv := v1.New(v1.Options{
		SEP10: &stubSEP10Validator{
			verify: func(_ context.Context, _ string) (auth.Token, error) {
				return auth.Token{
					JWT:       "fake.jwt.token",
					IssuedAt:  now,
					ExpiresAt: now.Add(time.Hour),
					Subject: auth.Subject{
						Identifier: "GCLIENT",
						Tier:       auth.TierSEP10,
						CreatedAt:  now,
					},
				}, nil
			},
		},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustPostJSON(t, ts.URL+"/v1/auth/sep10/token", `{"transaction":"signed-xdr"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := readAll(resp)
	for _, want := range []string{
		`"token":"fake.jwt.token"`,
		`"expires_at":"2026-04-28T13:00:00Z"`,
		`"account":"GCLIENT"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n  body=%s", want, body)
		}
	}
}

// TestSEP10Token_ExpiredChallenge_410 — ErrTokenExpired surfaces
// 410 Gone (the challenge can't be retried — the client must
// request a fresh one).
func TestSEP10Token_ExpiredChallenge_410(t *testing.T) {
	srv := v1.New(v1.Options{
		SEP10: &stubSEP10Validator{
			verify: func(_ context.Context, _ string) (auth.Token, error) {
				return auth.Token{}, auth.ErrTokenExpired
			},
		},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustPostJSON(t, ts.URL+"/v1/auth/sep10/token", `{"transaction":"signed-xdr"}`)
	if resp.StatusCode != http.StatusGone {
		t.Errorf("status = %d, want 410", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, "sep10-challenge-expired") {
		t.Errorf("error type missing: %s", body)
	}
}

// TestSEP10Token_MalformedXDR_400 — ErrTokenMalformed → 400.
func TestSEP10Token_MalformedXDR_400(t *testing.T) {
	srv := v1.New(v1.Options{
		SEP10: &stubSEP10Validator{
			verify: func(_ context.Context, _ string) (auth.Token, error) {
				return auth.Token{}, auth.ErrTokenMalformed
			},
		},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustPostJSON(t, ts.URL+"/v1/auth/sep10/token", `{"transaction":"junk"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestSEP10Token_VerificationFailed_401 — ErrUnauthorized → 401.
func TestSEP10Token_VerificationFailed_401(t *testing.T) {
	srv := v1.New(v1.Options{
		SEP10: &stubSEP10Validator{
			verify: func(_ context.Context, _ string) (auth.Token, error) {
				return auth.Token{}, auth.ErrUnauthorized
			},
		},
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustPostJSON(t, ts.URL+"/v1/auth/sep10/token", `{"transaction":"signed"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// TestSEP10Unavailable_TellsTheCallerWhatToDoInstead pins the BODY of
// the 503, not just its code.
//
// Four separate branches produce this answer — no validator wired on
// either route, and ErrNotImplemented surfacing from Challenge or
// Verify — and they used to carry two different strings. The terser
// one said only "this deployment has no SEP-10 validator wired",
// which is the error code restated in prose: a caller reading it
// learns nothing they can act on, and cannot tell a permanent
// deployment posture from an outage worth retrying.
//
// Every document that once promised the flow now says it is off here
// (/sdk, docs/getting-started.md, pkg/client's godoc, the spec). This
// response is the surface that answers a caller who read none of
// them, so it has to carry the same two facts on its own: WHY it
// refuses, and WHAT WORKS INSTEAD.
//
// The assertions are about substance rather than wording, so the
// sentence can be rewritten without a test edit — but it cannot be
// hollowed back out to a bare code, and the four branches cannot
// drift apart from each other again.
func TestSEP10Unavailable_TellsTheCallerWhatToDoInstead(t *testing.T) {
	notImplemented := func() *stubSEP10Validator {
		return &stubSEP10Validator{
			challenge: func(context.Context, string) (auth.Challenge, error) {
				return auth.Challenge{}, auth.ErrNotImplemented
			},
			verify: func(context.Context, string) (auth.Token, error) {
				return auth.Token{}, auth.ErrNotImplemented
			},
		}
	}

	cases := []struct {
		name string
		opts v1.Options
		call func(t *testing.T, base string) *http.Response
	}{
		{
			name: "challenge/no validator wired",
			opts: v1.Options{},
			call: func(t *testing.T, base string) *http.Response {
				return mustGet(t, base+"/v1/auth/sep10/challenge?account=GAIN")
			},
		},
		{
			name: "challenge/validator returns ErrNotImplemented",
			opts: v1.Options{SEP10: notImplemented()},
			call: func(t *testing.T, base string) *http.Response {
				return mustGet(t, base+"/v1/auth/sep10/challenge?account=GAIN")
			},
		},
		{
			name: "token/no validator wired",
			opts: v1.Options{},
			call: func(t *testing.T, base string) *http.Response {
				return mustPostJSON(t, base+"/v1/auth/sep10/token", `{"transaction":"signed-xdr"}`)
			},
		},
		{
			name: "token/validator returns ErrNotImplemented",
			opts: v1.Options{SEP10: notImplemented()},
			call: func(t *testing.T, base string) *http.Response {
				return mustPostJSON(t, base+"/v1/auth/sep10/token", `{"transaction":"signed-xdr"}`)
			},
		},
	}

	details := make(map[string]string, len(cases))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := startHTTPTest(t, v1.New(tc.opts).Handler())
			resp := tc.call(t, ts.URL)
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", resp.StatusCode)
			}
			body, _ := readAll(resp)

			var prob struct {
				Type   string `json:"type"`
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal([]byte(body), &prob); err != nil {
				t.Fatalf("decode problem body: %v\n  body=%s", err, body)
			}
			if !strings.HasSuffix(prob.Type, "/sep10-unavailable") {
				t.Fatalf("type = %q, want .../sep10-unavailable", prob.Type)
			}
			if prob.Detail == "" {
				t.Fatal("the 503 carries an empty detail — the code is all the caller gets")
			}
			details[tc.name] = prob.Detail

			// Why it refuses. Not an outage: a deployment posture.
			if !strings.Contains(prob.Detail, "signing seed") {
				t.Errorf("detail does not name the missing signing seed as the cause: %q", prob.Detail)
			}
			// What works instead. This is the half a bare code cannot
			// carry, and the half that makes the answer actionable.
			if !strings.Contains(prob.Detail, "API key") {
				t.Errorf("detail does not point the caller at the credential this deployment "+
					"does verify: %q", prob.Detail)
			}
			// Enabling SEP-10 SWAPS the deployment's credential type
			// rather than adding one — an operator who reads this as
			// "turn it on as well" breaks every existing key holder.
			if !strings.Contains(prob.Detail, "never both") {
				t.Errorf("detail does not say API keys and SEP-10 JWTs are mutually exclusive: %q",
					prob.Detail)
			}
		})
	}

	if len(details) != len(cases) {
		t.Fatalf("collected %d details from %d branches — a subtest failed before recording, "+
			"so the agreement check below would be vacuous", len(details), len(cases))
	}
	var first, firstName string
	for _, tc := range cases {
		got := details[tc.name]
		if first == "" {
			first, firstName = got, tc.name
			continue
		}
		if got != first {
			t.Errorf("the 503 body differs by branch — %q says:\n  %s\n%q says:\n  %s",
				firstName, first, tc.name, got)
		}
	}
}
