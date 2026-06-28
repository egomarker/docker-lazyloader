# docker-lazyloader — Specification

A small Go service that keeps rarely-used, self-hosted web apps stopped until
they are accessed, then transparently starts them, serves a "starting" page, and
shuts them down again after a period of inactivity.

## Problem

Immich (and similar apps) run 24/7 but are accessed rarely, yet they hold RAM
and CPU constantly. Piclaw and Immich can both be fronted by
Nginx Proxy Manager (NPM) on separate subdomains such as `app.example.com` and `photos.example.com`.

## Goals

- On first request to a managed host, start the target docker-compose stack.
- For browser navigation GETs during cold start, show a live "starting service"
  page and redirect back to the original path/query once healthy.
- For non-browser/API/media/upload requests during cold start, return a
  retryable `503 Service Unavailable` with `Retry-After` instead of HTML.
- After configurable idle time with zero in-flight requests, stop the stack.
- Do **not** touch piclaw (it has background automation that must stay running).
- Be a single native macOS binary installed to `~/bin`, started by `launchd`.

## Non-goals (v1)

- Managing piclaw.
- TLS termination (NPM already does this).
- Multi-user auth / access control.
- Persisting activity state across restarts.
- A web admin UI.
- Prometheus metrics.

## Architecture

```
internet → NPM (TLS, :443) → lazyloader 127.0.0.1:8787 (cleartext, localhost)
                                 ├─ read Host header → pick service
                                 ├─ service Up   → reverse-proxy to upstream,
                                 │                  bump lastActivity, inc/dec inflight
                                 └─ service Down → docker compose start (once)
                                                   ├─ browser GET  → SSE waiting page
                                                   │                → redirect to original URI
                                                   └─ API/upload   → 503 Retry-After
background ticker → if Up && inflight==0
                    && uptime >= min_uptime
                    && idle >= idle_timeout → docker compose stop
```

Lazyloader is **localhost-only**. NPM points the managed subdomain upstream at
`host.docker.internal:8787`.

## Lifecycle state machine

```
Down ──start→ Starting ──health ok→ Up ──idle→ Stopping ──→ Down
                                       │
                                       └──proxy error→ Down (auto-restart on next req)
```

- `Down` / `Starting` / `Stopping` → requests trigger `EnsureUp` + waiting page.
- `Up` → requests are proxied.
- A single `opMu` mutex serializes all start/stop transitions so a start and a
  stop can never overlap.
- In-flight counter and `min_uptime` anti-flap guard the idle shutdown.

## Managed target (locked)

- **Host:** `photos.example.com`
- **Compose project:** `immich` (whole-stack `stop` / `start`)
- **Upstream:** `http://127.0.0.1:2283`
- **Health:** `http://127.0.0.1:2283/api/server/ping` → require HTTP `200`, optionally body contains `pong`
- **Idle timeout:** 1h (configurable)
- **Min uptime:** 2m (anti-flap)

## Deployment (locked)

| Item | Value |
|---|---|
| Language | Go (stdlib + `gopkg.in/yaml.v3`) |
| Runs as | Native macOS binary via `launchd` |
| Binary path | `~/bin/lazyloader` |
| Config path | `~/.config/lazyloader/lazyloader.yaml` |
| Listen | `127.0.0.1:8787` |
| Distribution | `go install github.com/egomarker/docker-lazyloader/cmd/lazyloader@latest` (or Homebrew tap later) |

## One-time host setup (outside the app)

1. **NPM:** repoint `photos.example.com` upstream from `example.com:2283` to
   `host.docker.internal:8787`. Verify NPM's container can resolve
   `host.docker.internal` (Docker Desktop: default; OrbStack: default; custom
   networks: add `extra_hosts: ["host.docker.internal:host-gateway"]`).
2. **Immich compose:** change all four immich services (`immich-server`,
   `immich-machine-learning`, `redis`, `database`) from `restart: always` to
   `restart: unless-stopped`, so a manually-stopped stack stays stopped across
   Docker/daemon restarts instead of auto-resuming.
3. **launchd:** load `deploy/com.egomarker.lazyloader.plist` (edit paths).

## Configuration

See `examples/lazyloader.example.yaml`. Keys:

- `listen`, `docker_bin`, `poll_interval`, `log_level`
- per-service: `host`, `compose_dir`, `compose_project` (optional), `upstream`,
  `health`, `health_expect_status`, `health_expect_body`, `idle_timeout`,
  `min_uptime`, `health_timeout`, `start_timeout`

## Reserved paths (always handled by lazyloader, never proxied)

- `GET /__lazyloader/status` — minimal JSON state for the matched host only, including seconds since last activity
- `GET /__lazyloader/events` — SSE stream of state changes for the matched host
- `GET /__lazyloader/waiting` — the waiting page HTML

## Known risks / future work

- **Background ML jobs:** Immich runs in-container jobs that never touch HTTP, so
  idle timer can't see them. Mitigated by generous `idle_timeout`; future option
  is to query Immich's job API before shutting down.
- **`start` requires existing containers:** we use `compose start` (not `up`),
  so if a user manually runs `docker compose down` (removing containers) the
  next auto-start will fail and log an error. Manual `up -d` once recovers.
  (Acceptable for v1; `up -d` fallback is a future hardening.)
- **Health probe is liveness, not DB-readiness:** `/api/server/ping` returning
  HTTP 200 + `pong` is still not full DB-readiness. The first real request may
  briefly see app-level loading states.

## Test plan

- Unit: config parse + defaults/validation; state transitions; idle decision
  logic (`ShouldIdle` truth table).
- Manual:
  1. With stack stopped, browser GET to `https://photos.example.com/…` → waiting
     page; stream shows `starting → up`; redirect lands on the original path.
  2. With stack stopped, API GET to `https://photos.example.com/api/...` →
     `503 Service Unavailable` with `Retry-After: 5` and no HTML body.
  3. No traffic for `idle_timeout` → `docker compose ps` shows stopped; RAM freed.
  4. Request during `Stopping` → receives waiting page or retryable `503`, then
     starts cleanly once the stop completes.
  5. Reboot with `unless-stopped`: stack does **not** auto-start; first request
     brings it up.
  6. `GET /__lazyloader/status` returns the matched host's minimal state, including `seconds_since_last_activity`.

## Definition of done

- Binary builds with `go build ./...`, `go vet ./...` clean.
- `go install` produces a working `~/bin/lazyloader`.
- launchd keeps it alive; manual test plan passes.
- Immich frees resources when idle; returns within `start_timeout` on access.
