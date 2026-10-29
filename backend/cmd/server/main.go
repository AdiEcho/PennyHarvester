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

	provider := market.NewGammaClient(cfg.GammaBaseURL)
	liveExecutor := live.New(live.Config{
		PrivateKey:    cfg.PolymarketPrivateKey,
		Funder:        cfg.PolymarketFunder,
		SignatureType: cfg.PolymarketSignatureType,
		CLOBURL:       cfg.PolymarketCLOBURL,
		APIKey:        cfg.PolymarketAPIKey,
		APISecret:     cfg.PolymarketAPISecret,
		APIPassphrase: cfg.PolymarketAPIPassphrase,
	})
	strategyEngine, err := engine.New(ctx, repository, provider, liveExecutor)
	if err != nil {
		return err
	}
	strategyEngine.Start(ctx)
	defer strategyEngine.Stop()

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.NewRouter(strategyEngine, cfg.WebDir),
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
	log.Printf("PennyHarvester listening on http://%s | source=%s | default=LIVE | live_configured=%t",
		cfg.Addr, cfg.GammaBaseURL, liveExecutor.Configured())
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
