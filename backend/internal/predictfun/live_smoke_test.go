//go:build live

package predictfun

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveProviderSmoke exercises the real Predict.fun REST API. It requires
// both the live build tag and an explicit opt-in because it needs the network
// and depends on the venue's live data:
//
//	PREDICTFUN_LIVE=1 go test -tags live ./internal/predictfun/ -run TestLiveProviderSmoke -v
//
// It needs no credentials: the market-data endpoints are public, so this proves
// the read path without touching the operator's key. Offline builds (no tag)
// never compile or run this file, regardless of environment variables.
func TestLiveProviderSmoke(t *testing.T) {
	if os.Getenv("PREDICTFUN_LIVE") != "1" {
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

// TestLiveProviderSettlementSmoke proves the settlement mapping against settled
// markets the venue actually published, not against a fixture:
//
//	PREDICTFUN_LIVE=1 PREDICTFUN_TESTNET=1 go test -tags live ./internal/predictfun/ -run TestLiveProviderSettlementSmoke -v
func TestLiveProviderSettlementSmoke(t *testing.T) {
	if os.Getenv("PREDICTFUN_LIVE") != "1" {
		t.Skip("set PREDICTFUN_LIVE=1 to hit the real Predict.fun API")
	}
	testnet := os.Getenv("PREDICTFUN_TESTNET") != ""
	client, err := NewClient(Config{Testnet: testnet})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	provider := NewProvider(client)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	settled, _, err := client.Markets(ctx, MarketQuery{Status: "RESOLVED", First: 20})
	if err != nil {
		t.Fatalf("list resolved markets: %v", err)
	}
	if len(settled) == 0 {
		t.Skip("the venue returned no settled markets right now")
	}
	checked := 0
	for _, raw := range settled {
		if raw.Resolution == nil || len(raw.Outcomes) != 2 {
			continue
		}
		item, err := provider.Get(ctx, raw.ID.String())
		if err != nil {
			t.Fatalf("Get(%s): %v", raw.ID.String(), err)
		}
		if !item.Closed || item.ResolutionStatus != "resolved" {
			t.Fatalf("market %s is settled upstream but mapped to %q (closed=%v)", item.ID, item.ResolutionStatus, item.Closed)
		}
		won, lost := 0, 0
		for _, outcome := range item.Outcomes {
			price, resolved := item.SettlementPrice(outcome.TokenID)
			if !resolved {
				t.Fatalf("market %s outcome %s has no payout", item.ID, outcome.Name)
			}
			switch price {
			case 1:
				won++
			case 0:
				lost++
			default:
				t.Fatalf("market %s outcome %s priced %v, want 1 or 0", item.ID, outcome.Name, price)
			}
		}
		if won != 1 || lost != 1 {
			t.Fatalf("market %s has %d winners and %d losers, want 1/1", item.ID, won, lost)
		}
		checked++
		if checked == 3 {
			break
		}
	}
	if checked == 0 {
		t.Skip("no settled market with a verdict in this page")
	}
	t.Logf("verified %d settled markets map to a decisive 1/0 payout", checked)
}
