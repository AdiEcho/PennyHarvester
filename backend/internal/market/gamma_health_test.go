package market

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func gammaTestMarketBody(id, tokenYes, tokenNo, priceYes, priceNo string) string {
	return fmt.Sprintf(`{
		"id":%q,"conditionId":"condition-%s","question":"Question?","slug":"slug-%s",
		"outcomes":["Yes","No"],"outcomePrices":[%s,%s],
		"clobTokenIds":[%q,%q],"endDate":"2099-01-01T00:00:00Z",
		"active":true,"closed":false
	}`, id, id, id, priceYes, priceNo, tokenYes, tokenNo)
}

func gammaTestEventBody(market string) string {
	return `{"events":[{"id":"ev-1","slug":"event-1","title":"Event","markets":[` + market + `]}]}`
}

func gammaTestCacheKey(baseURL string, maxDays int, view gammaView) string {
	return fmt.Sprintf("%s|%d|%s|%s|%d|%s", baseURL, maxDays, view.order, view.ascending, view.pages, view.horizon)
}

func TestGammaClientStaleFallbackPreservesFetchAge(t *testing.T) {
	var failing atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if failing.Load() {
			http.Error(w, "upstream down", http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/events/keyset") {
			_, _ = w.Write([]byte(gammaTestEventBody(gammaTestMarketBody("tail", "tail-yes", "tail-no", "0.5", "0.5"))))
			return
		}
		id := "near"
		switch r.URL.Query().Get("order") {
		case "volume24hr":
			id = "volume"
		case "liquidity":
			id = "liquidity"
		}
		_, _ = w.Write([]byte(keysetBody(`[` + gammaTestMarketBody(id, id+"-yes", id+"-no", "0.6", "0.4") + `]`)))
	}))
	defer server.Close()

	t0 := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	client := NewGammaClient(server.URL, nil)
	client.clock = func() time.Time { return t0 }

	first, err := client.ListOpen(context.Background(), 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 4 {
		t.Fatalf("expected all four views on a cold scan, got %+v", first)
	}
	for _, item := range first {
		if item.Stale || !item.FetchedAt.Equal(t0) {
			t.Fatalf("fresh row has stale state: %+v", item)
		}
	}

	volumeView := gammaView{order: "volume24hr", ascending: "false", pages: 2, cache: true}
	liquidityView := gammaView{order: "liquidity", ascending: "false", pages: 2, cache: true}
	volumeKey := gammaTestCacheKey(server.URL, 30, volumeView)
	liquidityKey := gammaTestCacheKey(server.URL, 30, liquidityView)

	client.cacheMu.Lock()
	volumeBefore := client.viewCache[volumeKey]
	liquidityBefore := client.viewCache[liquidityKey]
	client.cacheMu.Unlock()
	if !volumeBefore.fetchedAt.Equal(t0) || !liquidityBefore.fetchedAt.Equal(t0) {
		t.Fatalf("broad views were not cached at their fetch time: %+v %+v", volumeBefore, liquidityBefore)
	}

	t1 := t0.Add(broadViewTTL + time.Minute)
	client.clock = func() time.Time { return t1 }
	failing.Store(true)

	stale, err := client.ListOpen(context.Background(), 30)
	if err != nil {
		t.Fatalf("a stale broad cache must keep the scan usable: %v", err)
	}
	if len(stale) != 2 {
		t.Fatalf("expected the two stale broad rows, got %+v", stale)
	}
	for _, item := range stale {
		if !item.Stale {
			t.Fatalf("stale fallback row was returned as fresh: %+v", item)
		}
		if item.StaleReason == "" || !strings.Contains(item.StaleReason, "503") {
			t.Fatalf("stale row lost the upstream reason: %+v", item)
		}
		if !item.FetchedAt.Equal(t0) {
			t.Fatalf("stale row fetch time moved from %s to %s", t0, item.FetchedAt)
		}
	}

	health := client.Health()
	if !health.Degraded || health.StaleViews != 2 || health.Rejected != 0 {
		t.Fatalf("unexpected health after stale fallback: %+v", health)
	}
	if health.Reason == "" || !health.At.Equal(t1) {
		t.Fatalf("health did not describe the latest attempt: %+v", health)
	}

	client.cacheMu.Lock()
	volumeAfter := client.viewCache[volumeKey]
	liquidityAfter := client.viewCache[liquidityKey]
	client.cacheMu.Unlock()
	for _, entry := range []gammaViewCache{volumeAfter, liquidityAfter} {
		if !entry.fetchedAt.Equal(t0) {
			t.Fatalf("a failed refresh renewed the cache age: %+v", entry)
		}
		if !entry.lastAttemptAt.Equal(t1) {
			t.Fatalf("lastAttemptAt was not recorded separately: %+v", entry)
		}
	}
	if _, ok := client.cachedView(volumeKey, broadViewTTL, t1); ok {
		t.Fatal("a stale fallback must not satisfy a later fresh TTL check")
	}
	if _, ok := client.cachedView(liquidityKey, broadViewTTL, t1); ok {
		t.Fatal("a stale fallback must not satisfy a later fresh TTL check")
	}
}

func TestParseGammaMarketRejectsOutOfRangePrices(t *testing.T) {
	for _, prices := range []string{
		`[-0.2,1.2]`,
		`[1.2,-0.2]`,
		`[1.0001,0]`,
		`[-0.0001,1]`,
		`["NaN","0.5"]`,
		`["Inf","0.5"]`,
	} {
		raw := gammaTestValidRaw()
		raw.OutcomePrices = json.RawMessage(prices)
		if parsed, err := parseGammaMarket(raw); err == nil {
			t.Fatalf("prices %s were accepted as %+v", prices, parsed)
		}
	}

	// The 0 and 1 boundaries are legitimate settlement prices.
	for _, prices := range []string{`[0,1]`, `[1,0]`} {
		raw := gammaTestValidRaw()
		raw.OutcomePrices = json.RawMessage(prices)
		if _, err := parseGammaMarket(raw); err != nil {
			t.Fatalf("boundary prices %s must parse: %v", prices, err)
		}
	}
}

func TestParseGammaMarketRejectsBadOutcomeIdentity(t *testing.T) {
	cases := []struct {
		name     string
		outcomes string
		tokens   string
	}{
		{name: "duplicate token", tokens: `["same","same"]`},
		{name: "empty token", tokens: `["yes",""]`},
		{name: "whitespace token", tokens: `["yes","   "]`},
		{name: "empty name", outcomes: `["Yes",""]`},
		{name: "whitespace name", outcomes: `["Yes","  "]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := gammaTestValidRaw()
			if tc.outcomes != "" {
				raw.Outcomes = json.RawMessage(tc.outcomes)
			}
			if tc.tokens != "" {
				raw.ClobTokenIDs = json.RawMessage(tc.tokens)
			}
			parsed, err := parseGammaMarket(raw)
			if err == nil {
				t.Fatalf("invalid outcome was accepted: %+v", parsed)
			}
			if parsed.ID != "" {
				t.Fatalf("a rejected row must not leak a partially parsed market: %+v", parsed)
			}
		})
	}
}

func TestParseGammaMarketInfersProtocolVersion(t *testing.T) {
	cases := []struct {
		name       string
		clobIDs    json.RawMessage
		positionID json.RawMessage
		wantVer    string
		wantToken  string
		wantErr    bool
	}{
		{
			name:       "clob ids imply v1",
			clobIDs:    json.RawMessage(`["clob-yes","clob-no"]`),
			positionID: json.RawMessage(`["pos-yes","pos-no"]`),
			wantVer:    "v1",
			wantToken:  "clob-yes",
		},
		{
			name:       "position ids imply v2 when clob is null",
			clobIDs:    json.RawMessage(`null`),
			positionID: json.RawMessage(`["pos-yes","pos-no"]`),
			wantVer:    "v2",
			wantToken:  "pos-yes",
		},
		{
			name:       "empty clob array falls back to v2",
			clobIDs:    json.RawMessage(`[]`),
			positionID: json.RawMessage(`["pos-yes","pos-no"]`),
			wantVer:    "v2",
			wantToken:  "pos-yes",
		},
		{
			name:    "no ids and no version is rejected",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := gammaTestValidRaw()
			raw.Version = ""
			raw.ClobTokenIDs = tc.clobIDs
			raw.PositionIDs = tc.positionID
			parsed, err := parseGammaMarket(raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected rejection, got %+v", parsed)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if parsed.Version != tc.wantVer {
				t.Fatalf("version = %q, want %q", parsed.Version, tc.wantVer)
			}
			if parsed.Outcomes[0].TokenID != tc.wantToken {
				t.Fatalf("token = %q, want %q", parsed.Outcomes[0].TokenID, tc.wantToken)
			}
			if parsed.Version == "" {
				t.Fatal("token IDs must never be returned with an empty protocol version")
			}
			if tc.wantVer == "v2" && !parsed.IsV2() {
				t.Fatal("inferred v2 must be recognised by IsV2")
			}
		})
	}
}

func TestGammaClientListOpenKeepsGoodRowsAndRecordsHealth(t *testing.T) {
	good := gammaTestMarketBody("good", "good-yes", "good-no", "0.98", "0.02")
	duplicate := gammaTestMarketBody("duplicate", "dup", "dup", "0.5", "0.5")
	emptyToken := gammaTestMarketBody("empty-token", "empty-yes", "", "0.4", "0.6")
	outOfRange := gammaTestMarketBody("out-of-range", "range-yes", "range-no", "1.2", "-0.2")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/events/keyset") {
			_, _ = w.Write([]byte(`{"events":[]}`))
			return
		}
		if r.URL.Query().Get("order") != "endDate" {
			_, _ = w.Write([]byte(keysetBody(`[]`)))
			return
		}
		_, _ = w.Write([]byte(keysetBody(`[` + good + `,` + duplicate + `,` + emptyToken + `,` + outOfRange + `]`)))
	}))
	defer server.Close()

	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	client := NewGammaClient(server.URL, nil)
	client.clock = func() time.Time { return now }

	items, err := client.ListOpen(context.Background(), 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != "good" {
		t.Fatalf("good row did not survive alongside bad rows: %+v", items)
	}
	if items[0].Stale || !items[0].FetchedAt.Equal(now) {
		t.Fatalf("good row is not marked fresh: %+v", items[0])
	}
	health := client.Health()
	if !health.Degraded || health.Rejected != 3 || health.StaleViews != 0 {
		t.Fatalf("rejected rows did not surface in health: %+v", health)
	}
	if health.Reason == "" || !health.At.Equal(now) {
		t.Fatalf("health reason/attempt time missing: %+v", health)
	}
}

func TestGammaClientPartialPageFailureDegradesHealth(t *testing.T) {
	good := gammaTestMarketBody("good", "good-yes", "good-no", "0.98", "0.02")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/events/keyset") {
			_, _ = w.Write([]byte(`{"events":[]}`))
			return
		}
		if r.URL.Query().Get("order") != "endDate" {
			_, _ = w.Write([]byte(keysetBody(`[]`)))
			return
		}
		if r.URL.Query().Get("after_cursor") == "" {
			_, _ = w.Write([]byte(`{"markets":[` + good + `],"next_cursor":"next"}`))
			return
		}
		http.Error(w, "cursor page failed", http.StatusBadGateway)
	}))
	defer server.Close()

	client := NewGammaClient(server.URL, nil)
	items, err := client.ListOpen(context.Background(), 30)
	if err != nil {
		t.Fatalf("a partial page failure must keep the first page: %v", err)
	}
	if len(items) != 1 || items[0].ID != "good" {
		t.Fatalf("first page lost: %+v", items)
	}
	health := client.Health()
	if !health.Degraded || health.Rejected != 0 {
		t.Fatalf("partial page failure did not surface in health: %+v", health)
	}
	if health.Reason == "" {
		t.Fatal("partial page failure must leave a reason")
	}
}

func TestGammaClientHealthResetsOnSuccessfulScan(t *testing.T) {
	var healthy atomic.Bool
	bad := gammaTestMarketBody("bad", "dup", "dup", "0.5", "0.5")
	good := gammaTestMarketBody("good", "good-yes", "good-no", "0.98", "0.02")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := bad
		if healthy.Load() {
			body = good
		}
		if strings.HasSuffix(r.URL.Path, "/events/keyset") {
			_, _ = w.Write([]byte(gammaTestEventBody(body)))
			return
		}
		_, _ = w.Write([]byte(keysetBody(`[` + body + `]`)))
	}))
	defer server.Close()

	t0 := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(broadViewTTL + time.Minute)
	client := NewGammaClient(server.URL, nil)
	client.clock = func() time.Time { return t0 }

	items, err := client.ListOpen(context.Background(), 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("bad rows must not enter the result: %+v", items)
	}
	before := client.Health()
	if !before.Degraded || before.Rejected == 0 || !before.At.Equal(t0) {
		t.Fatalf("bad scan health not recorded: %+v", before)
	}

	client.clock = func() time.Time { return t1 }
	healthy.Store(true)
	items, err = client.ListOpen(context.Background(), 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != "good" || items[0].Stale {
		t.Fatalf("recovered scan did not return a fresh row: %+v", items)
	}
	after := client.Health()
	if after.Degraded || after.Rejected != 0 || after.StaleViews != 0 || after.Reason != "" {
		t.Fatalf("health did not reset on a successful scan: %+v", after)
	}
	if !after.At.Equal(t1) {
		t.Fatalf("health attempt time = %s, want %s", after.At, t1)
	}
}

func TestGammaClientHealthBeforeFirstScan(t *testing.T) {
	client := NewGammaClient("http://example.invalid", nil)
	health := client.Health()
	if health.Degraded || health.Rejected != 0 || health.StaleViews != 0 {
		t.Fatalf("unexpected initial health: %+v", health)
	}
}

func gammaTestValidRaw() gammaMarket {
	return gammaMarket{
		ID:            "1",
		Version:       "v1",
		Outcomes:      json.RawMessage(`["Yes","No"]`),
		OutcomePrices: json.RawMessage(`[0.9,0.1]`),
		ClobTokenIDs:  json.RawMessage(`["yes","no"]`),
		EndDate:       "2099-01-01T00:00:00Z",
	}
}
