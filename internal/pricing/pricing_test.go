package pricing

import (
	"context"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

const testCatalog = `{
  "openai": {
    "id": "openai",
    "models": {
      "gpt-test": {
        "id": "gpt-test",
        "cost": {
          "input": 2,
          "output": 10,
          "cache_read": 0.1,
          "cache_write": 2.5,
          "tiers": [
            {
              "input": 4,
              "output": 15,
              "cache_read": 0.2,
              "cache_write": 5,
              "tier": {"type": "context", "size": 100}
            }
          ]
        }
      }
    }
  }
}`

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func int64ptr(value int64) *int64 {
	return &value
}

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-12
}

func TestEstimateUsageBaseAndCached(t *testing.T) {
	rates := Rates{Input: 2, CachedInput: 0.1, Output: 10, CacheWrite: 2.5}
	got := EstimateUsage(Usage{
		PromptTokens:     1000,
		CompletionTokens: 300,
		CachedTokens:     int64ptr(400),
	}, rates)
	want := (600*2.0 + 400*0.1 + 300*10.0) / 1_000_000
	if !almostEqual(got, want) {
		t.Fatalf("cost = %v, want %v", got, want)
	}
}

func TestEstimateUsageCacheWrite(t *testing.T) {
	rates := Rates{Input: 2, CachedInput: 0.1, Output: 10, CacheWrite: 2.5}
	got := EstimateUsage(Usage{
		PromptTokens:     1000,
		CompletionTokens: 0,
		CachedTokens:     int64ptr(200),
		CacheWriteTokens: int64ptr(100),
	}, rates)
	want := (700*2.0 + 200*0.1 + 100*2.5) / 1_000_000
	if !almostEqual(got, want) {
		t.Fatalf("cost = %v, want %v", got, want)
	}
}

func TestEstimateUsageClampsOverlappingCounts(t *testing.T) {
	rates := Rates{Input: 2, CachedInput: 0.1, Output: 10, CacheWrite: 2.5}
	got := EstimateUsage(Usage{
		PromptTokens: 100,
		CachedTokens: int64ptr(80),
	}, rates)
	want := (20*2.0 + 80*0.1) / 1_000_000
	if !almostEqual(got, want) {
		t.Fatalf("cost = %v, want %v", got, want)
	}

	got = EstimateUsage(Usage{
		PromptTokens: 100,
		CachedTokens: int64ptr(200),
	}, rates)
	if got < 0 {
		t.Fatalf("cost must not be negative: %v", got)
	}
}

func TestLookupRatesAppliesContextTier(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testCatalog))
	}))
	defer server.Close()

	svc := New(Options{URL: server.URL, Logger: testLogger()})
	if !svc.Refresh(context.Background()) {
		t.Fatal("Refresh failed")
	}

	base, ok := svc.LookupRates("openai", "gpt-test", 50)
	if !ok {
		t.Fatal("base rates not found")
	}
	if base.Input != 2 || base.Output != 10 || base.CachedInput != 0.1 || base.CacheWrite != 2.5 {
		t.Fatalf("unexpected base rates: %+v", base)
	}

	tiered, ok := svc.LookupRates("openai", "gpt-test", 200)
	if !ok {
		t.Fatal("tier rates not found")
	}
	if tiered.Input != 4 || tiered.Output != 15 || tiered.CachedInput != 0.2 || tiered.CacheWrite != 5 {
		t.Fatalf("unexpected tier rates: %+v", tiered)
	}

	if _, ok := svc.LookupRates("missing", "gpt-test", 50); ok {
		t.Fatal("missing provider should not resolve")
	}
	if _, ok := svc.LookupRates("openai", "missing", 50); ok {
		t.Fatal("missing model should not resolve")
	}
}

func TestRefreshWritesAndLoadsCache(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testCatalog))
	}))
	defer server.Close()

	cachePath := filepath.Join(t.TempDir(), "models.dev.json")
	svc := New(Options{URL: server.URL, CachePath: cachePath, Logger: testLogger()})
	if !svc.Refresh(context.Background()) {
		t.Fatal("Refresh failed")
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache file missing: %v", err)
	}

	// A new service with an unreachable URL must still load the cache.
	reloaded := New(Options{URL: "http://127.0.0.1:1", CachePath: cachePath, Logger: testLogger()})
	cost, ok := reloaded.Estimate("openai", "gpt-test", Usage{PromptTokens: 1000, CompletionTokens: 300})
	if !ok {
		t.Fatal("cached rates not loaded")
	}
	want := (1000*4.0 + 300*15.0) / 1_000_000
	if !almostEqual(cost, want) {
		t.Fatalf("cost = %v, want %v", cost, want)
	}
}

func TestTimerRefreshesCatalog(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testCatalog))
	}))
	defer server.Close()

	svc := New(Options{
		URL:             server.URL,
		RefreshInterval: 20 * time.Millisecond,
		Logger:          testLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for requests.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := requests.Load(); got < 2 {
		t.Fatalf("refresh requests = %d, want at least 2", got)
	}
}

func TestRefreshFailureKeepsPreviousCatalog(t *testing.T) {
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testCatalog))
	}))
	defer server.Close()

	svc := New(Options{URL: server.URL, Logger: testLogger()})
	if !svc.Refresh(context.Background()) {
		t.Fatal("initial Refresh failed")
	}
	fail.Store(true)
	if svc.Refresh(context.Background()) {
		t.Fatal("Refresh should fail")
	}
	if _, ok := svc.LookupRates("openai", "gpt-test", 50); !ok {
		t.Fatal("previous catalog should remain available")
	}
}
