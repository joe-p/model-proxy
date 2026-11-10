package logging

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// HTTPSinkOptions configures an outbound access-log sink. The sink accepts
// newline-delimited JSON records and posts them in batches to URL.
type HTTPSinkOptions struct {
	URL           string
	BatchSize     int
	FlushInterval time.Duration
	Timeout       time.Duration
	QueueSize     int
	Logger        *slog.Logger
}

// HTTPSink queues access-log records and delivers them to an HTTP collector.
// Write never blocks the caller: when the queue is full or the collector is
// unavailable, records are dropped and counted.
type HTTPSink struct {
	url           string
	client        *http.Client
	batchSize     int
	flushInterval time.Duration
	timeout       time.Duration
	logger        *slog.Logger

	queue chan []byte
	stop  chan struct{}
	done  chan struct{}

	closed         atomic.Bool
	closeOnce      sync.Once
	dropped        atomic.Uint64
	failures       atomic.Uint64
	lastFailureLog atomic.Int64
}

// NewHTTPSink starts an HTTP access-log sink.
func NewHTTPSink(opts HTTPSinkOptions) (*HTTPSink, error) {
	endpoint := strings.TrimSpace(opts.URL)
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("access sink url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("access sink url must use http or https")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("access sink url must include a host")
	}
	if opts.BatchSize <= 0 {
		return nil, fmt.Errorf("access sink batch size must be positive")
	}
	if opts.FlushInterval <= 0 {
		return nil, fmt.Errorf("access sink flush interval must be positive")
	}
	if opts.Timeout <= 0 {
		return nil, fmt.Errorf("access sink timeout must be positive")
	}
	if opts.QueueSize <= 0 {
		return nil, fmt.Errorf("access sink queue size must be positive")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	s := &HTTPSink{
		url: endpoint,
		client: &http.Client{
			Timeout: opts.Timeout,
			// A redirect would otherwise turn the POST into a GET and silently
			// lose the batch. Treat it as a failed delivery and retry.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		batchSize:     opts.BatchSize,
		flushInterval: opts.FlushInterval,
		timeout:       opts.Timeout,
		logger:        logger,
		queue:         make(chan []byte, opts.QueueSize),
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}
	go s.run()
	return s, nil
}

// Write enqueues one or more newline-delimited JSON records. It always reports
// success so a failed sink can never fail the request being logged.
func (s *HTTPSink) Write(p []byte) (int, error) {
	if s.closed.Load() {
		return len(p), nil
	}
	for _, line := range bytes.Split(p, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		record := append([]byte(nil), line...)
		select {
		case s.queue <- record:
		default:
			s.recordDropped()
		}
	}
	return len(p), nil
}

// Close stops accepting records, drains the queue, and makes one final delivery
// attempt. It waits for the worker or ctx, whichever comes first.
func (s *HTTPSink) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		close(s.stop)
	})
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Dropped reports how many records were discarded because the queue was full.
func (s *HTTPSink) Dropped() uint64 {
	return s.dropped.Load()
}

func (s *HTTPSink) run() {
	defer close(s.done)
	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()

	var pending [][]byte
	for {
		select {
		case record := <-s.queue:
			pending = append(pending, record)
			if len(pending) >= s.batchSize {
				pending = s.flushWithRetry(pending)
			}
		case <-ticker.C:
			if len(pending) > 0 {
				pending = s.flushWithRetry(pending)
			}
		case <-s.stop:
			// Drain whatever was queued before Close, then make one final
			// attempt. A slow or missing collector must not block shutdown.
		drain:
			for {
				select {
				case record := <-s.queue:
					pending = append(pending, record)
				default:
					break drain
				}
			}
			if len(pending) > 0 {
				s.flushFinal(pending)
			}
			return
		}
	}
}

func (s *HTTPSink) flushWithRetry(batch [][]byte) [][]byte {
	if len(batch) == 0 {
		return batch
	}
	backoff := 250 * time.Millisecond
	for {
		err := s.post(context.Background(), batch)
		if err == nil {
			s.recordSuccess()
			return batch[:0]
		}
		s.recordFailure(err, backoff)

		select {
		case <-s.stop:
			return batch
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func (s *HTTPSink) flushFinal(batch [][]byte) {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	if err := s.post(ctx, batch); err != nil {
		s.recordFailure(err, 0)
		return
	}
	s.recordSuccess()
}

func (s *HTTPSink) post(ctx context.Context, batch [][]byte) error {
	var body bytes.Buffer
	body.Grow(len(batch) * 256)
	body.WriteString(`{"records":[`)
	for i, record := range batch {
		if i > 0 {
			body.WriteByte(',')
		}
		body.Write(record)
	}
	body.WriteString(`]}`)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "model-proxy")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("collector returned %s", resp.Status)
	}
	return nil
}

func (s *HTTPSink) recordDropped() {
	n := s.dropped.Add(1)
	if n == 1 || n%1000 == 0 {
		s.logger.Warn("access log sink queue full; dropping records", "dropped", n)
	}
}

func (s *HTTPSink) recordFailure(err error, retryIn time.Duration) {
	n := s.failures.Add(1)
	now := time.Now().UnixNano()
	last := s.lastFailureLog.Load()
	// Make the first few attempts visible, then log periodically rather than
	// once per retry. A final failure is always logged.
	if n <= 3 || n%10 == 0 || retryIn <= 0 || now-last >= int64(time.Minute) {
		s.lastFailureLog.Store(now)
		attrs := []any{"failures", n, "error", err}
		if retryIn > 0 {
			attrs = append(attrs, "retry_in", retryIn.String())
		}
		s.logger.Warn("access log sink delivery failed", attrs...)
	}
}

func (s *HTTPSink) recordSuccess() {
	n := s.failures.Swap(0)
	if n > 0 {
		s.lastFailureLog.Store(0)
		s.logger.Info("access log sink recovered", "failures", n)
	}
}
