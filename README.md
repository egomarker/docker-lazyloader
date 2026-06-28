# docker-lazyloader

A tiny Go service that keeps rarely-used, self-hosted web apps **stopped** until
they are accessed, then transparently starts them, shows a live "starting
service" page, reverse-proxies traffic, and **shuts them down again after a
period of inactivity**.

Built to free up RAM/CPU from things like Immich that run 24/7 but get used
once a week. Runs as a native macOS binary behind Nginx Proxy Manager.

See [`SPEC.md`](./SPEC.md) for the full design.

## How it works

```
internet → NPM (TLS) → lazyloader 127.0.0.1:8787 (cleartext, localhost)
                            ├─ Host header → pick service
                            ├─ Up   → reverse-proxy, track activity + in-flight
                            └─ Down → docker compose start
                                      ├─ browser nav GET → SSE "waking up" page
                                      └─ API/upload/media → 503 Retry-After
idle ticker → no traffic for idle_timeout && inflight==0 && uptime>=min_uptime
              → docker compose stop
```

## Build

```sh
# From the repo root (cross-compiles to macOS arm64 by default).
make build            # -> ./lazyloader
make build GOOS=darwin GOARCH=amd64   # Intel Mac
```

Or, on the Mac:

```sh
go install github.com/egomarker/docker-lazyloader/cmd/lazyloader@latest
# binary lands in $(go env GOPATH)/bin -> copy to ~/bin/lazyloader
```

## One-time host setup

1. **Config** — copy and edit:

   ```sh
   mkdir -p ~/.config/lazyloader
   cp examples/lazyloader.example.yaml ~/.config/lazyloader/lazyloader.yaml
   ```

2. **Immich compose** — change the four immich services
   (`immich-server`, `immich-machine-learning`, `redis`, `database`) from
   `restart: always` to `restart: unless-stopped`, so a stopped stack stays
   stopped across Docker/daemon restarts instead of auto-resuming.

3. **NPM** — repoint the `photos.example.com` proxy host upstream from
   `example.com:2283` to `host.docker.internal:8787`.
   - Docker Desktop / OrbStack resolve `host.docker.internal` by default.
   - On a custom NPM network, add to its compose service:
     `extra_hosts: ["host.docker.internal:host-gateway"]`.

4. **launchd** —

   ```sh
   cp deploy/com.egomarker.lazyloader.plist ~/Library/LaunchAgents/
   # edit paths inside if needed, then:
   launchctl load -w ~/Library/LaunchAgents/com.egomarker.lazyloader.plist
   ```

## Verify

```sh
# With immich stopped:
curl -H 'Host: photos.example.com' http://127.0.0.1:8787/__lazyloader/status
curl -H 'Host: photos.example.com' http://127.0.0.1:8787/   # -> waiting page for browser-style GETs
curl -H 'Host: photos.example.com' -H 'Accept: application/json' http://127.0.0.1:8787/api/server/ping
# -> 503 Retry-After for API-style requests

# After idle_timeout, immich is stopped again:
docker compose -f /path/to/immich-app/docker-compose.yml ps
```

## Reserved paths (never proxied)

- `GET /__lazyloader/status` — minimal JSON state for the matched host only, including seconds since last activity
- `GET /__lazyloader/events` — SSE stream of state changes for the matched host
- `GET /__lazyloader/waiting` — the waiting page HTML

## Layout

```
cmd/lazyloader/main.go          entrypoint (config load, server, signal handling)
internal/config/                YAML load + defaults + validation
internal/docker/                `docker compose` stop/start wrapper
internal/lifecycle/             state machine, manager, idle watcher, SSE fan-out
internal/server/                host router, reverse proxy, waiting page, retry/503 path, status/SSE
examples/                       example config
deploy/                         launchd plist
```

## Notes / risks

- Uses `docker compose stop`/`start` (not `down`/`up`) so containers are
  preserved and a warm start is fast. If you manually `docker compose down`
  (removing containers), the next auto-start logs an error until you `up -d`
  once manually.
- Browser GETs get a waiting page during cold start; non-browser requests get a
  retryable `503 Service Unavailable` with `Retry-After: 5` instead of HTML.
- Health is strict by default: expect HTTP 200, and optionally match a response
  body substring (for Immich, `pong`). This is still a liveness check, not full
  DB-readiness; the first real request may briefly see app-level loading.
- In-container background jobs (e.g. Immich ML) don't generate HTTP traffic, so
  a generous `idle_timeout` (default 1h) avoids interrupting them.
