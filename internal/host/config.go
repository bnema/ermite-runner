// SPDX-License-Identifier: MIT

package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"bnema.dev/ermite-api/gen/ermite/ermiteconnect"
)

// Environment variables of the host process. The sandbox settings
// (ERMITE_RUNNER_ISOLATION, _CAPACITY, _IMAGE, _JOB_*) are read by package
// sandbox.
const (
	EnvURL           = "ERMITE_RUNNER_URL"
	EnvName          = "ERMITE_RUNNER_NAME"
	EnvLabels        = "ERMITE_RUNNER_LABELS"
	EnvRegisterToken = "ERMITE_RUNNER_REGISTER_TOKEN"
	// EnvInsecureHTTP allows a plain http server URL that is not loopback. The
	// runner token and the job tokens then travel in clear: tests only.
	EnvInsecureHTTP = "ERMITE_RUNNER_INSECURE_HTTP"
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
	// InsecureHTTP allows a non-loopback http URL (ERMITE_RUNNER_INSECURE_HTTP=1).
	InsecureHTTP bool
}

// Client returns the client for a token, honouring InsecureHTTP.
func (c Config) Client(url, token string) ermiteconnect.RunnerServiceClient {
	if c.InsecureHTTP {
		return NewInsecureClient(url, token, http.DefaultTransport)
	}
	return NewClient(url, token, http.DefaultTransport)
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
	switch strings.ToLower(strings.TrimSpace(getenv(EnvInsecureHTTP))) {
	case "", "0", "false", "no":
	case "1", "true", "yes":
		c.InsecureHTTP = true
	default:
		return c, fmt.Errorf("%s: %q is not 0 or 1", EnvInsecureHTTP, getenv(EnvInsecureHTTP))
	}
	if c.URL != "" {
		if err := CheckURL(c.URL, c.InsecureHTTP); err != nil {
			return c, fmt.Errorf("%s: %w", EnvURL, err)
		}
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
	// MkdirAll leaves an existing directory as it is; the directory holds the
	// runner token, so it is made private (the VM images live in it too, and
	// the runner runs as root: nobody else needs to enter).
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("make the state directory private: %w", err)
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

// ResolveRegisterToken returns the token for `register`, from the first source
// that has one: standard input (--token-stdin, read to the end and trimmed),
// the environment (ERMITE_RUNNER_REGISTER_TOKEN), then the --token flag. The
// flag is last because the arguments of a process are visible to every user of
// the host (ps, /proc/<pid>/cmdline); the other two are not.
func ResolveRegisterToken(flagValue string, fromStdin bool, stdin io.Reader, getenv func(string) string) (string, error) {
	if fromStdin {
		b, err := io.ReadAll(io.LimitReader(stdin, 4096))
		if err != nil {
			return "", fmt.Errorf("reading the token from standard input: %w", err)
		}
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, nil
		}
		return "", errors.New("standard input has no token")
	}
	if t := strings.TrimSpace(getenv(EnvRegisterToken)); t != "" {
		return t, nil
	}
	if t := strings.TrimSpace(flagValue); t != "" {
		return t, nil
	}
	return "", fmt.Errorf("give the token in %s or on standard input (--token-stdin); --token works too but is visible in the process list", EnvRegisterToken)
}
