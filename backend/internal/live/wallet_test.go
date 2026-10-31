package live

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"pennyharvester/backend/internal/engine"
)

// testSignerKey is a public Hardhat/Anvil fixture key. It is not a credential.
const testSignerKey = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"

func testSignerAddress(t *testing.T) common.Address {
	t.Helper()
	key, err := crypto.HexToECDSA(strings.TrimPrefix(testSignerKey, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	return crypto.PubkeyToAddress(key.PublicKey)
}

// The expected values below are cross-checked against the official client
// implementations (Polymarket py-builder-relayer-client test vector for the
// deposit wallet, GoPolymarket/polymarket-go-sdk for Safe/Proxy, and a real
// on-chain deposit wallet for the UUPS scheme). They pin the CREATE2 derivation
// so a silent constants change cannot ship.
func TestWalletAddressDerivationVectors(t *testing.T) {
	owner := common.HexToAddress("0xA60601A4d903af91855C52BFB3814f6bA342f201")
	if got := DeriveSafeAddress(owner); got != common.HexToAddress("0x5Dd207875896b8684c2E20a6eCBecE3388A513cF") {
		t.Fatalf("safe derivation = %s", got)
	}
	if got := DeriveProxyAddress(owner); got != common.HexToAddress("0x8b78801FEe9956A58d04099724ba62CfA6bF9912") {
		t.Fatalf("proxy derivation = %s", got)
	}
	if got := DeriveDepositAddress(owner); got != common.HexToAddress("0x8b60BF0f650Bf7a0d93F10D72375b37De18F8c40") {
		t.Fatalf("deposit derivation = %s", got)
	}

	// A real account: 0x8518D7D0… owns the deposit wallet 0x96CfCB0C… on Polygon.
	liveOwner := common.HexToAddress("0x8518D7D09e4874BC7D14265146f22fad07058542")
	if got := DeriveDepositAddress(liveOwner); got != common.HexToAddress("0x96CfCB0C30942cFCd1CDf76C7d408794D66B1aCb") {
		t.Fatalf("on-chain deposit wallet derivation = %s", got)
	}
}

func TestDetectWalletTypeFromBytecode(t *testing.T) {
	ctx := context.Background()
	signer := common.HexToAddress("0x0000000000000000000000000000000000000001")
	funder := common.HexToAddress("0x0000000000000000000000000000000000000002")
	cases := []struct {
		name string
		code string
		want int
	}{
		{"no code", "0x", -1},
		// Legacy Magic/Google proxy: EIP-1167 clone.
		{"legacy proxy", "0x363d3d373d3d3d363d7344e999d5c2f66ef0861317f9a4805ac2e90aeb4f5af43d82803e903d91602b57fd5bf3", WalletTypeProxy},
		// Deposit wallet deployed through the UUPS factory (ERC-1967 slot inline).
		{"uuus deposit", "0x363d3d373d3d363d7f360894a13ba1a3210667c828492db98dca3e2076cc3735a920a3ca505d382bbc545af43d6000803e6038573d6000fd5b3d6000f3", WalletTypeDeposit},
		// Deposit wallet deployed through the beacon factory.
		{"beacon deposit", "0x363d3d373d3d363d602036600436635c60da1b60e01b36527fa3f0ad74e5423aebfd80d3ef4346578335a9a72aeaee59ff6cb3582b35133d50545afa5036515af43d6000803e604d573d6000fd5b3d6000f3", WalletTypeDeposit},
		// EIP-7702 delegated EOA behaves like a deposit wallet.
		{"eip7702", "0xef0100363d3d373d3d363d7f360894a1", WalletTypeDeposit},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := detectWalletType(ctx, nil, signer, funder, common.FromHex(testCase.code)); got != testCase.want {
				t.Fatalf("detectWalletType = %d, want %d", got, testCase.want)
			}
		})
	}
}

// rpcServer is a tiny JSON-RPC double. codeFor maps an address to the bytecode
// eth_getCode should return; anything else answers "0x".
func rpcServer(t *testing.T, codeFor map[string]string, calls map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case "eth_getCode":
			var address string
			_ = json.Unmarshal(request.Params[0], &address)
			code, ok := codeFor[strings.ToLower(address)]
			if !ok {
				code = "0x"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": code})
		case "eth_call":
			var call struct {
				To   string `json:"to"`
				Data string `json:"data"`
			}
			_ = json.Unmarshal(request.Params[0], &call)
			result, ok := calls[strings.ToLower(call.To)+"|"+call.Data]
			if !ok {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"jsonrpc": "2.0", "id": 1,
					"error": map[string]any{"code": 3, "message": "execution reverted"},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": 1,
				"error": map[string]any{"code": -32601, "message": "method not found"},
			})
		}
	}))
}

func profileServer(t *testing.T, wallet string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != profilePath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if wallet == "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"type":"not found error","error":"profile not found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"proxyWallet":"` + wallet + `"}`))
	}))
}

func retiredCLOB(t *testing.T) *httptest.Server {
	t.Helper()
	// The public CLOB answers 404 on every /auth/get-proxy-wallets call.
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
}

// TestWalletDiscoveryDoesNotCallRetiredCLOBEndpoint guards the removal of GET
// /auth/get-proxy-wallets: the public CLOB answers 404 for it, so a discovery
// run must not spend a request on it (or report it as an operator-visible note).
func TestWalletDiscoveryDoesNotCallRetiredCLOBEndpoint(t *testing.T) {
	called := false
	clob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		http.NotFound(w, r)
	}))
	defer clob.Close()
	profile := profileServer(t, "")
	defer profile.Close()
	rpc := rpcServer(t, nil, nil)
	defer rpc.Close()

	executor := New(Config{PrivateKey: testSignerKey, PolygonRPC: rpc.URL})
	_, _ = executor.DiscoverWallet(context.Background(), engine.LiveSettings{}, engine.WalletEndpoints{
		CLOB: clob.URL, Gamma: profile.URL,
	})
	if called {
		t.Fatal("wallet discovery must not call the retired /auth/get-proxy-wallets endpoint")
	}
}

func TestDiscoverWalletUsesPublicProfile(t *testing.T) {
	deposit := common.HexToAddress("0x96CfCB0C30942cFCd1CDf76C7d408794D66B1aCb")
	profile := profileServer(t, deposit.Hex())
	defer profile.Close()
	rpc := rpcServer(t, map[string]string{
		strings.ToLower(deposit.Hex()): "0x363d3d373d3d363d602036600436635c60da1b60e01b36527fa3f0ad74e5423aebfd80d3ef4346578335a9a72aeaee59ff6cb3582b35133d50545afa5036515af43d6000803e604d573d6000fd5b3d6000f3",
	}, nil)
	defer rpc.Close()

	executor := New(Config{PrivateKey: testSignerKey, PolygonRPC: rpc.URL})
	discovery, err := executor.DiscoverWallet(context.Background(), engine.LiveSettings{}, engine.WalletEndpoints{
		Gamma: profile.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(discovery.Funder, deposit.Hex()) {
		t.Fatalf("funder = %q", discovery.Funder)
	}
	if discovery.WalletType != WalletTypeDeposit || !discovery.Deployed {
		t.Fatalf("wallet type = %d deployed = %v", discovery.WalletType, discovery.Deployed)
	}
	if discovery.Source != string(sourceProfile) {
		t.Fatalf("source = %q", discovery.Source)
	}
}

func TestDiscoverWalletFallsBackToDerivation(t *testing.T) {
	signer := testSignerAddress(t)
	safe := DeriveSafeAddress(signer)
	profile := profileServer(t, "")
	defer profile.Close()
	rpc := rpcServer(t, map[string]string{
		strings.ToLower(safe.Hex()): "0x6080604052",
	}, map[string]string{
		strings.ToLower(safe.Hex()) + "|0xa0e67e2b": "0x0000000000000000000000000000000000000000000000000000000000000020" +
			"0000000000000000000000000000000000000000000000000000000000000001" +
			"000000000000000000000000" + strings.TrimPrefix(strings.ToLower(signer.Hex()), "0x"),
	})
	defer rpc.Close()

	executor := New(Config{PrivateKey: testSignerKey, PolygonRPC: rpc.URL})
	discovery, err := executor.DiscoverWallet(context.Background(), engine.LiveSettings{}, engine.WalletEndpoints{
		Gamma: profile.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(discovery.Funder, safe.Hex()) {
		t.Fatalf("funder = %q, want %q", discovery.Funder, safe.Hex())
	}
	if discovery.WalletType != WalletTypeSafe {
		t.Fatalf("wallet type = %d, want %d", discovery.WalletType, WalletTypeSafe)
	}
	if discovery.Source != string(sourceDerivation) {
		t.Fatalf("source = %q", discovery.Source)
	}
}

func TestDiscoverWalletReportsMissingAccount(t *testing.T) {
	profile := profileServer(t, "")
	defer profile.Close()
	rpc := rpcServer(t, nil, nil)
	defer rpc.Close()

	executor := New(Config{PrivateKey: testSignerKey, PolygonRPC: rpc.URL})
	discovery, err := executor.DiscoverWallet(context.Background(), engine.LiveSettings{}, engine.WalletEndpoints{
		Gamma: profile.URL,
	})
	if err == nil {
		t.Fatalf("an unknown account must be reported: %+v", discovery)
	}
	if !containsNote(discovery.Notes, "polymarket.com") {
		t.Fatalf("expected operator guidance, got %v", discovery.Notes)
	}
	if discovery.Signer == "" {
		t.Fatal("the signer must be reported even when discovery fails")
	}
}

func TestDiscoverWalletRequiresPrivateKey(t *testing.T) {
	if _, err := New(Config{}).DiscoverWallet(context.Background(), engine.LiveSettings{}, engine.WalletEndpoints{}); err == nil {
		t.Fatal("discovery without a private key must fail")
	}
}

func containsNote(notes []string, fragment string) bool {
	for _, note := range notes {
		if strings.Contains(note, fragment) {
			return true
		}
	}
	return false
}
