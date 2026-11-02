package engine_test

import (
	"context"
	"strings"
	"testing"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
	"pennyharvester/backend/internal/predictfun"
	"pennyharvester/backend/internal/venue"
)

// The engine finds a venue's runtime capabilities by type assertion on the
// executor it registered. A fake executor proves the engine logic, but it cannot
// prove the real adapter exposes the method: a missing forwarder compiles, all
// fake-based tests pass, and the feature is silently dead in production. These
// tests wire the real Predict.fun executor on purpose.
const (
	pfKeyA = "0x" + "5a" + "11111111111111111111111111111111111111111111111111111111111111"
	pfKeyB = "0x" + "5a" + "22222222222222222222222222222222222222222222222222222222222222"
)

// newRealPredictFun builds a Predict.fun provider/executor pair without touching
// the network: construction validates the key and picks the chain, nothing else.
func newRealPredictFun(t *testing.T, key, apiKey string) (*predictfun.Provider, *predictfun.Executor) {
	t.Helper()
	provider, executor, err := predictfun.New(predictfun.Config{
		APIKey:     apiKey,
		PrivateKey: key,
		Testnet:    true,
		// A loopback endpoint keeps the test hermetic; nothing here dials it.
		RPCURL: "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("predictfun.New: %v", err)
	}
	return provider, executor
}

// TestRealPredictFunExecutorReceivesSavedCredentials is the regression guard for
// a save that silently did nothing: it asserts the *store* changed what the real
// executor signs with, not merely that the engine called something.
func TestRealPredictFunExecutorReceivesSavedCredentials(t *testing.T) {
	provider, executor := newRealPredictFun(t, pfKeyA, "key-a")
	registry := venue.NewRegistry()
	addVenue(t, registry, market.VenuePolymarket, &fakeProvider{markets: map[string]market.Market{}}, nil)
	addVenue(t, registry, market.VenuePredictFun, provider, executor)
	instance, _ := newTestEngineWithRegistry(t, registry)

	before := executor.Signer()
	if before == "" {
		t.Fatal("the real executor must report a signer")
	}

	ctx := context.Background()
	system := instance.SystemSettings()
	system.Venues[market.VenuePredictFun] = engine.VenueSystemSettings{
		Live: engine.VenueLiveSettings{
			LiveSettings: engine.LiveSettings{APIKey: "key-b", PrivateKey: pfKeyB},
			Testnet:      boolPtr(true),
		},
	}
	if err := instance.UpdateSystemSettings(ctx, system); err != nil {
		t.Fatalf("save system settings: %v", err)
	}
	after := executor.Signer()
	if after == before {
		t.Fatalf("saving credentials did not reach the real executor: signer still %s", after)
	}
	if executor.Maker() != after {
		t.Fatalf("maker %s must follow the new signer %s", executor.Maker(), after)
	}
	if got := executor.BaseURL(); !strings.Contains(got, "testnet") {
		t.Fatalf("network must stay on testnet, endpoint = %s", got)
	}
	if !executor.Configured() {
		t.Fatal("the executor reports not configured after a successful save")
	}
}

// TestRealPredictFunExecutorKeepsNetworkWhenTestnetOmitted pins the tri-state
// rule on the real client: saving a block without a network must not move a
// testnet deployment (test USDT, test contracts) to mainnet.
func TestRealPredictFunExecutorKeepsNetworkWhenTestnetOmitted(t *testing.T) {
	provider, executor := newRealPredictFun(t, pfKeyA, "key-a")
	registry := venue.NewRegistry()
	addVenue(t, registry, market.VenuePolymarket, &fakeProvider{markets: map[string]market.Market{}}, nil)
	addVenue(t, registry, market.VenuePredictFun, provider, executor)
	instance, _ := newTestEngineWithRegistry(t, registry)

	ctx := context.Background()
	system := instance.SystemSettings()
	system.Venues[market.VenuePredictFun] = engine.VenueSystemSettings{
		// No Testnet field: inherit what the process started with.
		Live: engine.VenueLiveSettings{LiveSettings: engine.LiveSettings{APIKey: "key-b", PrivateKey: pfKeyB}},
	}
	if err := instance.UpdateSystemSettings(ctx, system); err != nil {
		t.Fatalf("save system settings: %v", err)
	}
	if got := executor.BaseURL(); !strings.Contains(got, "testnet") {
		t.Fatalf("an omitted testnet flag switched networks: %s", got)
	}
	// The same rule for an explicit reset: back to the startup baseline.
	if err := executor.ResetLiveSettings(); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if got := executor.BaseURL(); !strings.Contains(got, "testnet") {
		t.Fatalf("reset left the client on %s", got)
	}
	if executor.Signer() == "" {
		t.Fatal("reset dropped the process credentials")
	}
}
