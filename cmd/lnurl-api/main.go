package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/max-gy/lnurl-nwc-payment-proxy/internal/pay"
)

func main() {
	path := flag.String("config", "/etc/lnurl-api/config.json", "JSON configuration file")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(*path, log); err != nil {
		log.Error("API stopped", "error", err)
		os.Exit(1)
	}
}

func run(path string, log *slog.Logger) error {
	cfg, err := pay.LoadConfig(path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := pay.OpenStore(cfg.DBPath, cfg.MaxQuotes)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Prune(time.Now()); err != nil {
		return fmt.Errorf("prune expired quotes: %w", err)
	}
	wallets := make(map[string]pay.Wallet)
	var clients []*pay.NWC
	for name, wc := range cfg.Wallets {
		client, err := pay.NewNWC(wc, log.With("wallet", name))
		if err != nil {
			return fmt.Errorf("initialize wallet %s: %w", name, err)
		}
		wallets[name] = client
		clients = append(clients, client)
	}
	var workers sync.WaitGroup
	defer workers.Wait()
	defer stop()
	for _, client := range clients {
		workers.Go(func() { client.Run(ctx) })
	}
	workers.Go(func() {
		ticker := time.NewTicker(pay.QuotePruneInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := store.Prune(time.Now()); err != nil {
					log.Error("Cannot prune expired quotes", "error", err)
				}
			}
		}
	})
	api := pay.NewAPI(cfg, store, pay.NewRates(time.Duration(cfg.RateMaxAgeSeconds)*time.Second), wallets, log)
	server := &http.Server{
		Addr: cfg.Listen, Handler: api.Handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: cfg.Timeout() + 10*time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	log.Info("LNURL API starting", "listen", cfg.Listen)
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			server.Close()
			return err
		}
		return nil
	}
}
