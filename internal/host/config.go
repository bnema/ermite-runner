// SPDX-License-Identifier: MIT

package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Environment variables of the host process. The sandbox settings
// (ERMITE_RUNNER_ISOLATION, _CAPACITY, _IMAGE, _JOB_*) are read by package
// sandbox.
const (
	EnvURL           = "ERMITE_RUNNER_URL"
	EnvName          = "ERMITE_RUNNER_NAME"
	EnvLabels        = "ERMITE_RUNNER_LABELS"
	EnvRegisterToken = "ERMITE_RUNNER_REGISTER_TOKEN"
)

// Config is the host part of the runner configuration.
type Config struct {
	// URL is the base URL of the ermite server.
	URL string
	// Name is the runner name: 1 to 64 letters, digits, ".", "_" and "-".
	Name string
	// Labels are the extra labels of the runner.
	Labels []string
	// StateDir holds the runner token (mode 0600).
	StateDir string
	// Capacity is the number of jobs in parallel.
	Capacity int
	// Version is the version of the runner binary.
	Version string
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// LoadConfig reads the host settings. hostname is the default runner name.
// stateDir and capacity come from the sandbox configuration, so that both
// agree.
func LoadConfig(getenv func(string) string, hostname, stateDir string, capacity int, version string) (Config, error) {
	c := Config{URL: strings.TrimSpace(getenv(EnvURL)), Name: strings.TrimSpace(getenv(EnvName)), StateDir: stateDir, Capacity: capacity, Version: version}
	if c.Name == "" {
		c.Name = sanitizeName(hostname)
	}
	if !nameRe.MatchString(c.Name) {
		return c, fmt.Errorf("%s: %q is not a valid runner name (1 to 64 letters, digits, '.', '_' and '-')", EnvName, c.Name)
	}
	seen := map[string]bool{}
	for _, l := range strings.Split(getenv(EnvLabels), ",") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if !nameRe.MatchString(l) {
			return c, fmt.Errorf("%s: %q is not a valid label (1 to 64 letters, digits, '.', '_' and '-')", EnvLabels, l)
		}
		if k := strings.ToLower(l); !seen[k] {
			seen[k] = true
			c.Labels = append(c.Labels, l)
		}
	}
	return c, nil
}

func sanitizeName(s string) string {
	b := []byte(s)
	for i, ch := range b {
		if ok := ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '.' || ch == '_' || ch == '-'; !ok {
			b[i] = '-'
		}
	}
	if len(b) > 64 {
		b = b[:64]
	}
	if len(b) == 0 {
		return "runner"
	}
	return string(b)
}

// State is what register stores: the runner token and who it belongs to. The
// file is mode 0600 and the token is never logged.
type State struct {
	URL         string `json:"url"`
	RunnerID    string `json:"runner_id"`
	Name        string `json:"name"`
	RunnerToken string `json:"runner_token"`
}

// StateFile is the path of the state file in dir.
func StateFile(dir string) string { return filepath.Join(dir, "runner.json") }

// ErrNotRegistered is returned by LoadState when no runner is registered.
var ErrNotRegistered = errors.New("this runner is not registered: run `ermite-runner register` first")

// SaveState writes the state atomically with mode 0600.
func SaveState(dir string, s State) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".runner-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), StateFile(dir))
}

// LoadState reads the state; it refuses a file readable by others, because the
// runner token is a secret.
func LoadState(dir string) (State, error) {
	p := StateFile(dir)
	st, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, ErrNotRegistered
	}
	if err != nil {
		return State{}, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return State{}, fmt.Errorf("%s is accessible by other users (mode %o): chmod 600 it", p, st.Mode().Perm())
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, fmt.Errorf("%s: %w", p, err)
	}
	if s.RunnerToken == "" {
		return State{}, fmt.Errorf("%s has no runner token", p)
	}
	return s, nil
}
