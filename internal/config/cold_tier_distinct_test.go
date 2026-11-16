package config

import "testing"

// A cold tier that is the hot archive under any spelling is no second copy; a different host or bucket path is.
func TestColdTierIsHotTier(t *testing.T) {
	for name, tc := range map[string]struct {
		hotEndpoint, hotBucket, coldEndpoint, coldBucket string
		want                                             bool
	}{
		"identical":                {"http://127.0.0.1:9000", "galexie-archive", "http://127.0.0.1:9000", "galexie-archive", true},
		"trailing slashes":         {"http://127.0.0.1:9000", "galexie-archive", "http://127.0.0.1:9000/", "galexie-archive/", true},
		"localhost for loopback":   {"http://127.0.0.1:9000", "galexie-archive", "http://localhost:9000", "galexie-archive", true},
		"ipv6 loopback":            {"http://127.0.0.1:9000", "galexie-archive", "http://[::1]:9000", "galexie-archive", true},
		"default port spelled out": {"https://minio.example", "galexie-archive", "https://MINIO.example:443", "galexie-archive", true},
		"dot segment in the path":  {"http://127.0.0.1:9000", "galexie-archive/mirror", "http://127.0.0.1:9000", "galexie-archive/./mirror", true},
		"another port":             {"http://127.0.0.1:9000", "galexie-archive", "http://127.0.0.1:9100", "galexie-archive", false},
		"another host":             {"http://127.0.0.1:9000", "galexie-archive", "https://s3.us-east-2.amazonaws.com", "galexie-archive", false},
		"another prefix":           {"http://127.0.0.1:9000", "galexie-archive", "http://127.0.0.1:9000", "galexie-archive/copy", false},
		"cold tier off":            {"http://127.0.0.1:9000", "galexie-archive", "http://127.0.0.1:9000", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			s := StorageConfig{
				S3Endpoint: tc.hotEndpoint, S3BucketArchive: tc.hotBucket,
				S3ColdEndpoint: tc.coldEndpoint, S3ColdBucketArchive: tc.coldBucket,
			}
			if got := s.ColdTierIsHotTier(); got != tc.want {
				t.Errorf("ColdTierIsHotTier() = %v, want %v", got, tc.want)
			}
		})
	}
}
