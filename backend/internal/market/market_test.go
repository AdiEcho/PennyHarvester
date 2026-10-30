package market

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGammaClientParsesEncodedArrays(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/markets" || r.URL.Query().Get("closed") != "false" {
			t.Errorf("unexpected request: %s", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{
			"id":"42","conditionId":"condition-42","question":"Will it resolve?","slug":"will-it-resolve",
			"outcomes":"[\"Yes\",\"No\"]","outcomePrices":"[\"0.98\",\"0.02\"]",
			"clobTokenIds":"[\"token-yes\",\"token-no\"]","endDate":"2099-01-01T00:00:00Z",
			"liquidity":"25000","volume":"90000","active":true,"closed":false
		}]`))
	}))
	defer server.Close()
	client := NewGammaClient(server.URL, nil)
	items, err := client.ListOpen(context.Background(), 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Outcomes[0].Price != 0.98 || items[0].Liquidity != 25000 {
		t.Fatalf("unexpected market: %+v", items)
	}
}

func TestGammaClientMarketsLocaleRequestsTranslatedText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/markets" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("locale"); got != "zh" {
			t.Errorf("locale = %q, want zh", got)
		}
		if ids := r.URL.Query()["id"]; len(ids) != 2 || ids[0] != "42" || ids[1] != "43" {
			t.Errorf("ids = %v, want [42 43]", ids)
		}
		if got := r.URL.Query().Get("limit"); got != "100" {
			t.Errorf("limit = %q, want 100", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{
			"id":"42","conditionId":"condition-42","question":"会结算吗？","groupItemTitle":"O/U 0.5","slug":"will-it-resolve",
			"outcomes":"[\"是\",\"否\"]","outcomePrices":"[\"0.98\",\"0.02\"]",
			"clobTokenIds":"[\"token-yes\",\"token-no\"]","endDate":"2099-01-01T00:00:00Z",
			"liquidity":"25000","volume":"90000","active":true,"closed":false,
			"events":[{"slug":"will-it-resolve","title":"会结算吗 -更多市场"}]
		}]`))
	}))
	defer server.Close()
	client := NewGammaClient(server.URL, nil)
	items, err := client.MarketsLocale(context.Background(), []string{"42", "43"}, "zh")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Question != "会结算吗？" || items[0].Outcomes[0].Name != "是" {
		t.Fatalf("unexpected localized market: %+v", items)
	}
	if items[0].GroupItemTitle != "O/U 0.5" || items[0].EventTitle != "会结算吗 -更多市场" {
		t.Fatalf("localized context not parsed: %+v", items[0])
	}
}

func TestWinnerRequiresTerminalResolution(t *testing.T) {
	item := binaryForTest(0.99, 0.01)
	item.Closed = true
	item.ResolutionStatus = "resolved"
	if _, ok := item.Winner(); ok {
		t.Fatal("non-terminal prices must not settle a position")
	}
	item.Outcomes[0].Price = 1
	item.Outcomes[1].Price = 0
	winner, ok := item.Winner()
	if !ok || winner.TokenID != "yes" {
		t.Fatalf("expected YES winner: %+v, %v", winner, ok)
	}
	item.ResolutionStatus = "proposed"
	if _, ok := item.Winner(); ok {
		t.Fatal("proposed resolution must not be treated as final")
	}
}

func TestSettlementPriceHandlesEqualSplit(t *testing.T) {
	item := binaryForTest(0.5, 0.5)
	item.Closed = true
	item.ResolutionStatus = "resolved"
	price, ok := item.SettlementPrice("yes")
	if !ok || price != 0.5 {
		t.Fatalf("expected 50/50 paper settlement, got %f, %v", price, ok)
	}
	if _, ok := item.Winner(); ok {
		t.Fatal("equal split must not have a winner")
	}
}

func TestDecodeFloatArraySupportsNumericJSON(t *testing.T) {
	var values []float64
	if err := decodeFloatArray(json.RawMessage(`[0.95,"0.99"]`), &values); err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0] != 0.95 || values[1] != 0.99 {
		t.Fatalf("unexpected values: %v", values)
	}
}

func TestMarketRejectsInvalidEndDate(t *testing.T) {
	raw := gammaMarket{ID: "1", Outcomes: json.RawMessage(`["Yes","No"]`), OutcomePrices: json.RawMessage(`[0.9,0.1]`), ClobTokenIDs: json.RawMessage(`["a","b"]`), EndDate: time.Time{}.Format(time.RFC3339)}
	// Zero time is valid RFC3339, so this verifies parsing rather than silently
	// replacing malformed values; malformed values must return an error.
	if _, err := parseGammaMarket(raw); err != nil {
		t.Fatal(err)
	}
	raw.EndDate = "not-a-date"
	if _, err := parseGammaMarket(raw); err == nil {
		t.Fatal("expected invalid end date error")
	}
}

func binaryForTest(yes, no float64) Market {
	return Market{Active: true, Outcomes: []Outcome{{Name: "Yes", TokenID: "yes", Price: yes}, {Name: "No", TokenID: "no", Price: no}}}
}
