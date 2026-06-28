// Package lifecycle holds the per-service state machine, the host→service
// manager, the idle watcher, and the SSE subscription fan-out.
package lifecycle

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// State is the lifecycle state of a managed service.
type State int

const (
	StateDown State = iota
	StateStarting
	StateUp
	StateStopping
)

func (s State) String() string {
	switch s {
	case StateDown:
		return "down"
	case StateStarting:
		return "starting"
	case StateUp:
		return "up"
	case StateStopping:
		return "stopping"
	default:
		return "unknown"
	}
}

// MarshalJSON renders State as its lowercase string name.
func (s State) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// ComposeController abstracts start/stop so the state machine is testable.
type ComposeController interface {
	Start() error
	Stop() error
}

// Service is one managed host's lifecycle.
type Service struct {
	log           *slog.Logger
	compose            ComposeController
	healthURL          string
	healthExpectStatus int
	healthExpectBody   string
	healthTimeout      time.Duration
	startTimeout       time.Duration
	idleTimeout        time.Duration
	minUptime          time.Duration
	upstream           string

	mu           sync.RWMutex
	state        State
	lastActivity time.Time
	upSince      time.Time
	inflight     int
	lastDetail   string
	startQueued  bool

	// opMu serializes all start/stop transitions so they can never overlap.
	opMu sync.Mutex

	subsMu sync.Mutex
	subs   map[chan Event]struct{}
}

// Event is a lifecycle snapshot, also streamed over SSE.
type Event struct {
	State        State     `json:"state"`
	UpSince      time.Time `json:"up_since,omitempty"`
	LastActivity time.Time `json:"last_activity"`
	Detail       string    `json:"detail,omitempty"`
	Inflight     int       `json:"inflight"`
}

// ServiceConfig configures a Service.
type ServiceConfig struct {
	Logger             *slog.Logger
	Compose            ComposeController
	HealthURL          string
	HealthExpectStatus int
	HealthExpectBody   string
	HealthTimeout      time.Duration
	StartTimeout       time.Duration
	IdleTimeout        time.Duration
	MinUptime          time.Duration
	Upstream           string
}

// NewService constructs a Service.
func NewService(c ServiceConfig) *Service {
	healthExpectStatus := c.HealthExpectStatus
	if healthExpectStatus == 0 {
		healthExpectStatus = http.StatusOK
	}
	return &Service{
		log:                c.Logger,
		compose:            c.Compose,
		healthURL:          c.HealthURL,
		healthExpectStatus: healthExpectStatus,
		healthExpectBody:   c.HealthExpectBody,
		healthTimeout:      c.HealthTimeout,
		startTimeout:       c.StartTimeout,
		idleTimeout:        c.IdleTimeout,
		minUptime:          c.MinUptime,
		upstream:           c.Upstream,
		subs:               make(map[chan Event]struct{}),
		lastActivity:       time.Now(),
	}
}

// Upstream returns the proxy target URL.
func (s *Service) Upstream() string { return s.upstream }

// IdleTimeout returns the configured idle timeout.
func (s *Service) IdleTimeout() time.Duration { return s.idleTimeout }

// MinUptime returns the configured minimum uptime before idle shutdown is allowed.
func (s *Service) MinUptime() time.Duration { return s.minUptime }

// State returns the current state under a read lock.
func (s *Service) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// Snapshot returns the current Event view.
func (s *Service) Snapshot() Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Event{
		State:        s.state,
		UpSince:      s.upSince,
		LastActivity: s.lastActivity,
		Detail:       s.lastDetail,
		Inflight:     s.inflight,
	}
}

// IsProxyable reports whether requests may be proxied right now.
func (s *Service) IsProxyable() bool {
	return s.State() == StateUp
}

// setState flips the state under the lock and broadcasts a snapshot.
// Use the manual set+broadcast in bringUp for the Up transition so upSince /
// lastActivity are set atomically with it.
func (s *Service) setState(st State, detail string) {
	s.mu.Lock()
	s.state = st
	s.lastDetail = detail
	s.mu.Unlock()
	s.broadcast(s.Snapshot())
}

// Touch bumps lastActivity to now (a request was observed).
func (s *Service) Touch() {
	s.mu.Lock()
	s.lastActivity = time.Now()
	s.mu.Unlock()
}

// IncInflight / DecInflight track live proxied requests to guard idle shutdown.
func (s *Service) IncInflight() {
	s.mu.Lock()
	s.inflight++
	s.mu.Unlock()
}

// DecInflight decrements the in-flight counter (floor at 0).
func (s *Service) DecInflight() {
	s.mu.Lock()
	if s.inflight > 0 {
		s.inflight--
	}
	s.mu.Unlock()
}

// EnsureUp brings the service up if it is currently down. Non-blocking; safe
// to call on every request.
func (s *Service) EnsureUp() {
	s.mu.Lock()
	if s.state == StateUp || s.state == StateStarting || s.startQueued {
		s.mu.Unlock()
		return
	}
	s.startQueued = true
	s.mu.Unlock()
	go s.bringUp()
}

func (s *Service) bringUp() {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	defer func() {
		s.mu.Lock()
		s.startQueued = false
		s.mu.Unlock()
	}()

	// Re-check after acquiring the lock; another goroutine may have raced ahead.
	if st := s.State(); st == StateUp {
		return
	}

	s.setState(StateStarting, "starting containers")
	s.log.Info("starting service")

	if err := s.compose.Start(); err != nil {
		s.log.Error("compose start failed", "err", err)
		s.setState(StateDown, "start failed")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.startTimeout)
	defer cancel()
	if !s.waitForHealth(ctx) {
		s.log.Error("health check timed out before service became ready")
		s.setState(StateDown, "health timeout")
		return
	}

	s.mu.Lock()
	s.state = StateUp
	s.upSince = time.Now()
	s.lastActivity = time.Now()
	s.lastDetail = "healthy"
	s.mu.Unlock()
	s.broadcast(s.Snapshot())
	s.log.Info("service up")
}

func (s *Service) waitForHealth(ctx context.Context) bool {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	// Immediate first probe, then on each tick.
	if ok, _ := s.probe(ctx); ok {
		return true
	}
	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			if ok, _ := s.probe(ctx); ok {
				return true
			}
		}
	}
}

func (s *Service) probe(ctx context.Context) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, s.healthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, s.healthURL, nil)
	if err != nil {
		return false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != s.healthExpectStatus {
		return false, nil
	}
	if s.healthExpectBody == "" {
		return true, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return false, err
	}
	return strings.Contains(string(body), s.healthExpectBody), nil
}

// Reconcile probes health on boot and sets the initial state without a noisy
// broadcast (no subscribers yet anyway).
func (s *Service) Reconcile() {
	ctx, cancel := context.WithTimeout(context.Background(), s.healthTimeout)
	defer cancel()
	if ok, _ := s.probe(ctx); ok {
		s.mu.Lock()
		s.state = StateUp
		s.upSince = time.Now()
		s.lastActivity = time.Now()
		s.lastDetail = "healthy"
		s.mu.Unlock()
		s.log.Info("reconciled: already up")
		return
	}
	s.mu.Lock()
	s.state = StateDown
	s.lastDetail = "down"
	s.mu.Unlock()
	s.log.Info("reconciled: down")
}

// MaybeIdle stops the service if all idle conditions are met.
func (s *Service) MaybeIdle() {
	s.mu.RLock()
	shouldStop := s.state == StateUp &&
		s.inflight == 0 &&
		time.Since(s.upSince) >= s.minUptime &&
		time.Since(s.lastActivity) >= s.idleTimeout
	s.mu.RUnlock()
	if shouldStop {
		go s.shutdown("idle timeout")
	}
}

func (s *Service) shutdown(reason string) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.State() != StateUp {
		return
	}
	s.setState(StateStopping, reason)
	s.log.Info("stopping service", "reason", reason)
	if err := s.compose.Stop(); err != nil {
		s.log.Error("compose stop failed", "err", err)
		ctx, cancel := context.WithTimeout(context.Background(), s.healthTimeout)
		defer cancel()
		if ok, _ := s.probe(ctx); ok {
			s.mu.Lock()
			s.state = StateUp
			s.lastDetail = "stop failed"
			s.mu.Unlock()
			s.broadcast(s.Snapshot())
			return
		}
		s.mu.Lock()
		s.state = StateDown
		s.lastDetail = "stop failed; service no longer healthy"
		s.mu.Unlock()
		s.broadcast(s.Snapshot())
		return
	}
	s.mu.Lock()
	s.state = StateDown
	s.lastDetail = "stopped"
	s.mu.Unlock()
	s.broadcast(s.Snapshot())
	s.log.Info("service stopped")
}

// MarkDown is invoked when the proxy sees the upstream is unreachable, so a
// crash/manual stop is detected and the next request triggers a clean restart.
func (s *Service) MarkDown(reason string) {
	s.mu.Lock()
	wasUp := s.state == StateUp
	if wasUp {
		s.state = StateDown
		s.lastDetail = reason
	}
	s.mu.Unlock()
	if wasUp {
		s.broadcast(s.Snapshot())
		s.log.Warn("service marked down", "reason", reason)
	}
}

// --- SSE subscriptions ---

// Subscribe returns a buffered channel of Events and an unsubscribe func.
func (s *Service) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 8)
	s.subsMu.Lock()
	s.subs[ch] = struct{}{}
	s.subsMu.Unlock()
	return ch, func() {
		s.subsMu.Lock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
		s.subsMu.Unlock()
	}
}

func (s *Service) broadcast(e Event) {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- e:
		default: // drop if subscriber is slow; the next event carries fresh state
		}
	}
}

func (e Event) withDetail(d string) Event { e.Detail = d; return e }

// --- Manager ---

// Manager maps normalized Host headers to Services.
type Manager struct {
	log      *slog.Logger
	services map[string]*Service
}

// NewManager constructs an empty Manager.
func NewManager(log *slog.Logger) *Manager {
	return &Manager{log: log, services: map[string]*Service{}}
}

// NormalizeHost trims whitespace, lowercases, and strips the :port suffix.
func NormalizeHost(h string) string {
	h = strings.TrimSpace(strings.ToLower(h))
	if i := strings.LastIndex(h, ":"); i >= 0 {
		// Note: fine for hostnames and IPv4; Host headers here are DNS names.
		h = h[:i]
	}
	return h
}

// Add registers a service under a host.
func (m *Manager) Add(host string, svc *Service) {
	m.services[NormalizeHost(host)] = svc
}

// Match returns the service for a Host header, or nil.
func (m *Manager) Match(host string) *Service {
	return m.services[NormalizeHost(host)]
}

// All returns the full host→service map.
func (m *Manager) All() map[string]*Service { return m.services }

// Reconcile probes every service once at startup.
func (m *Manager) Reconcile() {
	for _, svc := range m.services {
		svc.Reconcile()
	}
}

// IdleLoop runs MaybeIdle on every service at the given interval until ctx is done.
func (m *Manager) IdleLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, svc := range m.services {
				svc.MaybeIdle()
			}
		}
	}
}
