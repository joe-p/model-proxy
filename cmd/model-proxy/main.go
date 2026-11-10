package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/joe-p/model-proxy/internal/config"
	"github.com/joe-p/model-proxy/internal/logging"
	"github.com/joe-p/model-proxy/internal/pricing"
	"github.com/joe-p/model-proxy/internal/provider"
	"github.com/joe-p/model-proxy/internal/provider/codex"
	"github.com/joe-p/model-proxy/internal/provider/openai"
	"github.com/joe-p/model-proxy/internal/provider/openrouter"
	"github.com/joe-p/model-proxy/internal/server"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "model-proxy:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("model-proxy", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to JSON configuration")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *showVersion {
		fmt.Printf("model-proxy %s\n", version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	logger := logging.New(cfg.Logging.Format, cfg.Logging.Level, os.Stderr)
	providers, err := buildProviders(cfg)
	if err != nil {
		return err
	}
	pricingSvc, pricingCancel := startPricing(cfg, logger)
	defer func() {
		if pricingCancel != nil {
			pricingCancel()
		}
	}()

	srv := server.New(cfg, providers, logger, costEstimator(pricingSvc))
	logger.Info("starting model-proxy",
		"version", version,
		"listen", cfg.Server.Listen,
		"models", len(cfg.Models),
		"providers", len(cfg.Providers),
		"policies", len(cfg.Policies),
		"env_file", cfg.EnvFile,
	)

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	for {
		select {
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				newCancel, err := reload(srv, *configPath)
				if err != nil {
					logger.Error("configuration reload failed", "error", err)
					continue
				}
				if pricingCancel != nil {
					pricingCancel()
				}
				pricingCancel = newCancel
				continue
			}

			logger.Info("shutting down", "signal", sig.String())
			ctx, cancel := context.WithTimeout(context.Background(), srv.ShutdownTimeout())
			shutdownErr := srv.Shutdown(ctx)
			cancel()
			if shutdownErr != nil {
				return fmt.Errorf("shutdown: %w", shutdownErr)
			}
			return nil

		case err := <-errCh:
			return err
		}
	}
}

func reload(srv *server.Server, path string) (context.CancelFunc, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	providers, err := buildProviders(cfg)
	if err != nil {
		return nil, err
	}
	logger := logging.New(cfg.Logging.Format, cfg.Logging.Level, os.Stderr)
	pricingSvc, pricingCancel := startPricing(cfg, logger)
	if err := srv.Reload(cfg, providers, logger, costEstimator(pricingSvc)); err != nil {
		if pricingCancel != nil {
			pricingCancel()
		}
		return nil, err
	}
	logger.Info("configuration reloaded",
		"models", len(cfg.Models),
		"providers", len(cfg.Providers),
		"keys", len(cfg.Keys),
		"policies", len(cfg.Policies),
	)
	return pricingCancel, nil
}

// costEstimator wraps a possibly nil *pricing.Service so that callers receive a
// nil interface value. Storing a nil pointer in the interface would defeat the
// server's nil check and panic on the first estimate.
func costEstimator(svc *pricing.Service) server.CostEstimator {
	if svc == nil {
		return nil
	}
	return svc
}

// startPricing starts the optional models.dev refresh loop. The returned
// cancel function stops it; nil means pricing is disabled.
func startPricing(cfg *config.Config, logger *slog.Logger) (*pricing.Service, context.CancelFunc) {
	if !cfg.Pricing.Enabled() {
		return nil, nil
	}
	svc := pricing.New(pricing.Options{
		URL:             cfg.Pricing.URL,
		CachePath:       cfg.Pricing.Cache,
		RefreshInterval: cfg.Pricing.RefreshInterval,
		Timeout:         cfg.Pricing.Timeout,
		Logger:          logger,
	})
	ctx, cancel := context.WithCancel(context.Background())
	svc.Start(ctx)
	return svc, cancel
}

func buildProviders(cfg *config.Config) (map[string]provider.Provider, error) {
	result := make(map[string]provider.Provider, len(cfg.Providers))
	for name, providerCfg := range cfg.Providers {
		switch providerCfg.Type {
		case "openrouter":
			p, err := openrouter.New(
				providerCfg.Name,
				providerCfg.BaseURL,
				providerCfg.APIKey,
				providerCfg.DialTimeout,
				providerCfg.ResponseHeaderTimeout,
				providerCfg.Referer,
				providerCfg.Title,
			)
			if err != nil {
				return nil, fmt.Errorf("provider %q: %w", name, err)
			}
			result[name] = p
		case "openai-compatible", "openai", "private-ai-gateway", "phala":
			p, err := openai.New(
				providerCfg.Name,
				providerCfg.BaseURL,
				providerCfg.APIKey,
				providerCfg.DialTimeout,
				providerCfg.ResponseHeaderTimeout,
			)
			if err != nil {
				return nil, fmt.Errorf("provider %q: %w", name, err)
			}
			result[name] = p
		case "codex", "chatgpt":
			p, err := codex.New(
				providerCfg.Name,
				providerCfg.BaseURL,
				providerCfg.APIKey,
				providerCfg.AuthDir,
				providerCfg.ClientVersion,
				providerCfg.DialTimeout,
				providerCfg.ResponseHeaderTimeout,
			)
			if err != nil {
				return nil, fmt.Errorf("provider %q: %w", name, err)
			}
			result[name] = p
		default:
			return nil, fmt.Errorf("provider %q has unsupported type %q", name, providerCfg.Type)
		}
	}

	for alias, model := range cfg.Models {
		if _, ok := result[model.Provider]; !ok {
			return nil, fmt.Errorf("model %q references unavailable provider %q", alias, model.Provider)
		}
	}
	return result, nil
}

func defaultConfigPath() string {
	if path := os.Getenv("MODEL_PROXY_CONFIG"); path != "" {
		return path
	}
	return "./config.json"
}
