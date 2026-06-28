package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egomarker/docker-lazyloader/internal/lifecycle"
)

type fakeCompose struct {
	mu     sync.Mutex
	starts int
	stops  int
}

func (f *fakeCompose) Start() error { f.mu.Lock(); f.starts++; f.mu.Unlock(); return nil }
func (f *fakeCompose) Stop() error  { f.mu.Lock(); f.stops++; f.mu.Unlock(); return nil }
func (f *fakeCompose) Starts() int  { f.mu.Lock(); defer f.mu.Unlock(); return f.starts }

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func waitFor(t *testing.T, cond func() bool, max time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf(msg)
}

func newTestServer(t *testing.T, upstream, health string) (*Server, *lifecycle.Service, *fakeCompose) {
	t.Helper()
	fc := &fakeCompose{}
	mgr := lifecycle.NewManager(silentLogger())
	svc := lifecycle.NewService(lifecycle.ServiceConfig{
		Logger:             silentLogger(),
		Compose:            fc,
		HealthURL:          health,
		HealthExpectStatus: http.StatusOK,
		HealthTimeout:      20 * time.Millisecond,
		StartTimeout:       75 * time.Millisecond,
		IdleTimeout:        time.Hour,
		MinUptime:          time.Minute,
		Upstream:           upstream,
	})
	mgr.Add("photos.example.com", svc)
	return New(silentLogger(), mgr), svc, fc
}

func TestBrowserWaitingPagePreservesOriginalRequestURI(t *testing.T) {
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer health.Close()

	srv, _, fc := newTestServer(t, "http://127.0.0.1:2283", health.URL)
	req := httptest.NewRequest(http.MethodGet, "http://photos.example.com/albums/123?foo=bar", nil)
	req.Host = "photos.example.com"
	req.Header.Set("Accept", "text/html")
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)
	body := rr.Body.String()
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(body, "data-return-to=\"/albums/123?foo=bar\"") {
		t.Fatalf("waiting page should preserve original request URI; body=%q", body)
	}
	waitFor(t, func() bool { return fc.Starts() == 1 }, 250*time.Millisecond, "compose start was not triggered")
}

func TestAPIRequestWhileDownGetsRetryable503NotHTML(t *testing.T) {
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer health.Close()

	srv, _, fc := newTestServer(t, "http://127.0.0.1:2283", health.URL)
	req := httptest.NewRequest(http.MethodGet, "http://photos.example.com/api/server/ping", nil)
	req.Host = "photos.example.com"
	req.Header.Set("Accept", "application/json")
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)
	body := rr.Body.String()
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
	if got := rr.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After = %q, want 5", got)
	}
	if strings.Contains(strings.ToLower(body), "<html") {
		t.Fatalf("API response should not be HTML: %q", body)
	}
	if !strings.Contains(body, "service starting") {
		t.Fatalf("body should explain retryable startup: %q", body)
	}
	waitFor(t, func() bool { return fc.Starts() == 1 }, 250*time.Millisecond, "compose start was not triggered")
}

func TestStatusEndpointIsHostScopedAndMinimal(t *testing.T) {
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer health.Close()

	srv, _, _ := newTestServer(t, "http://127.0.0.1:2283", health.URL)
	req := httptest.NewRequest(http.MethodGet, "http://photos.example.com/__lazyloader/status", nil)
	req.Host = "photos.example.com"
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)
	body := rr.Body.String()
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(body, `"host":"photos.example.com"`) {
		t.Fatalf("status body missing host: %q", body)
	}
	if !strings.Contains(body, `"state":"down"`) {
		t.Fatalf("status body missing state: %q", body)
	}
	if strings.Contains(body, "upstream") || strings.Contains(body, "services") || strings.Contains(body, "last_activity") {
		t.Fatalf("status body should be minimal and host-scoped: %q", body)
	}
}

func TestProxyErrorForAPIRequestReturnsRetryable503(t *testing.T) {
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer health.Close()

	srv, svc, _ := newTestServer(t, "http://127.0.0.1:1", health.URL)
	svc.EnsureUp()
	waitFor(t, func() bool { return svc.IsProxyable() }, time.Second, "service never became proxyable")

	req := httptest.NewRequest(http.MethodGet, "http://photos.example.com/api/albums", nil)
	req.Host = "photos.example.com"
	req.Header.Set("Accept", "application/json")
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 after proxy error", rr.Code)
	}
	if strings.Contains(strings.ToLower(rr.Body.String()), "<html") {
		t.Fatalf("proxy error path for API request should not return HTML: %q", rr.Body.String())
	}
}
