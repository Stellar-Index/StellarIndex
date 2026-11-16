package auth

import "testing"

func TestRedactKeySecrets(t *testing.T) {
	for in, want := range map[string]string{
		"/v1/account/keys/kid_ab12":                 "/v1/account/keys/kid_ab12",
		"/v1/account/keys/sip_ab12cd34":             "/v1/account/keys/sip_[redacted]",
		"/v1/admin/keys/sip_ab12?identifier=acct:x": "/v1/admin/keys/sip_[redacted]?identifier=acct:x",
		"/a/sip_AA11/b/sip_bb22":                    "/a/sip_[redacted]/b/sip_[redacted]",
		"sip_":                                      "sip_[redacted]",
		"":                                          "",
	} {
		if got := RedactKeySecrets(in); got != want {
			t.Errorf("RedactKeySecrets(%q) = %q, want %q", in, got, want)
		}
	}
	if !IsKeySecret("sip_ab12") || IsKeySecret("kid_ab12") || IsKeySecret("") {
		t.Error("IsKeySecret must accept only the plaintext-key shape")
	}
}
