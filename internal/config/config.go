// Package config loads and validates the lazyloader YAML configuration.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration.
type Config struct {
	Listen       string    `yaml:"listen"`
	DockerBin    string    `yaml:"docker_bin"`
	PollInterval Duration  `yaml:"poll_interval"`
	LogLevel     string    `yaml:"log_level"`
	Services     []Service `yaml:"services"`
}

// Service describes one managed host and its compose stack.
type Service struct {
	Host               string   `yaml:"host"`
	ComposeDir         string   `yaml:"compose_dir"`
	ComposeProject     string   `yaml:"compose_project"`
	Upstream           string   `yaml:"upstream"`
	Health             string   `yaml:"health"`
	HealthExpectStatus int      `yaml:"health_expect_status"`
	HealthExpectBody   string   `yaml:"health_expect_body"`
	IdleTimeout        Duration `yaml:"idle_timeout"`
	MinUptime          Duration `yaml:"min_uptime"`
	HealthTimeout      Duration `yaml:"health_timeout"`
	StartTimeout       Duration `yaml:"start_timeout"`
}

// Duration is a yaml-decodable time.Duration (e.g. "1h", "90s", "2m").
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Std returns the value as a standard library time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Load reads, parses, defaults and validates the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8787"
	}
	if c.DockerBin == "" {
		c.DockerBin = "docker"
	}
	if c.PollInterval == 0 {
		c.PollInterval = Duration(5 * time.Second)
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	for i := range c.Services {
		s := &c.Services[i]
		if s.HealthExpectStatus == 0 {
			s.HealthExpectStatus = 200
		}
		if s.IdleTimeout == 0 {
			s.IdleTimeout = Duration(1 * time.Hour)
		}
		if s.MinUptime == 0 {
			s.MinUptime = Duration(2 * time.Minute)
		}
		if s.HealthTimeout == 0 {
			s.HealthTimeout = Duration(10 * time.Second)
		}
		if s.StartTimeout == 0 {
			s.StartTimeout = Duration(3 * time.Minute)
		}
	}
}

func (c *Config) validate() error {
	if len(c.Services) == 0 {
		return fmt.Errorf("config: no services defined")
	}
	seen := make(map[string]struct{}, len(c.Services))
	for i, s := range c.Services {
		if s.Host == "" {
			return fmt.Errorf("config: services[%d]: host is required", i)
		}
		if s.ComposeDir == "" {
			return fmt.Errorf("config: services[%d] (%s): compose_dir is required", i, s.Host)
		}
		if s.Upstream == "" {
			return fmt.Errorf("config: services[%d] (%s): upstream is required", i, s.Host)
		}
		if s.Health == "" {
			return fmt.Errorf("config: services[%d] (%s): health is required", i, s.Host)
		}
		if s.HealthExpectStatus < 100 || s.HealthExpectStatus > 599 {
			return fmt.Errorf("config: services[%d] (%s): health_expect_status must be a valid HTTP status", i, s.Host)
		}
		if _, dup := seen[s.Host]; dup {
			return fmt.Errorf("config: duplicate host %q", s.Host)
		}
		seen[s.Host] = struct{}{}
	}
	return nil
}
