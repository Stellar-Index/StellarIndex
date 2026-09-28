package keyedguard

import (
	"net/http"
	"time"
)

func fetchPublic(url string) (*http.Response, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	return client.Get(url)
}
