package keyedguard

import (
	"context"
	"net/http"
	"time"
)

func fetchKeyed(ctx context.Context, url, key string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-vendor-api-key", key)
	client := &http.Client{Timeout: 30 * time.Second}
	return client.Do(req)
}
