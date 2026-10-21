// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// selfPrewarmAdmitted reports whether authMode lets the credential-less self-prewarm through Auth; under apikey and sep10 it would 401
// and burn the loopback failed-auth budget. Unknown modes are refused.
func selfPrewarmAdmitted(authMode string) bool {
	switch authMode {
	case "", "none", "apikey_optional":
		return true
	}
	return false
}

// selfPrewarmAssetEndpoints GETs every [selfPrewarmPaths] path every 60 s against our own listener, warming every cache the real
// handler touches with byte-identical arguments.
func selfPrewarmAssetEndpoints(ctx context.Context, logger *slog.Logger, listenAddr string, verifiedAssetIDs []string) {
	// Guarded here because it runs on a detached goroutine and exercises more code than any other
	// warmer.
	defer recoverBackgroundWorker(logger, "self-prewarm")
	select {
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
		return
	}

	baseURL := fmt.Sprintf("http://%s", listenAddr)
	client := &http.Client{Timeout: 30 * time.Second}
	paths := selfPrewarmPaths(verifiedAssetIDs)

	get := func(path string) *http.Response {
		start := time.Now()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
		if err != nil {
			logger.Debug("self-prewarm request build failed", "path", path, "err", err)
			return nil
		}
		// Marked synthetic so obs.HTTPMetrics keeps these cold requests out of the customer
		// latency histogram and SLO.
		req.Header.Set("User-Agent", "stellarindex-prewarm/1")
		resp, err := client.Do(req)
		elapsed := time.Since(start)
		if err != nil {
			if ctx.Err() == nil {
				logger.Debug("self-prewarm GET failed", "path", path, "err", err, "elapsed", elapsed.String())
			}
			return nil
		}
		logger.Debug("self-prewarm", "path", path, "status", resp.StatusCode, "elapsed", elapsed.String())
		return resp
	}
	warm := func(path string) {
		if resp := get(path); resp != nil {
			_ = resp.Body.Close()
		}
	}
	runPass := func() {
		for _, path := range paths {
			if ctx.Err() != nil {
				return
			}
			warm(path)
		}
	}

	// 60 s matches lightCadence so the readers' 1-2 minute TTLs do not lapse between cycles.
	runPass()
	warmExplorerBuildAssets(ctx, get, warm)
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runPass()
		}
	}
}

// explorerBuildListingPath is the listing whose every row the explorer's static build fetches as /v1/assets/<id>, minutes
// after each deploy restart. Cold, each detail read took ~0.8 s and the burst tripped the latency SLO.
const explorerBuildListingPath = "/v1/assets?limit=500"

// explorerBuildWarmConcurrency bounds the one-shot detail warm so it cannot crowd out real traffic.
const explorerBuildWarmConcurrency = 8

// warmExplorerBuildAssets warms /v1/assets/<id> once for every [explorerBuildListingPath] row. Once is enough: the readers serve
// stale values while one fill refreshes them, so only the first read after a restart is slow.
func warmExplorerBuildAssets(ctx context.Context, get func(string) *http.Response, warm func(string)) {
	resp := get(explorerBuildListingPath)
	if resp == nil {
		return
	}
	ids, err := listingAssetIDs(resp)
	if err != nil {
		return
	}
	sem := make(chan struct{}, explorerBuildWarmConcurrency)
	var wg sync.WaitGroup
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(id string) {
			defer func() { <-sem; wg.Done() }()
			warm("/v1/assets/" + url.PathEscape(id))
		}(id)
	}
	wg.Wait()
}

// listingAssetIDs reads the asset_id of each row of a /v1/assets listing response and closes its body.
func listingAssetIDs(resp *http.Response) ([]string, error) {
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing status %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			AssetID string `json:"asset_id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(body.Data))
	for _, row := range body.Data {
		if row.AssetID != "" {
			ids = append(ids, row.AssetID)
		}
	}
	return ids, nil
}

// explorerAssetListingPaths are the /v1/assets listing reads explorer pages fire on first load (home, /assets, /rwa), copied from
// their hooks' default params. The listing's response cache lapses after 2 minutes idle, and a cold build with sparklines takes
// seconds, so the first visitor after a quiet spell would pay it.
var explorerAssetListingPaths = []string{
	"/v1/assets?asset_class=all&limit=100&include=sparkline7d&include_thin=true",
	"/v1/assets?limit=10&order_by=volume_24h_usd_desc&include_thin=true",
	"/v1/assets?limit=50",
	"/v1/assets?asset_class=stablecoin&limit=100",
}

// selfPrewarmPaths is one pass of the self-prewarm: /v1/assets/<id> for native (the explorer's landing) and each verified currency,
// the explorer's listing reads, and one price batch over the same assets (batch rows read Postgres uncached; cold after a restart).
func selfPrewarmPaths(verifiedAssetIDs []string) []string {
	// The explorer opts into include_thin, which is its own cache entry. The entry-changes first page is
	// one cache entry at any limit, and a liquid asset's walk outlasts a cold page view.
	queries := []string{"", "?include_thin=true", "/entry-changes"}
	targets := append([]string{"native"}, verifiedAssetIDs...)
	paths := make([]string, 0, len(targets)*len(queries)+len(explorerAssetListingPaths))
	for _, id := range targets {
		for _, query := range queries {
			paths = append(paths, "/v1/assets/"+id+query)
		}
	}
	paths = append(paths, explorerAssetListingPaths...)
	return append(paths, "/v1/price/batch?asset_ids="+strings.Join(targets, ","))
}
