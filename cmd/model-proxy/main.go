package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

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
	logs, _, err := newLogRuntime(cfg, nil)
	if err != nil {
		return err
	}
	logger := logs.logger
	providers, err := buildProviders(cfg)
	if err != nil {
		closeSink(logs.sink, cfg.Server.ShutdownTimeout)
		return err
	}
	pricingSvc, pricingCancel := startPricing(cfg, logger)

	// Reload is served over the administrative API; there is no signal-driven
	// path. The reloader is attached before the listeners start so a request can
	// never observe a reload that has no server to apply itself to.
	rl := newReloader(cfg, logs, pricingCancel)
	srv := server.New(cfg, providers, logger, costEstimator(pricingSvc), server.WithReloader(rl))
	rl.attach(srv)

	// Release whatever the *current* configuration owns, which may differ from the
	// startup values after a successful reload.
	defer func() {
		currentLogs, currentCancel, timeout := rl.current()
		if currentCancel != nil {
			currentCancel()
		}
		closeSink(currentLogs.sink, timeout)
	}()

	logger.Info("starting model-proxy",
		"version", version,
		"listen", cfg.Server.Listen,
		"admin_listen", cfg.Server.AdminListen,
		"models", len(cfg.Models),
		"providers", len(cfg.Providers),
		"policies", len(cfg.Policies),
		"env_file", cfg.EnvFile,
		"key_file", cfg.KeyFile,
		"access_sink", cfg.Logging.AccessSink.Enabled(),
	)
	if !cfg.Server.AdminEnabled() {
		logger.Warn("configuration reload is disabled",
			"hint", "set server.admin_listen and server.admin_token_env to enable POST /admin/reload")
	} else if !isLoopbackAddr(cfg.Server.AdminListen) {
		logger.Warn("admin api is not bound to a loopback address", "admin_listen", cfg.Server.AdminListen)
	}

	errCh := make(chan error, 2)
	go func() {
		errCh <- srv.ListenAndServe()
	}()
	if srv.AdminEnabled() {
		go func() {
			errCh <- srv.ListenAndServeAdmin()
		}()
	}

	// Only lifecycle signals are handled. SIGHUP is deliberately left to the
	// default disposition, which terminates the process: it is not a reload
	// trigger. Configuration reload is served by POST /admin/reload.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case sig := <-sigCh:
		logger.Info("shutting down", "signal", sig.String())
		return shutdown(srv)
	case err := <-errCh:
		_ = shutdown(srv)
		return err
	}
}

func shutdown(srv *server.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), srv.ShutdownTimeout())
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// isLoopbackAddr reports whether addr binds only to the local host, which is the
// expected placement for the admin listener.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// logRuntime owns the logger and, optionally, the HTTP access-log sink that
// receives request records in addition to stdout.
type logRuntime struct {
	logger  *slog.Logger
	sink    *logging.HTTPSink
	sinkCfg config.AccessSinkConfig
}

// newLogRuntime builds a logger. When the sink settings are unchanged, it
// reuses the existing sink so a reload does not interrupt delivery.
func newLogRuntime(cfg *config.Config, current *logRuntime) (*logRuntime, bool, error) {
	base := logging.New(cfg.Logging.Format, cfg.Logging.Level, os.Stdout)
	sinkCfg := cfg.Logging.AccessSink

	if !sinkCfg.Enabled() {
		return &logRuntime{logger: base}, false, nil
	}
	if current != nil && current.sink != nil && current.sinkCfg == sinkCfg {
		logger := logging.New(cfg.Logging.Format, cfg.Logging.Level, os.Stdout, current.sink)
		return &logRuntime{logger: logger, sink: current.sink, sinkCfg: sinkCfg}, true, nil
	}

	sink, err := logging.NewHTTPSink(logging.HTTPSinkOptions{
		URL:           sinkCfg.URL,
		BatchSize:     sinkCfg.BatchSize,
		FlushInterval: sinkCfg.FlushInterval,
		Timeout:       sinkCfg.Timeout,
		QueueSize:     sinkCfg.QueueSize,
		Logger:        base,
	})
	if err != nil {
		return nil, false, err
	}
	logger := logging.New(cfg.Logging.Format, cfg.Logging.Level, os.Stdout, sink)
	return &logRuntime{logger: logger, sink: sink, sinkCfg: sinkCfg}, false, nil
}

func closeSink(sink *logging.HTTPSink, timeout time.Duration) {
	if sink == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_ = sink.Close(ctx)
}

func closeSinkLater(sink *logging.HTTPSink, delay time.Duration) {
	if sink == nil {
		return
	}
	go func() {
		time.Sleep(delay)
		closeSink(sink, delay)
	}()
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
