// Package docker wraps the `docker compose` CLI for a single project.
package docker

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// Compose targets one docker-compose project via the CLI.
// We use `stop`/`start` (not `down`/`up`): containers are preserved, so a warm
// start is fast and volumes/networks are untouched.
type Compose struct {
	Bin     string // docker binary path (default "docker")
	Dir     string // working directory containing docker-compose.yml
	Project string // optional explicit project name (-p); otherwise inferred from the file/dir
}

func (c Compose) args(action ...string) []string {
	args := []string{"compose"}
	if c.Project != "" {
		args = append(args, "-p", c.Project)
	}
	return append(args, action...)
}

func (c Compose) run(action ...string) ([]byte, error) {
	cmd := exec.Command(c.Bin, c.args(action...)...)
	cmd.Dir = c.Dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("docker %s: %w: %s", strings.Join(action, " "), err, out)
	}
	return out, nil
}

// Start runs `docker compose start` (starts existing stopped containers).
func (c Compose) Start() error {
	_, err := c.run("start")
	return err
}

// Stop runs `docker compose stop` (graceful SIGTERM, containers preserved).
func (c Compose) Stop() error {
	_, err := c.run("stop")
	return err
}

// AnyRunning reports whether at least one container in the project is running.
// Used only for boot reconciliation / diagnostics.
func (c Compose) AnyRunning() (bool, error) {
	out, err := c.run("ps", "-q")
	if err != nil {
		return false, err
	}
	return len(bytes.TrimSpace(out)) > 0, nil
}
