package config

import (
	"os"
	"strconv"
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
	PolymarketAPIKey        string
	PolymarketAPISecret     string
	PolymarketAPIPassphrase string
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
		PolymarketAPIKey:        os.Getenv("POLYMARKET_API_KEY"),
		PolymarketAPISecret:     os.Getenv("POLYMARKET_API_SECRET"),
		PolymarketAPIPassphrase: os.Getenv("POLYMARKET_API_PASSPHRASE"),
	}
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

func envInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return parsed
}
