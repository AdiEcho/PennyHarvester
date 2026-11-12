package config

import (
	"os"
	"strconv"
	"strings"
)

// Builder attribution. Every order this deployment signs carries BuilderCode so
// matched trades are credited to the builder profile (and its fee share). The
// code is public: it is serialized into the signed order and is not a secret.
// BuilderAddress only identifies the builder account for reference — never send
// funds to it.
const (
	BuilderAddress = "0x1db6b86b012d59ebda322390d462295a6817a0c4"
	BuilderCode    = "0x07718b719cce8cdfb015b268d90732d15f7071e9fa845bd16ec058b0ab2226eb"
)

type Config struct {
	Addr         string
	DatabasePath string
	GammaBaseURL string
	WebDir       string

	PolymarketPrivateKey    string
	PolymarketFunder        string
	PolymarketSignatureType int
	PolymarketCLOBURL       string
	PolygonRPCURL           string
	PolymarketAPIKey        string
	PolymarketAPISecret     string
	PolymarketAPIPassphrase string

	// Predict.fun (BSC). Enabled is explicit so a half-filled block cannot
	// switch live trading on by accident.
	PredictFunEnabled     bool
	PredictFunAPIKey      string
	PredictFunPrivateKey  string
	PredictFunSmartWallet string
	PredictFunBaseURL     string
	PredictFunTestnet     bool
	PredictFunRPC         string
}

func FromEnv() Config {
	return Config{
		Addr:         env("ADDR", "127.0.0.1:8080"),
		DatabasePath: env("DATABASE_PATH", "./data/pennyharvester.db"),
		GammaBaseURL: env("GAMMA_BASE_URL", "https://gamma-api.polymarket.com"),
		WebDir:       os.Getenv("WEB_DIR"),

		PolymarketPrivateKey:    os.Getenv("POLYMARKET_PRIVATE_KEY"),
		PolymarketFunder:        envAny([]string{"POLYMARKET_FUNDER", "POLYMARKET_MAKER_ADDRESS"}, ""),
		PolymarketSignatureType: envInt("POLYMARKET_SIGNATURE_TYPE", 0),
		PolymarketCLOBURL:       env("POLYMARKET_CLOB_URL", "https://clob.polymarket.com"),
		// Read-only Polygon RPC used by wallet auto-detection. The keyless public
		// endpoint is fine for eth_getCode/eth_call; set your own for reliability.
		PolygonRPCURL:           envAny([]string{"POLYGON_RPC_URL", "POLYMARKET_RPC_URL"}, "https://polygon-bor-rpc.publicnode.com"),
		PolymarketAPIKey:        os.Getenv("POLYMARKET_API_KEY"),
		PolymarketAPISecret:     os.Getenv("POLYMARKET_API_SECRET"),
		PolymarketAPIPassphrase: os.Getenv("POLYMARKET_API_PASSPHRASE"),

		PredictFunEnabled:     envBool("PREDICTFUN_ENABLED"),
		PredictFunAPIKey:      os.Getenv("PREDICTFUN_API_KEY"),
		PredictFunPrivateKey:  os.Getenv("PREDICTFUN_PRIVATE_KEY"),
		PredictFunSmartWallet: envAny([]string{"PREDICTFUN_SMART_WALLET", "PREDICTFUN_FUNDER"}, ""),
		PredictFunTestnet:     envBool("PREDICTFUN_TESTNET"),
		PredictFunBaseURL:     os.Getenv("PREDICTFUN_BASE_URL"),
		// Empty means "pick the RPC that belongs to the configured network".
		// Defaulting to mainnet here would leave a testnet client signing
		// chainId 97 while reading balances from chain 56.
		PredictFunRPC: os.Getenv("PREDICTFUN_RPC_URL"),
	}
}

// PredictFunDefaultBaseURL is the production Predict.fun REST endpoint. The
// testnet endpoint is a different host, not a path, so the two are never
// derived from each other.
const (
	PredictFunDefaultBaseURL = "https://api.predict.fun"
	PredictFunTestnetBaseURL = "https://api-testnet.predict.fun"
)

// ResolvePredictFunBaseURL picks the explicit override when set and otherwise
// follows the testnet flag, so `PREDICTFUN_TESTNET=true` alone cannot end up
// talking to production.
func (c Config) ResolvePredictFunBaseURL() string {
	if url := trimURL(c.PredictFunBaseURL); url != "" {
		return url
	}
	if c.PredictFunTestnet {
		return PredictFunTestnetBaseURL
	}
	return PredictFunDefaultBaseURL
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envAny(keys []string, fallback string) string {
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return fallback
}

// envBool treats "1", "true", "yes" and "on" (any case) as true. Anything
// else, including an unset variable, is false: an opt-in flag must never be
// switched on by a typo.
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func trimURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

func envInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		// -1 is deliberately invalid so the live executor reports a clear
		// configuration error instead of silently falling back to EOA.
		return -1
	}
	return parsed
}
