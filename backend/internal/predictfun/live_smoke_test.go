package predictfun

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveProviderSmoke exercises the real Predict.fun REST API. It is opt-in
// because it needs the network and depends on the venue's live data:
//
//	PREDICTFUN_LIVE=1 go test ./internal/predictfun/ -run TestLiveProviderSmoke -v
//
// It needs no credentials: the market-data endpoints are public, so this proves
// the read path without touching the operator's key.
func TestLiveProviderSmoke(t *testing.T) {
	if os.Getenv("PREDICTFUN_LIVE") == "" {
		t.Skip("set PREDICTFUN_LIVE=1 to hit the real Predict.fun API")
	}
	testnet := os.Getenv("PREDICTFUN_TESTNET") != ""
	client, err := NewClient(Config{Testnet: testnet})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	provider := NewProvider(client)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	started := time.Now()
	markets, err := provider.ListOpen(ctx, 400)
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	t.Logf("ListOpen returned %d markets in %s", len(markets), time.Since(started).Round(time.Millisecond))
	if len(markets) == 0 {
		t.Fatal("no markets: the endpoint answered but every market was skipped, check the skip rules")
	}
	for i, m := range markets {
		if i == 3 {
			break
		}
		t.Logf("market %s venue=%s currency=%s outcomes=%d end=%s tick=%v fee_bps=%.0f liquidity=%.2f url=%q",
			m.ID, m.EffectiveVenue(), m.EffectiveCurrency(), len(m.Outcomes), m.EndDate.Format(time.RFC3339),
			m.TickSize, m.FeeRateBPS, m.Liquidity, m.URL())
		if m.EndDate.IsZero() || m.TickSize <= 0 || len(m.Outcomes) != 2 {
			t.Fatalf("market %s is not tradable yet passed the filters", m.ID)
		}
	}
	first, err := provider.Get(ctx, markets[0].ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", markets[0].ID, err)
	}
	if first.ConditionID == "" || first.Question == "" {
		t.Fatalf("Get returned an empty market: %+v", first)
	}
	t.Logf("Get(%s) ok: %q condition=%s", first.ID, first.Question, first.ConditionID)
}
