package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type sinkPayload struct {
	Records []map[string]any `json:"records"`
}

func testSink(t *testing.T, handler http.HandlerFunc, queueSize int) (*HTTPSink, *slog.Logger) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	sink, err := NewHTTPSink(HTTPSinkOptions{
		URL:           server.URL + "/ingest",
		BatchSize:     2,
		FlushInterval: 10 * time.Millisecond,
		Timeout:       time.Second,
		QueueSize:     queueSize,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewHTTPSink: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = sink.Close(ctx)
	})
	return sink, New("json", slog.LevelDebug, io.Discard, sink)
}

func TestHTTPSinkForwardsOnlyRequestRecords(t *testing.T) {
	payloads := make(chan sinkPayload, 4)
	sink, logger := testSink(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q", got)
		}
		var payload sinkPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode body: %v", err)
		}
		payloads <- payload
		w.WriteHeader(http.StatusAccepted)
	}, 16)

	logger.Info("starting model-proxy", "version", "test")
	logger.Info("request", "request_id", "r1", "status", 200)
	logger.Warn("configuration reload failed", "error", "bad file")
	logger.Info("request", "request_id", "r2", "status", 500)

	var records []map[string]any
	deadline := time.After(2 * time.Second)
	for len(records) < 2 {
		select {
		case payload := <-payloads:
			records = append(records, payload.Records...)
		case <-deadline:
			t.Fatalf("timed out waiting for sink payloads; got %+v", records)
		}
	}
	if len(records) != 2 {
		t.Fatalf("records = %d", len(records))
	}
	for _, record := range records {
		if record["msg"] != "request" {
			t.Fatalf("forwarded non-request record: %+v", record)
		}
	}
	if records[0]["request_id"] != "r1" || records[1]["request_id"] != "r2" {
		t.Fatalf("unexpected records: %+v", records)
	}
	if got := sink.Dropped(); got != 0 {
		t.Fatalf("dropped = %d", got)
	}
}

func TestHTTPSinkCloseFlushesPending(t *testing.T) {
	payloads := make(chan sinkPayload, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload sinkPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode body: %v", err)
		}
		payloads <- payload
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	sink, err := NewHTTPSink(HTTPSinkOptions{
		URL:           server.URL,
		BatchSize:     100,
		FlushInterval: time.Hour,
		Timeout:       time.Second,
		QueueSize:     8,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewHTTPSink: %v", err)
	}
	logger := New("json", slog.LevelInfo, io.Discard, sink)
	logger.Info("request", "request_id", "r1")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sink.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case payload := <-payloads:
		if len(payload.Records) != 1 || payload.Records[0]["request_id"] != "r1" {
			t.Fatalf("unexpected payload: %+v", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not deliver the pending batch")
	}
}

func TestHTTPSinkRetriesFailedDelivery(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	payloads := make(chan sinkPayload, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		attempt := attempts
		mu.Unlock()

		if attempt == 1 {
			http.Error(w, "try again", http.StatusServiceUnavailable)
			return
		}
		var payload sinkPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode body: %v", err)
		}
		payloads <- payload
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	sink, err := NewHTTPSink(HTTPSinkOptions{
		URL:           server.URL,
		BatchSize:     1,
		FlushInterval: 10 * time.Millisecond,
		Timeout:       time.Second,
		QueueSize:     8,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewHTTPSink: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = sink.Close(ctx)
	}()

	logger := New("json", slog.LevelInfo, io.Discard, sink)
	logger.Info("request", "request_id", "r1")

	select {
	case payload := <-payloads:
		if len(payload.Records) != 1 {
			t.Fatalf("records = %d", len(payload.Records))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("sink did not retry the failed delivery")
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts < 2 {
		t.Fatalf("attempts = %d", attempts)
	}
}

func TestHTTPSinkQueueFullDropsWithoutBlocking(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-release
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	sink, err := NewHTTPSink(HTTPSinkOptions{
		URL:           server.URL,
		BatchSize:     1,
		FlushInterval: time.Hour,
		Timeout:       2 * time.Second,
		QueueSize:     1,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewHTTPSink: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = sink.Close(ctx)
	}()
	defer close(release)

	logger := New("json", slog.LevelInfo, io.Discard, sink)
	logger.Info("request", "request_id", "r1")
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("sink did not start its first delivery")
	}

	// The worker is blocked on r1, so r2 fills the queue and r3 is dropped.
	logger.Info("request", "request_id", "r2")
	logger.Info("request", "request_id", "r3")
	if got := sink.Dropped(); got < 1 {
		t.Fatalf("dropped = %d, want at least 1", got)
	}
}

func TestHTTPSinkLogsRetriesAndRecovery(t *testing.T) {
	var attempts atomic.Int32
	delivered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) <= 2 {
			http.Error(w, "try again", http.StatusServiceUnavailable)
			return
		}
		delivered <- struct{}{}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	var logs bytes.Buffer
	sinkLogger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sink, err := NewHTTPSink(HTTPSinkOptions{
		URL:           server.URL,
		BatchSize:     1,
		FlushInterval: 10 * time.Millisecond,
		Timeout:       time.Second,
		QueueSize:     8,
		Logger:        sinkLogger,
	})
	if err != nil {
		t.Fatalf("NewHTTPSink: %v", err)
	}

	logger := New("json", slog.LevelInfo, io.Discard, sink)
	logger.Info("request", "request_id", "r1")
	select {
	case <-delivered:
	case <-time.After(3 * time.Second):
		t.Fatal("sink did not recover")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := sink.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	output := logs.String()
	if !strings.Contains(output, "access log sink delivery failed") {
		t.Fatalf("missing retry warning: %s", output)
	}
	if !strings.Contains(output, "retry_in=250ms") {
		t.Fatalf("missing retry delay: %s", output)
	}
	if !strings.Contains(output, "access log sink recovered") {
		t.Fatalf("missing recovery message: %s", output)
	}
}
