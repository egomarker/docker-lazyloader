package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaults(t *testing.T) {
	p := writeFile(t, "c.yaml", `
services:
  - host: photos.example.com
    compose_dir: /srv/immich
    upstream: http://127.0.0.1:2283
    health: http://127.0.0.1:2283/api/server/ping
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:8787" {
		t.Errorf("Listen default = %q, want 127.0.0.1:8787", cfg.Listen)
	}
	if cfg.DockerBin != "docker" {
		t.Errorf("DockerBin default = %q", cfg.DockerBin)
	}
	if cfg.PollInterval.Std() != 5*time.Second {
		t.Errorf("PollInterval default = %v", cfg.PollInterval)
	}
	s := cfg.Services[0]
	if s.HealthExpectStatus != 200 {
		t.Errorf("HealthExpectStatus default = %d, want 200", s.HealthExpectStatus)
	}
	if s.IdleTimeout.Std() != time.Hour {
		t.Errorf("IdleTimeout default = %v", s.IdleTimeout)
	}
	if s.MinUptime.Std() != 2*time.Minute {
		t.Errorf("MinUptime default = %v", s.MinUptime)
	}
	if s.StartTimeout.Std() != 3*time.Minute {
		t.Errorf("StartTimeout default = %v", s.StartTimeout)
	}
}

func TestLoadValidation(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"no services", "", "no services defined"},
		{"missing compose_dir", `
services:
  - host: a.example.com
    upstream: http://127.0.0.1:1
    health: http://127.0.0.1:1/h
`, "compose_dir is required"},
		{"missing upstream", `
services:
  - host: a.example.com
    compose_dir: /srv
    health: http://127.0.0.1:1/h
`, "upstream is required"},
		{"missing health", `
services:
  - host: a.example.com
    compose_dir: /srv
    upstream: http://127.0.0.1:1
`, "health is required"},
		{"bad health status", `
services:
  - host: a.example.com
    compose_dir: /srv
    upstream: http://127.0.0.1:1
    health: http://127.0.0.1:1/h
    health_expect_status: 42
`, "health_expect_status"},
		{"dup host", `
services:
  - host: a.example.com
    compose_dir: /srv
    upstream: http://127.0.0.1:1
    health: http://127.0.0.1:1/h
  - host: a.example.com
    compose_dir: /srv2
    upstream: http://127.0.0.1:2
    health: http://127.0.0.1:2/h
`, "duplicate host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeFile(t, "c.yaml", tc.yaml)
			_, err := Load(p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestLoadExplicitValues(t *testing.T) {
	p := writeFile(t, "c.yaml", `
listen: 0.0.0.0:9999
docker_bin: /usr/local/bin/docker
poll_interval: 90s
log_level: debug
services:
  - host: photos.example.com
    compose_dir: /srv/immich
    compose_project: immich
    upstream: http://127.0.0.1:2283
    health: http://127.0.0.1:2283/api/server/ping
    health_expect_status: 200
    health_expect_body: pong
    idle_timeout: 30m
    min_uptime: 10s
    health_timeout: 5s
    start_timeout: 1m
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "0.0.0.0:9999" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if cfg.PollInterval.Std() != 90*time.Second {
		t.Errorf("PollInterval = %v", cfg.PollInterval)
	}
	s := cfg.Services[0]
	if s.ComposeProject != "immich" {
		t.Errorf("ComposeProject = %q", s.ComposeProject)
	}
	if s.HealthExpectStatus != 200 {
		t.Errorf("HealthExpectStatus = %d", s.HealthExpectStatus)
	}
	if s.HealthExpectBody != "pong" {
		t.Errorf("HealthExpectBody = %q", s.HealthExpectBody)
	}
	if s.MinUptime.Std() != 10*time.Second {
		t.Errorf("MinUptime = %v", s.MinUptime)
	}
}
