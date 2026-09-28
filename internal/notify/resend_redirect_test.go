// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package notify_test

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/httpx/httpxtest"
	"github.com/Stellar-Index/StellarIndex/internal/notify"
)

// net/http keeps Authorization across a redirect that changes only the
// port or scheme of the same hostname.
func TestResendSender_KeyDoesNotFollowAnOffOriginRedirect(t *testing.T) {
	trap := httpxtest.NewRedirectTrap(t, "Authorization")
	s, err := notify.NewResendSender("probe")
	if err != nil {
		t.Fatal(err)
	}
	s.BaseURL = trap.URL
	err = s.Send(context.Background(), notify.Message{
		From: "x@y.com", To: []string{"a@b.com"}, Subject: "s", Text: "t",
	})
	if err == nil {
		t.Error("Send succeeded through a refused redirect")
	}
	trap.AssertKeyStayedOnOrigin(t)
}
