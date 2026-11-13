package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
	"pennyharvester/backend/internal/netproxy"
)

type walletVerifyLive struct {
	*runtimeLive
	discovery    engine.WalletDiscovery
	discoveryErr error
	discover     func(engine.LiveSettings) (engine.WalletDiscovery, error)
	calls        int
	endpoints    engine.WalletEndpoints
	env          engine.LiveEnvironment
}

func (l *walletVerifyLive) Environment() engine.LiveEnvironment { return l.env }

func (l *walletVerifyLive) DiscoverWallet(_ context.Context, settings engine.LiveSettings, endpoints engine.WalletEndpoints) (engine.WalletDiscovery, error) {
	l.calls++
	l.endpoints = endpoints
	if l.discover != nil {
		return l.discover(settings)
	}
	return l.discovery, l.discoveryErr
}

func walletTestsEngine(t *testing.T, discovery engine.WalletDiscovery, signatureType int) (*engine.Engine, *walletVerifyLive) {
	t.Helper()
	live := &walletVerifyLive{
		runtimeLive: &runtimeLive{
			fakeLive:    &fakeLive{},
			clobURL:     "https://clob.example.com",
			defaultCLOB: "https://clob.example.com",
		},
		discovery: discovery,
		env:       engine.LiveEnvironment{CLOBBaseURL: "https://clob.example.com", PrivateKeySet: true, SignatureType: signatureType},
	}
	instance, _ := newTestEngine(t, &fakeProvider{markets: map[string]market.Market{}}, live)
	return instance, live
}

func walletSettings(funder string, signatureType *int) engine.SystemSettings {
	return engine.SystemSettings{
		Proxy: netproxy.DefaultConfig(),
		DataSource: engine.DataSourceSettings{
			GammaBaseURL: "https://gamma.candidate.example",
			CLOBBaseURL:  "https://clob.candidate.example",
		},
		Live: engine.LiveSettings{
			PrivateKey:    "0x" + strings.Repeat("1", 64),
			Funder:        funder,
			SignatureType: signatureType,
		},
	}
}

func verifiedWalletDiscovery(funder string, walletType int) engine.WalletDiscovery {
	return engine.WalletDiscovery{
		Signer:     "0x0000000000000000000000000000000000000001",
		Funder:     funder,
		WalletType: walletType,
		Deployed:   true,
		Verified:   true,
		ChainID:    137,
		Owner:      "0x0000000000000000000000000000000000000001",
		Evidence:   "polygon:137:owner",
	}
}

func TestProxyFunderRejectsUnverifiedDiscovery(t *testing.T) {
	ctx := context.Background()
	funder := "0x00000000000000000000000000000000000000a1"
	discovery := verifiedWalletDiscovery(funder, 1)
	discovery.Verified = false
	instance, live := walletTestsEngine(t, discovery, 1)
	baseline := walletSettings("", intPointer(0))
	baseline.DataSource.GammaBaseURL = "https://gamma.baseline.example"
	baseline.DataSource.CLOBBaseURL = "https://clob.baseline.example"
	if _, err := instance.PatchSystemSettings(ctx, func(candidate *engine.SystemSettings) error {
		*candidate = baseline
		return nil
	}); err != nil {
		t.Fatalf("baseline save failed: %v", err)
	}

	_, err := instance.PatchSystemSettings(ctx, func(candidate *engine.SystemSettings) error {
		*candidate = walletSettings(funder, intPointer(1))
		return nil
	})
	if !errors.Is(err, engine.ErrInvalidSettings) {
		t.Fatalf("unverified discovery error = %v, want ErrInvalidSettings", err)
	}
	if !strings.Contains(err.Error(), "无法证明") {
		t.Fatalf("error should explain the ownership failure: %v", err)
	}
	if live.calls != 1 {
		t.Fatalf("discovery calls = %d, want 1", live.calls)
	}
	stored := instance.SystemSettings()
	if got := stored.Venues[market.VenuePolymarket].Live.Funder; got != "" {
		t.Fatalf("rejected funder was applied: %q", got)
	}
	if stored.DataSource.GammaBaseURL != baseline.DataSource.GammaBaseURL || stored.DataSource.CLOBBaseURL != baseline.DataSource.CLOBBaseURL {
		t.Fatalf("rejected save changed other settings: %+v", stored.DataSource)
	}
}

func TestProxyFunderRejectsWrongChain(t *testing.T) {
	ctx := context.Background()
	funder := "0x00000000000000000000000000000000000000a2"
	discovery := verifiedWalletDiscovery(funder, 1)
	discovery.ChainID = 1
	instance, live := walletTestsEngine(t, discovery, 1)

	_, err := instance.PatchSystemSettings(ctx, func(candidate *engine.SystemSettings) error {
		*candidate = walletSettings(funder, intPointer(1))
		return nil
	})
	if !errors.Is(err, engine.ErrInvalidSettings) {
		t.Fatalf("wrong-chain discovery error = %v, want ErrInvalidSettings", err)
	}
	if live.calls != 1 {
		t.Fatalf("discovery calls = %d, want 1", live.calls)
	}
	if got := instance.SystemSettings().Venues[market.VenuePolymarket].Live.Funder; got != "" {
		t.Fatalf("wrong-chain funder was applied: %q", got)
	}
}

func TestProxyFunderRejectsDiscoveryMismatch(t *testing.T) {
	ctx := context.Background()
	candidateFunder := "0x00000000000000000000000000000000000000a3"
	tests := []struct {
		name      string
		discovery engine.WalletDiscovery
	}{
		{
			name:      "funder mismatch",
			discovery: verifiedWalletDiscovery("0x00000000000000000000000000000000000000ff", 1),
		},
		{
			name:      "wallet type mismatch",
			discovery: verifiedWalletDiscovery(candidateFunder, 2),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			instance, live := walletTestsEngine(t, test.discovery, 1)
			_, err := instance.PatchSystemSettings(ctx, func(candidate *engine.SystemSettings) error {
				*candidate = walletSettings(candidateFunder, intPointer(1))
				return nil
			})
			if !errors.Is(err, engine.ErrInvalidSettings) {
				t.Fatalf("mismatch error = %v, want ErrInvalidSettings", err)
			}
			if live.calls != 1 {
				t.Fatalf("discovery calls = %d, want 1", live.calls)
			}
			if got := instance.SystemSettings().Venues[market.VenuePolymarket].Live.Funder; got != "" {
				t.Fatalf("mismatched funder was applied: %q", got)
			}
		})
	}
}

func TestVerifiedProxyFunderIsAcceptedAndCached(t *testing.T) {
	ctx := context.Background()
	funder := "0x00000000000000000000000000000000000000a4"
	instance, live := walletTestsEngine(t, verifiedWalletDiscovery(funder, 1), 1)

	for i := 0; i < 2; i++ {
		if _, err := instance.PatchSystemSettings(ctx, func(candidate *engine.SystemSettings) error {
			*candidate = walletSettings(funder, intPointer(1))
			return nil
		}); err != nil {
			t.Fatalf("save %d failed: %v", i+1, err)
		}
	}
	if live.calls != 1 {
		t.Fatalf("discovery calls = %d, want one cached discovery", live.calls)
	}
	if live.endpoints.Gamma != "https://gamma.candidate.example" || live.endpoints.CLOB != "https://clob.candidate.example" {
		t.Fatalf("discovery endpoints = %+v, want candidate endpoints", live.endpoints)
	}
	if got := instance.SystemSettings().Venues[market.VenuePolymarket].Live.Funder; !strings.EqualFold(got, funder) {
		t.Fatalf("verified funder = %q, want %q", got, funder)
	}
}

func TestChangedProxyFunderOrTypeDiscoversAgain(t *testing.T) {
	ctx := context.Background()
	funderA := "0x00000000000000000000000000000000000000a5"
	funderB := "0x00000000000000000000000000000000000000a6"
	instance, live := walletTestsEngine(t, engine.WalletDiscovery{}, 1)
	live.discover = func(settings engine.LiveSettings) (engine.WalletDiscovery, error) {
		walletType := 0
		if settings.SignatureType != nil {
			walletType = *settings.SignatureType
		}
		return verifiedWalletDiscovery(settings.Funder, walletType), nil
	}

	saves := []engine.SystemSettings{
		walletSettings(funderA, intPointer(1)),
		walletSettings(funderA, intPointer(2)),
		walletSettings(funderB, intPointer(2)),
	}
	for i, settings := range saves {
		if _, err := instance.PatchSystemSettings(ctx, func(candidate *engine.SystemSettings) error {
			*candidate = settings
			return nil
		}); err != nil {
			t.Fatalf("save %d failed: %v", i+1, err)
		}
		if live.calls != i+1 {
			t.Fatalf("after save %d discovery calls = %d, want %d", i+1, live.calls, i+1)
		}
	}
	stored := instance.SystemSettings().Venues[market.VenuePolymarket].Live
	if !strings.EqualFold(stored.Funder, funderB) || stored.SignatureType == nil || *stored.SignatureType != 2 {
		t.Fatalf("stored wallet = %+v, want funder %s type 2", stored, funderB)
	}
}

func TestProxyFunderUsesRuntimeSignatureType(t *testing.T) {
	ctx := context.Background()
	funder := "0x00000000000000000000000000000000000000a7"
	instance, live := walletTestsEngine(t, verifiedWalletDiscovery(funder, 1), 1)

	if _, err := instance.PatchSystemSettings(ctx, func(candidate *engine.SystemSettings) error {
		*candidate = walletSettings(funder, nil)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if live.calls != 1 {
		t.Fatalf("discovery calls = %d, want 1", live.calls)
	}
}

func TestProxyFunderRejectsRuntimeEOAType(t *testing.T) {
	ctx := context.Background()
	funder := "0x00000000000000000000000000000000000000a8"
	instance, live := walletTestsEngine(t, verifiedWalletDiscovery(funder, 0), 0)

	_, err := instance.PatchSystemSettings(ctx, func(candidate *engine.SystemSettings) error {
		*candidate = walletSettings(funder, nil)
		return nil
	})
	if !errors.Is(err, engine.ErrInvalidSettings) {
		t.Fatalf("runtime EOA with funder error = %v, want ErrInvalidSettings", err)
	}
	if live.calls != 0 {
		t.Fatalf("EOA funder should be rejected before discovery, calls = %d", live.calls)
	}
}

func TestEOAWithoutFunderSkipsDiscovery(t *testing.T) {
	ctx := context.Background()
	instance, live := walletTestsEngine(t, verifiedWalletDiscovery("", 0), 0)

	if _, err := instance.PatchSystemSettings(ctx, func(candidate *engine.SystemSettings) error {
		*candidate = walletSettings("", intPointer(0))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if live.calls != 0 {
		t.Fatalf("EOA without funder made %d discovery calls", live.calls)
	}
}

func TestDiscoverCacheAvoidsSecondRPCDuringSave(t *testing.T) {
	ctx := context.Background()
	funder := "0x00000000000000000000000000000000000000a9"
	instance, live := walletTestsEngine(t, verifiedWalletDiscovery(funder, 1), 1)
	endpoints := engine.WalletEndpoints{Gamma: "https://gamma.detect.example", CLOB: "https://clob.detect.example"}
	result, err := instance.DiscoverWallet(ctx, market.VenuePolymarket, engine.LiveSettings{PrivateKey: "0x" + strings.Repeat("1", 64)}, endpoints)
	if err != nil {
		t.Fatal(err)
	}
	if live.calls != 1 || !result.Verified {
		t.Fatalf("detect calls = %d, result = %+v", live.calls, result)
	}

	if _, err := instance.PatchSystemSettings(ctx, func(candidate *engine.SystemSettings) error {
		*candidate = walletSettings(result.Funder, intPointer(result.WalletType))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if live.calls != 1 {
		t.Fatalf("save after detect repeated discovery: calls = %d, want 1", live.calls)
	}
}

func TestDiscoverWalletWithoutExplicitKeyDoesNotReuseCachedSigner(t *testing.T) {
	ctx := context.Background()
	funder := "0x00000000000000000000000000000000000000ab"
	instance, live := walletTestsEngine(t, verifiedWalletDiscovery(funder, 1), 1)
	endpoints := engine.WalletEndpoints{Gamma: "https://gamma.example", CLOB: "https://clob.example"}
	explicit := engine.LiveSettings{PrivateKey: "0x" + strings.Repeat("1", 64)}
	if _, err := instance.DiscoverWallet(ctx, market.VenuePolymarket, explicit, endpoints); err != nil {
		t.Fatal(err)
	}
	if live.calls != 1 {
		t.Fatalf("explicit-key discovery calls = %d, want 1", live.calls)
	}

	// The browser can clear a stored key; the executor then falls back to the
	// process key. A cache entry for the old signer must not answer for it.
	if _, err := instance.DiscoverWallet(ctx, market.VenuePolymarket, engine.LiveSettings{}, endpoints); err != nil {
		t.Fatal(err)
	}
	if live.calls != 2 {
		t.Fatalf("empty-key discovery reused a cached signer: calls = %d, want 2", live.calls)
	}
}

func TestDiscoverWalletDoesNotCacheFailures(t *testing.T) {
	ctx := context.Background()
	funder := "0x00000000000000000000000000000000000000aa"
	instance, live := walletTestsEngine(t, verifiedWalletDiscovery(funder, 1), 1)
	live.discoveryErr = errors.New("temporary RPC failure")
	settings := engine.LiveSettings{PrivateKey: "0x" + strings.Repeat("1", 64)}

	if _, err := instance.DiscoverWallet(ctx, market.VenuePolymarket, settings, engine.WalletEndpoints{}); err == nil {
		t.Fatal("expected the first discovery to fail")
	}
	live.discoveryErr = nil
	if _, err := instance.DiscoverWallet(ctx, market.VenuePolymarket, settings, engine.WalletEndpoints{}); err != nil {
		t.Fatal(err)
	}
	if live.calls != 2 {
		t.Fatalf("failed discovery was cached: calls = %d, want 2", live.calls)
	}
	if _, err := instance.DiscoverWallet(ctx, market.VenuePolymarket, settings, engine.WalletEndpoints{}); err != nil {
		t.Fatal(err)
	}
	if live.calls != 2 {
		t.Fatalf("successful discovery was not cached: calls = %d, want 2", live.calls)
	}
}
