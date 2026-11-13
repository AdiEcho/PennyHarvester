package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/market"
)

type apiWalletLive struct {
	quoteLive
	discovery    engine.WalletDiscovery
	discoveryErr error
	calls        int
	environment  engine.LiveEnvironment
	clobURL      string
}

func (l *apiWalletLive) Signer() string {
	if l.discovery.Signer != "" {
		return l.discovery.Signer
	}
	return "0x0000000000000000000000000000000000000001"
}

func (l *apiWalletLive) UpdateSettings(_ engine.LiveSettings, clobURL string) error {
	l.clobURL = clobURL
	return nil
}

func (l *apiWalletLive) EffectiveCLOBURL() string {
	if l.clobURL != "" {
		return l.clobURL
	}
	return "https://clob.example.com"
}

func (l *apiWalletLive) Environment() engine.LiveEnvironment { return l.environment }

func (l *apiWalletLive) DiscoverWallet(_ context.Context, _ engine.LiveSettings, _ engine.WalletEndpoints) (engine.WalletDiscovery, error) {
	l.calls++
	return l.discovery, l.discoveryErr
}

func TestSystemSaveRequiresVerifiedProxyFunder(t *testing.T) {
	key := "0x" + strings.Repeat("1", 64)
	funder := "0x00000000000000000000000000000000000000b1"
	baseDiscovery := engine.WalletDiscovery{
		Signer: "0x0000000000000000000000000000000000000001", Funder: funder,
		WalletType: 1, Deployed: true, Verified: true, ChainID: 137,
		Owner: "0x0000000000000000000000000000000000000001", Evidence: "polygon:137:owner",
	}
	tests := []struct {
		name       string
		discovery  engine.WalletDiscovery
		wantStatus int
		wantStored bool
	}{
		{name: "unverified is rejected", discovery: func() engine.WalletDiscovery {
			discovery := baseDiscovery
			discovery.Verified = false
			return discovery
		}(), wantStatus: http.StatusBadRequest},
		{name: "verified is accepted", discovery: baseDiscovery, wantStatus: http.StatusOK, wantStored: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, repository := newSystemRouterWithExecutor(t, &apiWalletLive{
				discovery:   test.discovery,
				environment: engine.LiveEnvironment{PrivateKeySet: true, SignatureType: 1},
			})
			response := request(t, handler, http.MethodPut, "/api/v1/system", map[string]any{
				"live": map[string]any{
					"private_key":    key,
					"funder":         funder,
					"signature_type": 1,
				},
			})
			if response.Code != test.wantStatus {
				t.Fatalf("PUT status = %d, want %d: %s", response.Code, test.wantStatus, response.Body.String())
			}
			if test.wantStatus != http.StatusOK && !strings.Contains(response.Body.String(), "无法证明") {
				t.Fatalf("rejection should explain the ownership failure: %s", response.Body.String())
			}
			stored, err := repository.LoadSystemSettings(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			pm := stored.Venues[market.VenuePolymarket].Live
			if test.wantStored {
				if !strings.EqualFold(stored.Live.Funder, funder) || !strings.EqualFold(pm.Funder, funder) {
					t.Fatalf("verified funder was not persisted: flat=%q venue=%q", stored.Live.Funder, pm.Funder)
				}
				return
			}
			if stored.Live.Funder != "" || pm.Funder != "" {
				t.Fatalf("unverified funder was persisted: flat=%q venue=%q", stored.Live.Funder, pm.Funder)
			}
		})
	}
}
