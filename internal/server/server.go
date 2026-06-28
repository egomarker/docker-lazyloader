// Package server implements the HTTP layer: host routing, reverse proxy,
// the SSE "starting service" waiting page, and a JSON status endpoint.
package server

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	"github.com/egomarker/docker-lazyloader/internal/lifecycle"
)

// ReservedPathPrefix is claimed by lazyloader and never proxied upstream.
const ReservedPathPrefix = "/__lazyloader/"

// Server is the HTTP front end.
type Server struct {
	log     *slog.Logger
	mgr     *lifecycle.Manager
	mux     *http.ServeMux
	proxies sync.Map // host -> *httputil.ReverseProxy
}

// New builds the HTTP server and pre-compiles a reverse proxy per service.
func New(log *slog.Logger, mgr *lifecycle.Manager) *Server {
	s := &Server{log: log, mgr: mgr}
	for host, svc := range mgr.All() {
		s.proxies.Store(host, s.buildProxy(svc))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/__lazyloader/status", s.handleStatusJSON)
	mux.HandleFunc("/__lazyloader/events", s.handleEvents)
	mux.HandleFunc("/__lazyloader/waiting", s.handleWaiting)
	mux.HandleFunc("/", s.handleRoot)
	s.mux = mux
	return s
}

func (s *Server) buildProxy(svc *lifecycle.Service) *httputil.ReverseProxy {
	target, err := url.Parse(svc.Upstream())
	if err != nil {
		s.log.Error("invalid upstream URL, proxy disabled", "upstream", svc.Upstream(), "err", err)
		return nil
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	// Preserve the original Host header so the upstream app generates correct
	// self-referential URLs (e.g. photos.example.com). NewSingleHostReverseProxy
	// only rewrites URL.Scheme/Host, leaving req.Host intact by default.
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		if req.Header.Get("X-Forwarded-Proto") == "" {
			req.Header.Set("X-Forwarded-Proto", "http")
		}
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		s.log.Warn("upstream proxy error", "err", err)
		svc.MarkDown("upstream unreachable")
		svc.EnsureUp()
		if wantsHTMLWaitingPage(r) {
			s.serveWaiting(w, r, svc)
			return
		}
		s.serveStartingRetry(w, r, svc)
	}
	return proxy
}

func (s *Server) getProxy(host string) *httputil.ReverseProxy {
	if v, ok := s.proxies.Load(host); ok {
		return v.(*httputil.ReverseProxy)
	}
	return nil
}

// ServeHTTP routes reserved paths first, then everything else.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	svc := s.mgr.Match(r.Host)
	if svc == nil {
		http.Error(w, "no service configured for host "+r.Host, http.StatusNotFound)
		return
	}
	if svc.IsProxyable() {
		s.proxy(svc, w, r)
		return
	}
	// Not up: trigger start (idempotent). Browser navigations get a waiting page;
	// API/media/upload calls get a retryable 503 rather than unexpected HTML.
	svc.EnsureUp()
	if wantsHTMLWaitingPage(r) {
		s.serveWaiting(w, r, svc)
		return
	}
	s.serveStartingRetry(w, r, svc)
}

func (s *Server) proxy(svc *lifecycle.Service, w http.ResponseWriter, r *http.Request) {
	p := s.getProxy(lifecycle.NormalizeHost(r.Host))
	if p == nil {
		http.Error(w, "proxy not available", http.StatusBadGateway)
		return
	}
	svc.IncInflight()
	defer svc.DecInflight()
	svc.Touch()
	p.ServeHTTP(w, r)
}

func wantsHTMLWaitingPage(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Dest"), "document") || strings.EqualFold(r.Header.Get("Sec-Fetch-Mode"), "navigate") {
		return true
	}
	accept := strings.ToLower(r.Header.Get("Accept"))
	return strings.Contains(accept, "text/html") || strings.Contains(accept, "application/xhtml+xml")
}

func retryAfterSeconds() string { return "5" }

func (s *Server) serveStartingRetry(w http.ResponseWriter, r *http.Request, svc *lifecycle.Service) {
	svc.Touch()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", retryAfterSeconds())
	if strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/json") || strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":               "service starting",
			"retry_after_seconds": 5,
		})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("service starting; retry shortly\n"))
}

// --- status JSON ---

type statusResponse struct {
	Host   string `json:"host"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

func (s *Server) handleStatusJSON(w http.ResponseWriter, r *http.Request) {
	svc := s.mgr.Match(r.Host)
	if svc == nil {
		http.Error(w, "no service for host", http.StatusNotFound)
		return
	}
	snap := svc.Snapshot()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(statusResponse{
		Host:   lifecycle.NormalizeHost(r.Host),
		State:  snap.State.String(),
		Detail: snap.Detail,
	})
}

// --- SSE events ---

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	svc := s.mgr.Match(r.Host)
	if svc == nil {
		http.Error(w, "no service for host", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering (NPM)

	writeEvent := func(e lifecycle.Event) {
		b, _ := json.Marshal(e)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	// Send current snapshot immediately.
	writeEvent(svc.Snapshot())

	ch, unsub := svc.Subscribe()
	defer unsub()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			writeEvent(e)
			if e.State == lifecycle.StateUp || e.State == lifecycle.StateDown {
				return
			}
		}
	}
}

// --- waiting page ---

var waitingTmpl = template.Must(template.New("waiting").Parse(waitingHTML))

func (s *Server) serveWaiting(w http.ResponseWriter, r *http.Request, svc *lifecycle.Service) {
	svc.Touch() // waiting is activity; keeps the start intent
	returnTo := r.URL.RequestURI()
	if strings.HasPrefix(r.URL.Path, ReservedPathPrefix) {
		returnTo = r.URL.Query().Get("return_to")
		if returnTo == "" {
			returnTo = "/"
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = waitingTmpl.Execute(w, map[string]string{
		"Host":     r.Host,
		"Detail":   svc.Snapshot().Detail,
		"ReturnTo": returnTo,
	})
}

func (s *Server) handleWaiting(w http.ResponseWriter, r *http.Request) {
	svc := s.mgr.Match(r.Host)
	if svc == nil {
		http.Error(w, "no service for host", http.StatusNotFound)
		return
	}
	svc.EnsureUp()
	s.serveWaiting(w, r, svc)
}

const waitingHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Starting {{.Host}}</title>
<style>
  :root { color-scheme: light dark; }
  * { box-sizing: border-box; }
  body {
    margin: 0; min-height: 100vh; display: flex; align-items: center; justify-content: center;
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
    background: #0f1115; color: #e6e6e6; text-align: center; padding: 2rem;
  }
  .card { max-width: 420px; }
  .spinner {
    width: 48px; height: 48px; margin: 0 auto 1.5rem;
    border: 4px solid rgba(255,255,255,0.12); border-top-color: #4f8cff;
    border-radius: 50%; animation: spin 0.9s linear infinite;
  }
  @keyframes spin { to { transform: rotate(360deg); } }
  h1 { font-size: 1.25rem; font-weight: 600; margin: 0 0 0.5rem; }
  .host { color: #8b9bb4; font-size: 0.95rem; margin-bottom: 1.25rem; }
  .state {
    display: inline-block; padding: 0.3rem 0.75rem; border-radius: 999px;
    background: rgba(79,140,255,0.15); color: #7fb0ff; font-size: 0.85rem;
    border: 1px solid rgba(79,140,255,0.3); text-transform: capitalize;
  }
  .err {
    margin-top: 1.25rem; color: #ff8b8b; font-size: 0.9rem; min-height: 1.2em;
  }
  .hint { margin-top: 1.5rem; color: #6b7689; font-size: 0.8rem; }
</style>
</head>
<body data-return-to="{{.ReturnTo}}">
  <div class="card">
    <div class="spinner" id="spin"></div>
    <h1>Waking up service</h1>
    <div class="host">{{.Host}}</div>
    <div class="state" id="state">starting</div>
    <div class="err" id="err"></div>
    <div class="hint">This window stays open while containers start, then redirects automatically.</div>
  </div>
<script>
(function () {
  var state = document.getElementById('state');
  var err = document.getElementById('err');
  var startedFallback;
  var returnTo = document.body.dataset.returnTo || "/";

  function redirect() { window.location.replace(returnTo); }

  function pollFallback() {
    fetch("/__lazyloader/status", { cache: "no-store" })
      .then(function (r) { return r.json(); })
      .then(function (data) {
        if (data && data.state === "up") { redirect(); }
      })
      .catch(function () {});
  }

  function connectSSE() {
    var es = new EventSource("/__lazyloader/events");
    es.onmessage = function (ev) {
      var e;
      try { e = JSON.parse(ev.data); } catch (_) { return; }
      if (e.state) state.textContent = e.state;
      if (e.state === "up") { err.textContent = ""; es.close(); redirect(); return; }
      if (e.state === "down") {
        err.textContent = e.detail || "Service could not start. Retrying on next access\u2026";
        return;
      }
      err.textContent = "";
    };
    es.onerror = function () {
      es.close();
      // Fall back to polling if SSE breaks.
      if (!startedFallback) { startedFallback = setInterval(pollFallback, 3000); }
    };
  }

  connectSSE();
  // Belt-and-braces: poll too, in case the SSE redirect never fires.
  startedFallback = setInterval(pollFallback, 5000);
})();
</script>
</body>
</html>`
