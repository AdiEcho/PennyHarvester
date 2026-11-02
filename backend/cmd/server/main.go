package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"pennyharvester/backend/internal/api"
	"pennyharvester/backend/internal/config"
	"pennyharvester/backend/internal/engine"
	"pennyharvester/backend/internal/live"
	"pennyharvester/backend/internal/market"
	"pennyharvester/backend/internal/netproxy"
	"pennyharvester/backend/internal/predictfun"
	"pennyharvester/backend/internal/store"
	"pennyharvester/backend/internal/venue"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg := config.FromEnv()
	if cfg.DatabasePath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(cfg.DatabasePath), 0o750); err != nil {
			return err
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	repository, err := store.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer repository.Close()

	systemSettings, err := repository.LoadSystemSettings(ctx)
	if err != nil {
		return err
	}
	proxyManager, err := netproxy.New(systemSettings.Proxy)
	if err != nil {
		// A bad stored row must not stop the process: fall back to the
		// environment, and let the system settings page surface the value.
		log.Printf("系统代理配置无效，回退到环境变量模式: %v", err)
		proxyManager = netproxy.NewMust(netproxy.DefaultConfig())
	}

	provider := market.NewGammaClient(cfg.GammaBaseURL, proxyManager)
	liveExecutor := live.New(live.Config{
		PrivateKey:    cfg.PolymarketPrivateKey,
		Funder:        cfg.PolymarketFunder,
		SignatureType: cfg.PolymarketSignatureType,
		CLOBURL:       cfg.PolymarketCLOBURL,
		PolygonRPC:    cfg.PolygonRPCURL,
		APIKey:        cfg.PolymarketAPIKey,
		APISecret:     cfg.PolymarketAPISecret,
		APIPassphrase: cfg.PolymarketAPIPassphrase,
		BuilderCode:   config.BuilderCode,
		Proxy:         proxyManager,
	})
	strategyEngine, err := engine.New(ctx, repository, buildVenues(cfg, proxyManager, provider, liveExecutor), proxyManager)
	if err != nil {
		return err
	}
	strategyEngine.Start(ctx)
	defer strategyEngine.Stop()

	authenticator := api.NewAuthenticator(repository)
	passwordProtected, err := authenticator.Enabled(ctx)
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.NewRouter(strategyEngine, cfg.WebDir, authenticator),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Printf("PennyHarvester listening on http://%s | source=%s | venues=%d | proxy=%s | access_password=%t | default=LIVE | live_configured=%t pf_enabled=%t",
		cfg.Addr, cfg.GammaBaseURL, len(strategyEngine.Venues()), proxyManager.Config().Mode, passwordProtected, liveExecutor.Configured(), cfg.PredictFunEnabled)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// buildVenues registers every venue this process can serve. Polymarket is
// always registered: its paper accounts must keep working without credentials,
// and its live executor reports itself as unconfigured when it is. Predict.fun
// is opt-in, so a half-filled BSC block can never start trading by accident.
func buildVenues(cfg config.Config, proxyManager *netproxy.Manager, provider *market.GammaClient, liveExecutor *live.Executor) *venue.Registry {
	registry := venue.NewRegistry()
	if err := registry.Register(venue.PolymarketCapabilities(), provider, liveExecutor); err != nil {
		log.Printf("跳过 Polymarket 场所注册: %v", err)
	}
	if !cfg.PredictFunEnabled {
		return registry
	}
	pfConfig := predictfun.Config{
		BaseURL:    cfg.PredictFunBaseURL,
		APIKey:     cfg.PredictFunAPIKey,
		PrivateKey: cfg.PredictFunPrivateKey,
		Maker:      cfg.PredictFunSmartWallet,
		Testnet:    cfg.PredictFunTestnet,
		Proxy:      proxyManager,
		RPCURL:     cfg.PredictFunRPC,
	}
	pfProvider, pfExecutor, err := predictfun.New(pfConfig)
	if err != nil {
		// A bad key or endpoint must not take the whole process down; the
		// system page surfaces the venue as missing until it is fixed.
		log.Printf("Predict.fun 场所初始化失败，已跳过: %v", err)
		return registry
	}
	if err := registry.Register(predictfun.Capabilities(pfConfig), pfProvider, pfExecutor); err != nil {
		log.Printf("跳过 Predict.fun 场所注册: %v", err)
	}
	return registry
}
