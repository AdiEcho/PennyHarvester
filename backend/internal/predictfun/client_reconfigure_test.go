package predictfun

import (
	"encoding/hex"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

// newTestKey returns a unique throwaway key with its hex encoding. Keys are
// generated per test so no real credential can leak into a fixture.
func newTestKey(t *testing.T) (string, string) {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return hex.EncodeToString(crypto.FromECDSA(key)), crypto.PubkeyToAddress(key.PublicKey).Hex()
}

// TestApplyLiveSettingsReplacesCredentials covers the settings-page path: a
// credential saved while the process runs must become effective immediately,
// and a later save must not resurrect a cleared value.
func TestApplyLiveSettingsReplacesCredentials(t *testing.T) {
	client, err := NewClient(Config{BaseURL: "https://example.test", Testnet: true})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if client.Configured() {
		t.Fatal("a client without credentials must not report configured")
	}

	keyHex, signer := newTestKey(t)
	if err := client.ApplyLiveSettings("api-key-1", keyHex, "", boolPtr(true), ""); err != nil {
		t.Fatalf("apply credentials: %v", err)
	}
	if !client.Configured() {
		t.Fatal("configured must be true after a key and api key were applied")
	}
	if got := client.Signer(); got != signer {
		t.Fatalf("signer = %q, want %q", got, signer)
	}
	if got := client.Maker(); got != signer {
		t.Fatalf("maker = %q, want the signer %q", got, signer)
	}
	if got := client.MaskedAPIKey(); got != "api-key-…" {
		t.Fatalf("masked api key = %q", got)
	}

	// An empty api key keeps the previous one; the endpoint may still change.
	if err := client.ApplyLiveSettings("", "", "", boolPtr(true), "https://second.example.test"); err != nil {
		t.Fatalf("apply endpoint: %v", err)
	}
	if got := client.MaskedAPIKey(); got != "api-key-…" {
		t.Fatalf("empty api key must keep the old one, got %q", got)
	}
	if got := client.BaseURL(); got != "https://second.example.test" {
		t.Fatalf("base url = %q", got)
	}
	if got := client.Signer(); got != signer {
		t.Fatalf("signer changed on an endpoint-only update: %q", got)
	}
}

// TestApplyLiveSettingsRejectsInvalidKey pins the fail-closed behaviour: a
// malformed key must be rejected before it replaces a working one.
func TestApplyLiveSettingsRejectsInvalidKey(t *testing.T) {
	keyHex, signer := newTestKey(t)
	client, err := NewClient(Config{BaseURL: "https://example.test", PrivateKey: keyHex, APIKey: "key"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := client.ApplyLiveSettings("", "not-a-key", "", boolPtr(false), ""); err == nil {
		t.Fatal("an invalid private key must be rejected")
	}
	if got := client.Signer(); got != signer {
		t.Fatalf("a rejected key must not replace the working signer: %q", got)
	}
	if got := client.Signer(); got == "" {
		t.Fatal("signer must still be set")
	}
}

// TestSetBaseURLEmptyRestoresDefault keeps the data-source override reversible,
// which is what the engine's rollback relies on.
func TestSetBaseURLEmptyRestoresDefault(t *testing.T) {
	client, err := NewClient(Config{BaseURL: "https://example.test"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := client.SetBaseURL(""); err != nil {
		t.Fatalf("reset base url: %v", err)
	}
	if got, want := client.BaseURL(), DefaultBaseURL; got != want {
		t.Fatalf("base url = %q, want %q", got, want)
	}
	if got := client.DefaultBaseURL(); got != DefaultBaseURL {
		t.Fatalf("default base url = %q", got)
	}
}

// TestSetBaseURLRejectsRelative prevents a typo from turning every request into
// a relative URL that silently hits the wrong host or fails with an opaque error.
func TestSetBaseURLRejectsRelative(t *testing.T) {
	client, err := NewClient(Config{Testnet: true})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	for _, raw := range []string{"example.test", "/v1", "ftp://example.test", "https://"} {
		if err := client.SetBaseURL(raw); err == nil {
			t.Fatalf("SetBaseURL(%q) must fail", raw)
		}
	}
	if got := client.BaseURL(); got != TestnetBaseURL {
		t.Fatalf("a rejected value must not change the endpoint, got %q", got)
	}
}

// TestApplyLiveSettingsSwitchingNetworkResetsEndpoint pins the rule that a
// testnet host is never reused for mainnet.
func TestApplyLiveSettingsSwitchingNetworkResetsEndpoint(t *testing.T) {
	client, err := NewClient(Config{BaseURL: "https://example.test", Testnet: true})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	keyHex, _ := newTestKey(t)
	if err := client.ApplyLiveSettings("key", keyHex, "", boolPtr(false), ""); err != nil {
		t.Fatalf("switch network: %v", err)
	}
	if got := client.BaseURL(); got != DefaultBaseURL {
		t.Fatalf("base url after switching to mainnet = %q, want %q", got, DefaultBaseURL)
	}
	if got := client.ChainID(); got != ChainIDBnbMainnet {
		t.Fatalf("chain id = %d", got)
	}
	if _, ok := AddressesFor(client.ChainID()); !ok {
		t.Fatal("mainnet chain must have an address set")
	}
}

// TestReconfigureFiresHooks proves derived caches are dropped after a change.
func TestReconfigureFiresHooks(t *testing.T) {
	client, err := NewClient(Config{BaseURL: "https://example.test"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	var calls atomic.Int32
	client.OnReconfigure(func() { calls.Add(1) })
	if err := client.SetBaseURL("https://second.example.test"); err != nil {
		t.Fatalf("set base url: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("hooks called %d times, want 1", calls.Load())
	}
	// Setting the same value is a no-op, so a settings save that changed
	// nothing does not drop caches.
	if err := client.SetBaseURL("https://second.example.test"); err != nil {
		t.Fatalf("set base url again: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("an unchanged endpoint must not fire hooks, got %d", calls.Load())
	}
}

// TestProviderResetDropsCaches covers the provider side of a reconfigure.
func TestProviderResetDropsCaches(t *testing.T) {
	client, err := NewClient(Config{BaseURL: "https://example.test"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	provider := NewProvider(client)
	provider.endsAt["crypto"] = endsAtEntry{ok: true}
	provider.byToken["111"] = marketMeta{MarketID: "42"}
	provider.byCondition["0xcond"] = marketMeta{MarketID: "42"}

	provider.Reset()

	if len(provider.endsAt) != 0 || len(provider.byToken) != 0 || len(provider.byCondition) != 0 {
		t.Fatal("Reset must empty every cache")
	}
	if err := client.SetBaseURL("https://third.example.test"); err != nil {
		t.Fatalf("set base url: %v", err)
	}
	if len(provider.byToken) != 0 {
		t.Fatal("a client reconfigure must reset the provider caches")
	}
}

// TestConcurrentReconfigure is the -race test: the scanner reads the endpoint and
// the signer while the settings page swaps the configuration.
func TestConcurrentReconfigure(t *testing.T) {
	client, err := NewClient(Config{BaseURL: "https://example.test", Testnet: true})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	firstKey, _ := newTestKey(t)
	if err := client.ApplyLiveSettings("key", firstKey, "", boolPtr(true), ""); err != nil {
		t.Fatalf("initial apply: %v", err)
	}
	secondKey, secondSigner := newTestKey(t)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = client.BaseURL()
				_ = client.Signer()
				_ = client.Configured()
				_ = client.Testnet()
				_ = client.Addresses()
			}
		}()
	}
	for i := 0; i < 20; i++ {
		if err := client.ApplyLiveSettings("", secondKey, "", boolPtr(true), ""); err != nil {
			t.Errorf("apply: %v", err)
			break
		}
	}
	close(stop)
	wg.Wait()
	if got := client.Signer(); got != secondSigner {
		t.Fatalf("signer = %q, want %q", got, secondSigner)
	}
}

// boolPtr keeps the test calls readable: the credential API takes a tri-state
// network, where nil means "leave the network alone".
func boolPtr(v bool) *bool { return &v }

// A network switch must not keep the endpoint resolved for the old network,
// even though the caller passes it back (the settings handler forwards the URL
// it just resolved against the network in use).
func TestApplyLiveSettingsDropsEndpointPinnedToTheOldNetwork(t *testing.T) {
	client, err := NewClient(Config{})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := client.SetBaseURL("https://predictfun.test"); err != nil {
		t.Fatalf("pin host: %v", err)
	}
	oldHost := client.BaseURL()
	if oldHost != "https://predictfun.test" {
		t.Fatalf("pinned host not applied: %q", oldHost)
	}
	// Switching to testnet while passing the old host back must land on the
	// testnet default rather than "mainnet chain id, testnet REST".
	if err := client.ApplyLiveSettings("", "", "", boolPtr(true), oldHost); err != nil {
		t.Fatal(err)
	}
	if got := client.BaseURL(); got != TestnetBaseURL {
		t.Fatalf("endpoint after network switch = %q, want %q", got, TestnetBaseURL)
	}
	// An explicitly different host is a deliberate pin and must be honoured.
	if err := client.ApplyLiveSettings("", "", "", boolPtr(true), "https://predictfun.testnet.test"); err != nil {
		t.Fatal(err)
	}
	if got := client.BaseURL(); got != "https://predictfun.testnet.test" {
		t.Fatalf("explicit host was dropped: %q", got)
	}
	// Omitting the network must never move it.
	if err := client.ApplyLiveSettings("another-key", "", "", nil, ""); err != nil {
		t.Fatal(err)
	}
	if got := client.BaseURL(); got != "https://predictfun.testnet.test" {
		t.Fatalf("endpoint changed without a network switch: %q", got)
	}
	if got := client.Testnet(); !got {
		t.Fatal("a nil network must keep the client on testnet")
	}
}

func TestSideIsSellFallsBackWhenTheAckOmitsTheSide(t *testing.T) {
	cases := []struct {
		name     string
		ack      OrderAck
		fallback bool
		want     bool
	}{
		{name: "explicit sell", ack: OrderAck{Side: Number("1")}, fallback: false, want: true},
		{name: "explicit buy", ack: OrderAck{Side: Number("0")}, fallback: true, want: false},
		{name: "missing side, sell request", ack: OrderAck{}, fallback: true, want: true},
		{name: "missing side, buy request", ack: OrderAck{}, fallback: false, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ack.sideIsSell(tc.fallback); got != tc.want {
				t.Fatalf("sideIsSell = %v, want %v", got, tc.want)
			}
		})
	}
}
