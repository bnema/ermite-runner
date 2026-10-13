// SPDX-License-Identifier: MIT

// Package externals provides the tools that jobs expect to find outside the
// image, today the Node.js builds of actions/runner (node20, node24 and the
// alpine variants for job containers), so that a runner upgrade or a new Node.js
// version never needs an image rebuild.
//
// The host downloads the pinned builds (table.go) into
// <state dir>/externals/<name>-<version>-<arch>/ at start, verifying the
// SHA-256 of each archive before it is unpacked, and the sandbox drivers attach
// them read-only at /home/runner/externals/<name> (GitHub's layout:
// externals/node24/bin/node). Only the pinned versions are kept.
//
// A tool that cannot be provided (offline, checksum mismatch, no build for this
// architecture, checksum not recorded) never stops the runner: the sandboxes get
// a directory with the file UNAVAILABLE and a bin/node stub that print why, and
// a job that needs the tool fails with that message. Failed downloads are retried in the
// background.
package externals

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bnema/zerowrap"

	"bnema.dev/ermite-runner/internal/sandbox"
)

// maxDownload bounds one archive (the largest build is about 60 MiB).
const maxDownload = 512 << 20

// Manager downloads, verifies and keeps the externals of one architecture.
type Manager struct {
	// Dir is <state dir>/externals.
	Dir string
	// Arch is GitHub's architecture name of the host (x64, arm64).
	Arch string
	// Table is the set of known artifacts (Pinned in production).
	Table []Artifact
	// Client downloads the archives; nil means a client with a 15 minute limit
	// that follows redirects to https locations only.
	Client *http.Client
	// RetryEvery is how long a failed download waits before the next attempt
	// (default 5 minutes).
	RetryEvery time.Duration

	mu      sync.Mutex
	state   map[string]entry // by artifact name
	bg      context.Context  // the context of Start, for retries
	lastTry time.Time
	syncing bool
}

type entry struct {
	ext   sandbox.External
	retry bool // a failure worth trying again (network), not a verdict
}

// New returns a Manager for the host architecture; ok is false when the runner
// has no builds for it.
func New(stateDir string) (m *Manager, ok bool) {
	arch := HostArch()
	return &Manager{Dir: filepath.Join(stateDir, "externals"), Arch: arch, Table: Pinned}, arch != ""
}

// Start runs Sync in the background and waits for it at most wait, so that a
// runner with a working network normally takes its first job with everything in
// place, and one without never waits long. When the wait ends the download goes
// on; jobs that start meanwhile see the tools that are not done yet as
// unavailable ("still being downloaded"). Retries of failed downloads use ctx
// too: they stop with it.
func (m *Manager) Start(ctx context.Context, wait time.Duration) {
	log := zerowrap.FromCtx(ctx)
	m.mu.Lock()
	m.bg = ctx
	m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := m.Sync(ctx); err != nil && ctx.Err() == nil {
			log.Warn().Err(err).Msg("some external tools are not available: jobs that need them fail with the reason; the download is retried")
		}
	}()
	select {
	case <-done:
	case <-time.After(wait):
		log.Warn().Dur("waited", wait).Msg("external tools are still downloading: the runner takes jobs, those that need them fail until they are ready")
	case <-ctx.Done():
	}
}

// Sync makes the externals match the table: it downloads what is missing,
// removes everything else from Dir, and records the outcome per tool as it goes.
// It returns an error describing the artifacts that could not be provided; the
// runner goes on with them marked unavailable.
func (m *Manager) Sync(ctx context.Context) error {
	m.mu.Lock()
	if m.syncing {
		m.mu.Unlock()
		return nil
	}
	m.syncing = true
	if m.state == nil {
		m.state = map[string]entry{}
	}
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.syncing = false; m.lastTry = time.Now(); m.mu.Unlock() }()

	log := zerowrap.FromCtx(ctx)
	var errs []error
	keep := map[string]bool{}
	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return err
	}
	for _, name := range m.names() {
		a, ok := m.artifact(name)
		var e entry
		switch {
		case !ok:
			e = m.unavailable(name, fmt.Sprintf("there is no %s build for linux-%s", name, m.Arch), false)
		case a.SHA256 == "":
			e = m.unavailable(name, fmt.Sprintf("%s (Node.js %s) is not available yet on this runner: %s", a.Name, a.Version, a.Pending), false)
		default:
			var err error
			if e, err = m.ensure(ctx, a); err != nil {
				errs = append(errs, fmt.Errorf("%s %s: %w", a.Name, a.Version, err))
				log.Warn().Err(err).Str("external", a.Name).Str("version", a.Version).Msg("external tool not available")
			} else {
				log.Info().Str("external", a.Name).Str("version", a.Version).Str("arch", a.Arch).Msg("external tool ready")
			}
		}
		m.mu.Lock()
		m.state[name] = e
		m.mu.Unlock()
		keep[filepath.Base(e.ext.Dir)] = true
		if e.ext.Unavailable {
			keep[unavailableDir] = true
		}
	}
	m.prune(log, keep)
	return errors.Join(errs...)
}

// Externals implements sandbox.ExternalsSource. A tool that has no outcome yet
// is reported as being downloaded. When a download failed for a reason that may
// pass, a retry is started in the background (at most one per RetryEvery).
func (m *Manager) Externals() []sandbox.External {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []sandbox.External
	retry := false
	for _, n := range m.names() {
		e, ok := m.state[n]
		if !ok {
			e = m.unavailable(n, n+" is still being downloaded on this runner: run the job again in a few minutes", false)
		}
		retry = retry || e.retry
		out = append(out, e.ext)
	}
	every := m.RetryEvery
	if every == 0 {
		every = 5 * time.Minute
	}
	if retry && !m.syncing && m.bg != nil && m.bg.Err() == nil && time.Since(m.lastTry) >= every {
		m.lastTry = time.Now() // no second attempt before the next interval
		go func(ctx context.Context) { _ = m.Sync(ctx) }(m.bg)
	}
	return out
}

// names is every artifact name in the table, in table order, once.
func (m *Manager) names() []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range m.Table {
		if !seen[a.Name] {
			seen[a.Name] = true
			out = append(out, a.Name)
		}
	}
	return out
}

func (m *Manager) artifact(name string) (Artifact, bool) {
	for _, a := range m.Table {
		if a.Name == name && a.Arch == m.Arch {
			return a, true
		}
	}
	return Artifact{}, false
}

const unavailableDir = ".unavailable"

// unavailable builds the entry of a tool that is missing: a directory with the
// reason in ExternalUnavailableFile and a bin/node stub that prints it. Its name carries a hash of the reason, so
// that the ID (and so a VM's disk) changes only when the reason does.
func (m *Manager) unavailable(name, reason string, retry bool) entry {
	sum := sha256.Sum256([]byte(name + "\x00" + reason))
	id := "unavailable-" + hex.EncodeToString(sum[:8])
	dir := filepath.Join(m.Dir, unavailableDir, name+"-"+id)
	e := entry{retry: retry, ext: sandbox.External{Name: name, ID: id, Dir: dir, Unavailable: true}}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		return e // still marked unavailable; the sandbox then has an empty directory
	}
	// Modes are set explicitly: the umask of the runner must not hide the files
	// from the user of the sandbox.
	for _, d := range []string{filepath.Dir(dir), dir, filepath.Join(dir, "bin")} {
		_ = os.Chmod(d, 0o755)
	}
	write := func(name, content string, mode os.FileMode) {
		p := filepath.Join(dir, name)
		if os.WriteFile(p, []byte(content), mode) == nil {
			_ = os.Chmod(p, mode)
		}
	}
	write(sandbox.ExternalUnavailableFile, reason+"\n", 0o644)
	// A stub where the executable would be, so that a job which runs it anyway
	// (a node action, a run step) fails with the reason instead of "No such file
	// or directory". The reason goes through a quoted here-document: it is data.
	stub := "#!/bin/sh\ncat >&2 <<'ERMITE_UNAVAILABLE'\n" + strings.ReplaceAll(reason, "\nERMITE_UNAVAILABLE", "") + "\nERMITE_UNAVAILABLE\nexit 1\n"
	write("bin/node", stub, 0o755)
	return e
}

// ensure returns the ready entry of a, downloading it when needed. On failure it
// returns the unavailable entry together with the error.
func (m *Manager) ensure(ctx context.Context, a Artifact) (entry, error) {
	final := filepath.Join(m.Dir, a.dirName())
	if isReady(final) {
		return readyEntry(a, final), nil
	}
	err := m.fetch(ctx, a, final)
	if err != nil {
		reason := fmt.Sprintf("%s (Node.js %s) is not available on this runner: %v", a.Name, a.Version, err)
		var v *verdictError
		return m.unavailable(a.Name, reason, !errors.As(err, &v)), err
	}
	return readyEntry(a, final), nil
}

func readyEntry(a Artifact, dir string) entry {
	return entry{ext: sandbox.External{Name: a.Name, ID: a.dirName() + "-" + a.SHA256[:16], Dir: dir}}
}

// isReady reports whether dir holds a complete tool: a regular executable
// bin/node. Directories only appear under their final name by atomic rename
// after a verified extraction.
func isReady(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "bin", "node"))
	return err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0
}

// verdictError is a failure that another attempt will not change (the checksum
// differs, the archive is unsafe).
type verdictError struct{ error }

func (e *verdictError) Unwrap() error { return e.error }

// fetch downloads a, verifies its checksum, unpacks it next to final and
// renames it into place.
func (m *Manager) fetch(ctx context.Context, a Artifact, final string) error {
	u, err := url.Parse(a.URL)
	if err != nil || u.Scheme != "https" {
		return &verdictError{fmt.Errorf("%q is not an https URL", a.URL)}
	}
	tmp, err := os.MkdirTemp(m.Dir, ".tmp-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	archive := filepath.Join(tmp, "archive.tar.gz")
	if err := m.download(ctx, a, archive); err != nil {
		return err
	}
	tree := filepath.Join(tmp, "tree")
	if err := os.Mkdir(tree, 0o755); err != nil {
		return err
	}
	if err := extractTarGz(archive, tree); err != nil {
		return &verdictError{fmt.Errorf("unpack: %w", err)}
	}
	if !isReady(tree) {
		return &verdictError{errors.New("the archive has no executable bin/node")}
	}
	if err := os.RemoveAll(final); err != nil { // an incomplete directory of an older run
		return err
	}
	return os.Rename(tree, final)
}

// download fetches a.URL to dest and verifies its SHA-256 as it arrives.
func (m *Manager) download(ctx context.Context, a Artifact, dest string) error {
	client := m.Client
	if client == nil {
		client = newClient()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", a.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", a.URL, resp.Status)
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxDownload+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", a.URL, err)
	}
	if n > maxDownload {
		return &verdictError{fmt.Errorf("download %s: more than %d bytes", a.URL, int64(maxDownload))}
	}
	if err := VerifySum(hex.EncodeToString(h.Sum(nil)), a.SHA256); err != nil {
		return &verdictError{fmt.Errorf("download %s: %w", a.URL, err)}
	}
	return nil
}

// newClient is the default downloader: a time limit, and redirects only to
// https locations (nodejs.org does not redirect; GitHub releases do).
func newClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to a non-https URL %s", req.URL.Redacted())
			}
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

// VerifySum compares two hex SHA-256 values.
func VerifySum(got, want string) error {
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("SHA-256 mismatch: got %s, want %s", got, want)
	}
	return nil
}

// prune removes everything in Dir that is not pinned: other versions, other
// architectures, leftovers of an interrupted download. The reasons of
// unavailable tools (a few bytes each) may be bind-mounted into a running
// container, so those are only removed once they are an hour old.
func (m *Manager) prune(log zerowrap.Logger, keep map[string]bool) {
	ents, err := os.ReadDir(m.Dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		switch name := e.Name(); {
		case keep[name]:
		case name == unavailableDir:
			m.pruneUnavailable(keep)
		default:
			if err := os.RemoveAll(filepath.Join(m.Dir, name)); err != nil {
				log.Warn().Err(err).Str("path", name).Msg("removing an unpinned external tool")
			}
		}
	}
}

func (m *Manager) pruneUnavailable(keep map[string]bool) {
	dir := filepath.Join(m.Dir, unavailableDir)
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		fi, err := e.Info()
		if keep[e.Name()] || err != nil || time.Since(fi.ModTime()) < time.Hour {
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}
