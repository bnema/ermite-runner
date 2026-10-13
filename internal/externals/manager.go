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
	// RetryEvery is the interval of the background retries of the downloads that
	// failed for a reason that may pass (default 5 minutes).
	RetryEvery time.Duration

	mu      sync.Mutex
	state   map[string]entry // by artifact name
	syncing bool

	// stubMu serialises the creation of the directories of unavailable tools;
	// stubs remembers the ones built (or checked) by this process.
	stubMu sync.Mutex
	stubs  map[string]bool
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
// unavailable ("still being downloaded"). Downloads that failed for a reason
// that may pass (network, HTTP status) are retried every RetryEvery, jobs or
// not, until ctx ends.
func (m *Manager) Start(ctx context.Context, wait time.Duration) {
	log := zerowrap.FromCtx(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := m.Sync(ctx); err != nil && ctx.Err() == nil {
			log.Warn().Err(err).Msg("some external tools are not available: jobs that need them fail with the reason; the download is retried")
		}
	}()
	go m.retryLoop(ctx)
	select {
	case <-done:
	case <-time.After(wait):
		log.Warn().Dur("waited", wait).Msg("external tools are still downloading: the runner takes jobs, those that need them fail until they are ready")
	case <-ctx.Done():
	}
}

// retryLoop calls Sync every RetryEvery while a tool is missing for a reason
// that may pass.
func (m *Manager) retryLoop(ctx context.Context) {
	every := m.RetryEvery
	if every == 0 {
		every = 5 * time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if m.wantsRetry() {
			_ = m.Sync(ctx)
		}
	}
}

func (m *Manager) wantsRetry() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.state {
		if e.retry {
			return true
		}
	}
	return false
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
	defer func() { m.mu.Lock(); m.syncing = false; m.mu.Unlock() }()

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
// is reported as being downloaded. It has no side effect on the downloads (the
// retries run on a timer, see Start); the directories of unavailable tools are
// built at most once and only touched here.
func (m *Manager) Externals() []sandbox.External {
	m.mu.Lock()
	names := m.names()
	got := make([]entry, len(names))
	have := make([]bool, len(names))
	for i, n := range names {
		got[i], have[i] = m.state[n]
	}
	m.mu.Unlock()

	// Outside the lock: the first call for a reason writes files.
	out := make([]sandbox.External, len(names))
	for i, n := range names {
		switch {
		case !have[i]:
			got[i] = m.unavailable(n, n+" is still being downloaded on this runner: run the job again in a few minutes", false)
		case got[i].ext.Unavailable:
			touch(got[i].ext.Dir) // handed out again: not stale
		}
		out[i] = got[i].ext
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

// unavailableStub is the bin/node of a tool that is missing: it prints the
// reason (the file UNAVAILABLE next to bin/) on stderr and fails, so that a job
// which runs the tool anyway (a node action, a run step) gets the reason rather
// than "No such file or directory". It does not contain the reason.
const unavailableStub = "#!/bin/sh\ncat \"$(dirname \"$0\")/../" + sandbox.ExternalUnavailableFile + "\" >&2\nexit 1\n"

// unavailable returns the entry of a tool that is missing: a directory with the
// reason in ExternalUnavailableFile and the bin/node stub. Its name carries a
// hash of the reason, so that the ID (and so a VM's disk) changes only when the
// reason does. The directory is built once per process, complete, under a
// temporary name and renamed into place; files that exist with the right
// content are never rewritten, and a missing or different one is replaced
// through a rename, so a reader (the build of a VM's externals disk, a job that
// runs the stub) never sees a file half written. Each call touches the
// directory: the pruning keeps what was handed out in the last hour.
func (m *Manager) unavailable(name, reason string, retry bool) entry {
	sum := sha256.Sum256([]byte(name + "\x00" + reason))
	id := "unavailable-" + hex.EncodeToString(sum[:8])
	dir := filepath.Join(m.Dir, unavailableDir, name+"-"+id)
	e := entry{retry: retry, ext: sandbox.External{Name: name, ID: id, Dir: dir, Unavailable: true}}

	m.stubMu.Lock()
	defer m.stubMu.Unlock()
	if _, err := os.Lstat(filepath.Join(dir, "bin", "node")); err != nil || !m.stubs[dir] {
		// Best effort: on failure the entry stays marked unavailable and the
		// sandbox sees whatever there is.
		if err := buildStub(dir, reason); err == nil {
			if m.stubs == nil {
				m.stubs = map[string]bool{}
			}
			m.stubs[dir] = true
		}
	}
	touch(dir)
	return e
}

func touch(dir string) {
	now := time.Now()
	_ = os.Chtimes(dir, now, now)
}

// buildStub makes dir hold the UNAVAILABLE file and the bin/node stub, with
// explicit modes (the umask of the runner must not hide them from the user of
// the sandbox).
func buildStub(dir, reason string) error {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		return err
	}
	files := []stubFile{
		{sandbox.ExternalUnavailableFile, reason + "\n", 0o644},
		{"bin/node", unavailableStub, 0o755},
	}
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		tmp, err := os.MkdirTemp(parent, ".tmp-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		if err := os.Mkdir(filepath.Join(tmp, "bin"), 0o755); err != nil {
			return err
		}
		for _, d := range []string{tmp, filepath.Join(tmp, "bin")} {
			if err := os.Chmod(d, 0o755); err != nil {
				return err
			}
		}
		for _, f := range files {
			if err := f.write(tmp); err != nil {
				return err
			}
		}
		if err := os.Rename(tmp, dir); err == nil {
			return nil
		}
		// Lost a race with another process, or the directory appeared: repair it.
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		return err
	}
	for _, f := range files {
		if f.current(dir) {
			continue
		}
		if err := f.write(dir); err != nil {
			return err
		}
	}
	return nil
}

type stubFile struct {
	name, content string
	mode          os.FileMode
}

// current reports whether the file exists with this content and mode.
func (f stubFile) current(dir string) bool {
	p := filepath.Join(dir, f.name)
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != f.mode {
		return false
	}
	b, err := os.ReadFile(p)
	return err == nil && string(b) == f.content
}

// write creates the file through a temporary file in the same directory and a
// rename, never in place.
func (f stubFile) write(dir string) error {
	p := filepath.Join(dir, f.name)
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-")
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.WriteString(f.content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(f.mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return err
	}
	ok = true
	return nil
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
	if err := extractTarGz(archive, tree, a.StripTop); err != nil {
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
