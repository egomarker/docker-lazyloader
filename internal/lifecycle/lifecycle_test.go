package lifecycle

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type fakeCompose struct {
	mu     sync.Mutex
	starts int
	stops  int
}

func (f *fakeCompose) Start() error { f.mu.Lock(); f.starts++; f.mu.Unlock(); return nil }
func (f *fakeCompose) Stop() error  { f.mu.Lock(); f.stops++; f.mu.Unlock(); return nil }
func (f *fakeCompose) Starts() int  { f.mu.Lock(); defer f.mu.Unlock(); return f.starts }
func (f *fakeCompose) Stops() int   { f.mu.Lock(); defer f.mu.Unlock(); return f.stops }

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestService(t *testing.T, idle, min time.Duration, healthURL string) (*Service, *fakeCompose) {
	t.Helper()
	fc := &fakeCompose{}
	s := NewService(ServiceConfig{
		Logger:        silentLogger(),
		Compose:       fc,
		HealthURL:     healthURL,
		HealthTimeout: 2 * time.Second,
		StartTimeout:  5 * time.Second,
		IdleTimeout:   idle,
		MinUptime:     min,
		Upstream:      "http://127.0.0.1:2283",
	})
	return s, fc
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

func setStateUp(s *Service, upSince, lastActivity time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = StateUp
	s.upSince = upSince
	s.lastActivity = lastActivity
	s.inflight = 0
}

func TestMaybeIdle_StopsWhenIdle(t *testing.T) {
	s, fc := newTestService(t, time.Hour, time.Minute, "http://127.0.0.1:0/ping")
	setStateUp(s, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour))

	s.MaybeIdle()
	waitFor(t, func() bool { return s.State() == StateDown }, 2*time.Second, "service did not go down")
	if fc.Stops() != 1 {
		t.Errorf("stops = %d, want 1", fc.Stops())
	}
}

func TestMaybeIdle_SkipsWhenInflight(t *testing.T) {
	s, fc := newTestService(t, time.Hour, time.Minute, "http://127.0.0.1:0/ping")
	setStateUp(s, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour))
	s.mu.Lock()
	s.inflight = 1
	s.mu.Unlock()

	s.MaybeIdle()
	time.Sleep(150 * time.Millisecond)
	if fc.Stops() != 0 {
		t.Errorf("expected no stop with inflight>0, got %d", fc.Stops())
	}
}

func TestMaybeIdle_SkipsWhenMinUptimeNotReached(t *testing.T) {
	s, fc := newTestService(t, time.Hour, time.Hour, "http://127.0.0.1:0/ping")
	setStateUp(s, time.Now(), time.Now().Add(-2*time.Hour)) // just up, but idle long ago

	s.MaybeIdle()
	time.Sleep(150 * time.Millisecond)
	if fc.Stops() != 0 {
		t.Errorf("expected no stop before min_uptime, got %d", fc.Stops())
	}
}

func TestMaybeIdle_SkipsWhenNotUp(t *testing.T) {
	s, fc := newTestService(t, time.Hour, time.Minute, "http://127.0.0.1:0/ping")
	// default state is Down
	s.MaybeIdle()
	time.Sleep(150 * time.Millisecond)
	if fc.Stops() != 0 {
		t.Errorf("expected no stop when not Up, got %d", fc.Stops())
	}
}

func TestEnsureUp_BringsUpViaHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s, fc := newTestService(t, time.Hour, time.Minute, srv.URL)

	s.EnsureUp()
	waitFor(t, func() bool { return s.State() == StateUp }, 5*time.Second, "service did not come up")
	if fc.Starts() != 1 {
		t.Errorf("starts = %d, want 1", fc.Starts())
	}
	if !s.IsProxyable() {
		t.Errorf("IsProxyable = false, want true")
	}
}

func TestEnsureUp_IdempotentWhenUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s, fc := newTestService(t, time.Hour, time.Minute, srv.URL)
	s.EnsureUp()
	waitFor(t, func() bool { return s.State() == StateUp }, 5*time.Second, "service did not come up")

	s.EnsureUp() // already up
	s.EnsureUp()
	time.Sleep(150 * time.Millisecond)
	if fc.Starts() != 1 {
		t.Errorf("starts = %d, want 1 (EnsureUp should not double-start)", fc.Starts())
	}
}

func TestProbe_RequiresExpectedStatusAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"res":"pong"}`))
	}))
	defer srv.Close()

	s, _ := newTestService(t, time.Hour, time.Minute, srv.URL)
	s.healthExpectBody = "pong"
	ok, err := s.probe(context.Background())
	if err != nil {
		t.Fatalf("probe err: %v", err)
	}
	if !ok {
		t.Fatal("probe should succeed when status/body match")
	}

	s.healthExpectBody = "not-there"
	ok, err = s.probe(context.Background())
	if err != nil {
		t.Fatalf("probe err: %v", err)
	}
	if ok {
		t.Fatal("probe should fail when expected body substring is missing")
	}
}

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"Photos.Example.COM":    "photos.example.com",
		"photos.example.com":    "photos.example.com",
		"photos.example.com:80": "photos.example.com",
		"  ":                    "",
	}
	for in, want := range cases {
		if got := NormalizeHost(in); got != want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestManagerMatch(t *testing.T) {
	m := NewManager(silentLogger())
	s, _ := newTestService(t, time.Hour, time.Minute, "http://127.0.0.1:0/ping")
	m.Add("Photos.Example.com", s)
	if m.Match("photos.example.com") != s {
		t.Error("Match failed to find normalized host")
	}
	if m.Match("other.example.com") != nil {
		t.Error("Match should return nil for unknown host")
	}
}
