// Package notify posts messages to a Mattermost channel via an incoming webhook.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Notifier delivers channel announcements. Implementations must not block.
type Notifier interface {
	Notify(text string)
}

const (
	queueSize   = 100
	maxAttempts = 3
)

// Webhook is a Notifier backed by a Mattermost incoming webhook.
//
// Messages are queued and delivered in order by a single worker so that slash
// commands never wait on the network. Failed deliveries are retried a few times
// (network errors and 5xx only); persistent failures are logged and dropped.
type Webhook struct {
	url    string
	client *http.Client
	retry  time.Duration // base backoff between attempts

	queue  chan string
	mu     sync.Mutex // guards closed and sends on queue
	closed bool

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// NewWebhook creates a Webhook notifier and starts its delivery worker.
func NewWebhook(webhookURL string) *Webhook {
	ctx, cancel := context.WithCancel(context.Background())
	w := &Webhook{
		url:    webhookURL,
		client: &http.Client{Timeout: 5 * time.Second},
		retry:  time.Second,
		queue:  make(chan string, queueSize),
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go w.run()
	return w
}

// Notify enqueues text for delivery. It never blocks.
func (w *Webhook) Notify(text string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		slog.Warn("Webhook notifier closed; dropping message")
		return
	}
	select {
	case w.queue <- text:
	default:
		slog.Error("Webhook queue full; dropping message")
	}
}

// Close stops accepting messages and waits for queued ones to be delivered, or
// until ctx expires (in which case in-flight delivery is abandoned).
func (w *Webhook) Close(ctx context.Context) error {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.queue)
	}
	w.mu.Unlock()

	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		w.cancel()
		return fmt.Errorf("webhook queue not fully drained: %w", ctx.Err())
	}
}

func (w *Webhook) run() {
	defer close(w.done)
	for text := range w.queue {
		w.deliver(text)
	}
}

func (w *Webhook) deliver(text string) {
	body, _ := json.Marshal(map[string]string{"text": text})

	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var retryable bool
		retryable, err = w.post(body)
		if err == nil {
			return
		}
		if !retryable || attempt == maxAttempts {
			break
		}
		slog.Warn("Webhook delivery failed; retrying", "attempt", attempt, "error", err)
		select {
		case <-time.After(w.retry * time.Duration(attempt)):
		case <-w.ctx.Done():
			return
		}
	}
	slog.Error("Webhook delivery failed; message dropped", "error", err)
}

func (w *Webhook) post(body []byte) (retryable bool, err error) {
	req, err := http.NewRequestWithContext(w.ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return false, sanitize(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		return true, sanitize(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		return false, nil
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	return resp.StatusCode >= 500, fmt.Errorf("webhook returned %s: %s", resp.Status, bytes.TrimSpace(snippet))
}

// sanitize strips the request URL from net/http errors: webhook URLs embed a
// secret and must not end up in logs.
func sanitize(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}
