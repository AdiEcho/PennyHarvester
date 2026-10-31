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
	"pennyharvester/backend/internal/store"
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
	strategyEngine, err := engine.New(ctx, repository, provider, liveExecutor, proxyManager)
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
	log.Printf("PennyHarvester listening on http://%s | source=%s | proxy=%s | access_password=%t | default=LIVE | live_configured=%t",
		cfg.Addr, cfg.GammaBaseURL, proxyManager.Config().Mode, passwordProtected, liveExecutor.Configured())
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
