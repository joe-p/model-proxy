package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/joe-p/model-proxy/internal/config"
	"github.com/joe-p/model-proxy/internal/server"
)

// reloader applies configuration changes to the running process over the
// administrative HTTP API. It owns the resources a reload replaces - the
// access-log sink and the pricing refresh loop - because those live in this
// package rather than in the server.
//
// Every reload is built in full before anything is swapped, so a rejected or
// invalid configuration leaves the running process untouched.
type reloader struct {
	mu              sync.Mutex
	path            string
	shutdownTimeout time.Duration
	logs            *logRuntime
	pricingCancel   context.CancelFunc
	hash            string
	applied         *config.Config
	srv             *server.Server
}

func newReloader(cfg *config.Config, logs *logRuntime, pricingCancel context.CancelFunc) *reloader {
	r := &reloader{
		path:            cfg.SourcePath,
		shutdownTimeout: cfg.Server.ShutdownTimeout,
		logs:            logs,
		pricingCancel:   pricingCancel,
		applied:         cfg,
	}
	if hash, err := hashFile(r.path); err == nil {
		// Remember the version that is already applied so that an unchanged file
		// is a cheap no-op instead of a rebuild.
		r.hash = hash
	}
	return r
}

// attach wires the server in. It must be called before the listeners start
// serving, because the server is constructed with this reloader.
func (r *reloader) attach(srv *server.Server) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.srv = srv
}

// current returns the live log runtime and pricing loop that shutdown must
// release. A reload replaces both, so a shutdown must not use the startup
// values captured by run().
func (r *reloader) current() (*logRuntime, context.CancelFunc, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.logs, r.pricingCancel, r.shutdownTimeout
}

func (r *reloader) ReloadConfig(ctx context.Context, force bool) (server.ReloadResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.srv == nil {
		return server.ReloadResult{}, fmt.Errorf("reload is not available")
	}

	sum, err := hashFile(r.path)
	if err != nil {
		return server.ReloadResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return server.ReloadResult{}, err
	}
	if !force && sum == r.hash {
		// Report the counts of the configuration that is already applied so a
		// no-op is still informative.
		return server.ReloadResult{Changed: false, ConfigHash: sum, Models: len(r.applied.Models),
			Providers:  len(r.applied.Providers),
			Keys:       len(r.applied.Keys),
			Policies:   len(r.applied.Policies),
			AccessSink: r.applied.Logging.AccessSink.Enabled(),
		}, nil
	}

	cfg, err := config.Load(r.path)
	if err != nil {
		return server.ReloadResult{}, err
	}
	providers, err := buildProviders(cfg)
	if err != nil {
		return server.ReloadResult{}, err
	}
	logs, reused, err := newLogRuntime(cfg, r.logs)
	if err != nil {
		return server.ReloadResult{}, err
	}
	pricingSvc, pricingCancel := startPricing(cfg, logs.logger)

	if err := r.srv.Reload(cfg, providers, logs.logger, costEstimator(pricingSvc)); err != nil {
		// The live state was not touched: release everything built for the
		// rejected configuration.
		if pricingCancel != nil {
			pricingCancel()
		}
		if !reused {
			closeSink(logs.sink, cfg.Server.ShutdownTimeout)
		}
		return server.ReloadResult{}, err
	}

	oldLogs, oldCancel, oldTimeout := r.logs, r.pricingCancel, r.shutdownTimeout
	r.logs, r.pricingCancel, r.shutdownTimeout = logs, pricingCancel, cfg.Server.ShutdownTimeout
	r.hash = sum
	r.applied = cfg

	// Only now is it safe to retire the resources the old configuration owned.
	if oldCancel != nil {
		oldCancel()
	}
	if !reused {
		closeSinkLater(oldLogs.sink, oldTimeout)
	}

	logs.logger.Info("configuration reloaded",
		"config", r.path,
		"models", len(cfg.Models),
		"providers", len(cfg.Providers),
		"keys", len(cfg.Keys),
		"policies", len(cfg.Policies),
		"access_sink", cfg.Logging.AccessSink.Enabled(),
		"pricing", cfg.Pricing.Enabled(),
	)

	return server.ReloadResult{
		Changed:    true,
		ConfigHash: sum,
		Models:     len(cfg.Models),
		Providers:  len(cfg.Providers),
		Keys:       len(cfg.Keys),
		Policies:   len(cfg.Policies),
		AccessSink: cfg.Logging.AccessSink.Enabled(),
	}, nil
}

func hashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read config %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
