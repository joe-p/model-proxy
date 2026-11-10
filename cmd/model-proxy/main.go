package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/joe-p/model-proxy/internal/config"
	"github.com/joe-p/model-proxy/internal/logging"
	"github.com/joe-p/model-proxy/internal/provider"
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

	srv := server.New(cfg, providers, logger)
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
				if err := reload(srv, *configPath); err != nil {
					logger.Error("configuration reload failed", "error", err)
				}
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

func reload(srv *server.Server, path string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	providers, err := buildProviders(cfg)
	if err != nil {
		return err
	}
	logger := logging.New(cfg.Logging.Format, cfg.Logging.Level, os.Stderr)
	if err := srv.Reload(cfg, providers, logger); err != nil {
		return err
	}
	logger.Info("configuration reloaded",
		"models", len(cfg.Models),
		"providers", len(cfg.Providers),
		"keys", len(cfg.Keys),
		"policies", len(cfg.Policies),
	)
	return nil
}

func buildProviders(cfg *config.Config) (map[string]provider.Provider, error) {
	result := make(map[string]provider.Provider, len(cfg.Providers))
	for name, providerCfg := range cfg.Providers {
		switch name {
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
		default:
			return nil, fmt.Errorf("provider %q is not supported", name)
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
