package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestWebhook(url string) *Webhook {
	w := NewWebhook(url)
	w.retry = 5 * time.Millisecond
	return w
}

func closeWithin(t *testing.T, w *Webhook) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestDeliversInOrderAndDrainsOnClose(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content type %q", ct)
		}
		var p struct {
			Text string `json:"text"`
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &p); err != nil {
			t.Errorf("bad payload %s", b)
		}
		mu.Lock()
		got = append(got, p.Text)
		mu.Unlock()
	}))
	defer srv.Close()

	w := newTestWebhook(srv.URL)
	for _, m := range []string{"one", "two", "three"} {
		w.Notify(m)
	}
	closeWithin(t, w)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 || got[0] != "one" || got[1] != "two" || got[2] != "three" {
		t.Fatalf("got %v", got)
	}
}

func TestRetriesServerErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			rw.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer srv.Close()

	w := newTestWebhook(srv.URL)
	w.Notify("hello")
	closeWithin(t, w)
	if calls.Load() != 3 {
		t.Fatalf("expected 3 attempts, got %d", calls.Load())
	}
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		rw.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	w := newTestWebhook(srv.URL)
	w.Notify("hello")
	closeWithin(t, w)
	if calls.Load() != 1 {
		t.Fatalf("expected 1 attempt, got %d", calls.Load())
	}
}

func TestFailureDoesNotBlockLaterMessages(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var p struct{ Text string }
		json.Unmarshal(b, &p)
		if p.Text == "bad" {
			rw.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		got = append(got, p.Text)
		mu.Unlock()
	}))
	defer srv.Close()

	w := newTestWebhook(srv.URL)
	w.Notify("bad")
	w.Notify("good")
	closeWithin(t, w)
	if len(got) != 1 || got[0] != "good" {
		t.Fatalf("got %v", got)
	}
}

func TestNotifyAfterCloseIsSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	w := newTestWebhook(srv.URL)
	closeWithin(t, w)
	w.Notify("late")  // must not panic
	closeWithin(t, w) // idempotent
}

func TestErrorsDoNotLeakWebhookURL(t *testing.T) {
	// Connection refused -> url.Error containing the full URL; make sure sanitize strips it.
	w := newTestWebhook("http://127.0.0.1:1/hooks/SECRET")
	defer w.cancel()
	_, err := w.post([]byte(`{}`))
	if err == nil {
		t.Fatal("expected error")
	}
	if s := err.Error(); contains(s, "SECRET") || contains(s, "127.0.0.1:1/hooks") {
		t.Fatalf("error leaks URL: %q", s)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
