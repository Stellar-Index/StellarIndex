package auth

import (
	"context"
	"errors"
	"testing"
)

// TestNoopAPIKeyValidator_FailLoud: auth_mode=apikey without a real validator must fail every request, never silently demote to anonymous.
func TestNoopAPIKeyValidator_FailLoud(t *testing.T) {
	_, err := NoopAPIKeyValidator{}.Lookup(context.Background(), "anything")
	if !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Lookup should return ErrNotImplemented, got %v", err)
	}
}

// TestNoopSEP10Validator_FailLoud: all three protocol functions return ErrNotImplemented, which the middleware translates to 404 so misconfiguration shows on the first request.
func TestNoopSEP10Validator_FailLoud(t *testing.T) {
	v := NoopSEP10Validator{}
	if _, err := v.Challenge(context.Background(), "GA…"); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Challenge: got %v, want ErrNotImplemented", err)
	}
	if _, err := v.Verify(context.Background(), "any-xdr"); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Verify: got %v, want ErrNotImplemented", err)
	}
	if _, err := v.VerifyJWT(context.Background(), "any.jwt.string"); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("VerifyJWT: got %v, want ErrNotImplemented", err)
	}
}
