package diagnostics

import (
	"context"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/option"
)

func TestHubbleBQQueryStampsCap(t *testing.T) {
	client, err := bigquery.NewClient(context.Background(), "p", option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	q := hubbleBQ{client: client, maxBytes: 12345}.query("SELECT 1")
	if q.MaxBytesBilled != 12345 {
		t.Fatalf("MaxBytesBilled = %d, want 12345", q.MaxBytesBilled)
	}
	if defaultMaxBytesBilled < 1 {
		t.Fatalf("defaultMaxBytesBilled = %d; BigQuery reads < 1 as uncapped", defaultMaxBytesBilled)
	}
}
