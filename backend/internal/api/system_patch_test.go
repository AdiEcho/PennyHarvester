package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"pennyharvester/backend/internal/market"
)

func TestFlatPolymarketPatchWinsOverVenuePatch(t *testing.T) {
	handler, repository := newSystemRouter(t)
	ctx := context.Background()

	save := func(t *testing.T, payload map[string]any) systemResponse {
		t.Helper()
		response := request(t, handler, http.MethodPut, "/api/v1/system", payload)
		if response.Code != http.StatusOK {
			t.Fatalf("save failed: %d %s", response.Code, response.Body.String())
		}
		var body systemResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	flat := func(gamma, clob string) map[string]any {
		return map[string]any{"data_source": map[string]any{
			"gamma_base_url": gamma, "clob_base_url": clob,
		}}
	}

	save(t, flat("https://gamma-a.example.com", "https://clob-a.example.com"))
	saved := save(t, flat("https://gamma-b.example.com", "https://clob-b.example.com"))
	if saved.DataSource.GammaBaseURL != "https://gamma-b.example.com" || saved.DataSource.CLOBBaseURL != "https://clob-b.example.com" {
		t.Fatalf("second flat save did not take effect: %+v", saved.DataSource)
	}
	stored, err := repository.LoadSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DataSource.GammaBaseURL != "https://gamma-b.example.com" || stored.Venues[market.VenuePolymarket].DataSource.GammaBaseURL != "https://gamma-b.example.com" {
		t.Fatalf("flat and venue views disagree: flat=%+v venue=%+v", stored.DataSource, stored.Venues[market.VenuePolymarket].DataSource)
	}

	// A venue-only patch still reaches the flat runtime view.
	venueOnly := save(t, map[string]any{"venues": map[string]any{
		"polymarket": map[string]any{"data_source": map[string]any{
			"gamma_base_url": "https://gamma-c.example.com", "clob_base_url": "https://clob-c.example.com",
		}},
	}})
	if venueOnly.DataSource.GammaBaseURL != "https://gamma-c.example.com" {
		t.Fatalf("venue-only patch did not reach the flat view: %+v", venueOnly.DataSource)
	}

	// When both shapes are present, the explicit flat patch is authoritative.
	both := save(t, map[string]any{
		"data_source": map[string]any{
			"gamma_base_url": "https://gamma-d.example.com", "clob_base_url": "https://clob-d.example.com",
		},
		"venues": map[string]any{"polymarket": map[string]any{"data_source": map[string]any{
			"gamma_base_url": "https://gamma-stale.example.com", "clob_base_url": "https://clob-stale.example.com",
		}}},
	})
	if both.DataSource.GammaBaseURL != "https://gamma-d.example.com" {
		t.Fatalf("flat patch lost to venue patch: %+v", both.DataSource)
	}

	// Clearing the flat fields must actually clear both representations.
	cleared := save(t, flat("", ""))
	if cleared.DataSource.GammaBaseURL != "" || cleared.DataSource.CLOBBaseURL != "" {
		t.Fatalf("clear did not take effect: %+v", cleared.DataSource)
	}
	stored, err = repository.LoadSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DataSource.GammaBaseURL != "" || stored.Venues[market.VenuePolymarket].DataSource.GammaBaseURL != "" {
		t.Fatalf("clear left stale values: flat=%+v venue=%+v", stored.DataSource, stored.Venues[market.VenuePolymarket].DataSource)
	}
}

func TestConcurrentDisjointSystemPatchesKeepBoth(t *testing.T) {
	handler, repository := newSystemRouter(t)
	ctx := context.Background()
	// Two handlers read the same starting snapshot. Serialized read/merge/write
	// must keep both edits; a handler that merges outside the lock loses one.
	for attempt := 0; attempt < 30; attempt++ {
		base := request(t, handler, http.MethodPut, "/api/v1/system", map[string]any{
			"proxy":       map[string]any{"mode": "off"},
			"data_source": map[string]any{"gamma_base_url": "https://gamma-a.example.com"},
		})
		if base.Code != http.StatusOK {
			t.Fatalf("seed failed: %d %s", base.Code, base.Body.String())
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		statuses := make([]int, 2)
		bodies := make([]string, 2)
		run := func(index int, payload map[string]any) {
			defer wg.Done()
			<-start
			response := request(t, handler, http.MethodPut, "/api/v1/system", payload)
			statuses[index], bodies[index] = response.Code, response.Body.String()
		}
		wg.Add(2)
		go run(0, map[string]any{"data_source": map[string]any{"gamma_base_url": "https://gamma-b.example.com"}})
		go run(1, map[string]any{"proxy": map[string]any{"mode": "off", "no_proxy": "127.0.0.1"}})
		close(start)
		wg.Wait()
		for i, code := range statuses {
			if code != http.StatusOK {
				t.Fatalf("attempt %d patch %d failed: %d %s", attempt, i, code, bodies[i])
			}
		}
		stored, err := repository.LoadSystemSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if stored.DataSource.GammaBaseURL != "https://gamma-b.example.com" {
			t.Fatalf("attempt %d lost endpoint patch: %+v", attempt, stored.DataSource)
		}
	}
}
