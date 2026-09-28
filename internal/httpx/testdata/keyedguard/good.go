package keyedguard

import (
	"context"
	"errors"
	"net/http"
	"time"
)

func fetchGuarded(ctx context.Context, url, key string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("no") },
	}
	return client.Do(req)
}
