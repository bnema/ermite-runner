// SPDX-License-Identifier: MIT

// Package deps applies dependency updates inside a job sandbox.
package deps

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

const MaxFiles = 64
const MaxFile = 2 << 20
const MaxTotal = 8 << 20
const MaxChunk = 1 << 20

var goVersion = regexp.MustCompile(`^1\.\d+(\.\d+)?$`)
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func Allowed(r *execpb.DepsRequest) (map[string]bool, error) {
	if !goVersion.MatchString(r.GoVersion) {
		return nil, errors.New("invalid Go version")
	}
	out := map[string]bool{}
	for _, u := range r.Updates {
		if u == nil || u.Ecosystem != "gomod" {
			return nil, errors.New("unsupported dependency ecosystem")
		}
		d := u.Directory
		if d != "" && (path.Clean(d) != d || d == "." || strings.HasPrefix(d, "/") || d == ".." || strings.HasPrefix(d, "../") || strings.Contains(d, "\\")) {
			return nil, errors.New("invalid module directory")
		}
		if u.Name == "" || u.Version == "" || strings.HasPrefix(u.Name, "-") || strings.ContainsAny(u.Name+u.Version, "\x00\r\n @") {
			return nil, errors.New("invalid dependency update")
		}
		out[path.Join(d, "go.mod")] = true
		out[path.Join(d, "go.sum")] = true
	}
	if len(out) == 0 || len(out) > MaxFiles {
		return nil, errors.New("invalid number of dependency files")
	}
	return out, nil
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// Script groups updates by directory and pins the module environment.
func Script(r *execpb.DepsRequest) (string, error) {
	if _, err := Allowed(r); err != nil {
		return "", err
	}
	groups := map[string][]string{}
	for _, u := range r.Updates {
		groups[u.Directory] = append(groups[u.Directory], quote(u.Name+"@"+u.Version))
	}
	dirs := make([]string, 0, len(groups))
	for d := range groups {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	s := "set -eu\nexport GOFLAGS=-mod=mod GOTOOLCHAIN=local GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org GOPRIVATE= GONOSUMDB=\n"
	for _, d := range dirs {
		// `go get` adds a toolchain line (the image's Go) to a go.mod that has none, and
		// raises one that is older. Bernard only accepts the toolchain it was given, so put
		// the original back, or drop the line when there was none.
		s += "(cd /repo/" + quote(d) + "; tc=$(awk '$1==\"toolchain\"{print $2}' go.mod); go get " + strings.Join(groups[d], " ") + "; go mod tidy; go mod edit -toolchain=\"${tc:-none}\")\n"
	}
	return s, nil
}

func basicCredential(token string) string {
	return base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
}

// Secrets lists every string of the clone credentials that must not reach a
// log: the token, the x-access-token pair, its base64 value and the header.
func Secrets(token string) []string {
	if token == "" {
		return nil
	}
	cred := basicCredential(token)
	return []string{token, "x-access-token:" + token, cred, "Authorization: Basic " + cred}
}

// Run uses the sandbox's Docker daemon, never the host daemon. Files are read
// through an os.Root so repository symlinks cannot escape the checkout.
func Run(ctx context.Context, r *execpb.DepsRequest, log io.Writer) (map[string][]byte, error) {
	allowed, err := Allowed(r)
	if err != nil {
		return nil, err
	}
	script, err := Script(r)
	if err != nil {
		return nil, err
	}
	if err := checkCloneURL(r.CloneUrl, r.AllowInsecureHttp); err != nil {
		return nil, err
	}
	if !shaPattern.MatchString(r.Sha) {
		return nil, errors.New("invalid commit SHA")
	}
	dir, err := os.MkdirTemp("", "ermite-deps-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	// Authentication is passed in the environment, not a URL or command line.
	auth := "Authorization: Basic " + basicCredential(r.Token)
	prefix := "set -eu\ngit -c http.extraHeader=\"$DEPS_AUTH\" clone --no-checkout -- " + quote(r.CloneUrl) + " /repo\nunset DEPS_AUTH\ncd /repo\ngit checkout --detach " + quote(r.Sha) + "\n"
	// Preserve the initial manifests for byte-for-byte change detection.
	prefix += "mkdir /before\n"
	paths := make([]string, 0, len(allowed))
	for p := range allowed {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for i, p := range paths {
		prefix += "if test -f " + quote(p) + "; then cp -- " + quote(p) + fmt.Sprintf(" /before/%d; fi\n", i)
	}
	// Capture changed files only; the guest checks all file sizes before sending.
	suffix := "\n"
	for i, p := range paths {
		suffix += fmt.Sprintf("if test -f %s && ! cmp -s %s /before/%d; then cp -- %s /out/%d; fi\n", quote("/repo/"+p), quote("/repo/"+p), i, quote("/repo/"+p), i)
	}
	name := fmt.Sprintf("ermite-deps-%d", os.Getpid())
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(cleanup, "docker", "rm", "-f", name).Run()
	}()
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--name", name, "-e", "DEPS_AUTH", "-v", dir+":/out", "golang:"+r.GoVersion, "sh", "-c", prefix+script+suffix)
	cmd.Env = append(os.Environ(), "DEPS_AUTH="+auth)
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Run(); err != nil {
		return nil, errors.New("dependency command failed")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	files := map[string][]byte{}
	total := 0
	for i, p := range paths {
		f, err := root.Open(fmt.Sprint(i))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(f, MaxFile+1))
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		total += len(b)
		if len(b) > MaxFile || total > MaxTotal {
			return nil, errors.New("dependency output exceeds limits")
		}
		files[p] = b
	}
	return files, nil
}

// Collector validates the untrusted guest's chunk stream before allocating.
type Collector struct {
	Allowed  map[string]bool
	Files    map[string][]byte
	current  string
	data     []byte
	total    int
	finished bool
}

func (c *Collector) Add(f *execpb.DepsFileChunk) error {
	if c.finished || f == nil || !c.Allowed[f.Path] || len(f.Data) > MaxChunk {
		return errors.New("invalid dependency chunk")
	}
	if c.Files == nil {
		c.Files = map[string][]byte{}
	}
	if c.current == "" {
		if _, ok := c.Files[f.Path]; ok || len(c.Files) >= MaxFiles {
			return errors.New("duplicate or excessive dependency file")
		}
		c.current = f.Path
	}
	if c.current != f.Path || f.Offset != uint64(len(c.data)) || len(c.data)+len(f.Data) > MaxFile || c.total+len(f.Data) > MaxTotal {
		return errors.New("invalid dependency chunk bounds")
	}
	c.data = append(c.data, f.Data...)
	c.total += len(f.Data)
	if f.Last {
		c.Files[c.current] = c.data
		c.current = ""
		c.data = nil
	}
	return nil
}
func (c *Collector) Finish(r *execpb.DepsResult) error {
	if c.finished || c.current != "" || r == nil || int(r.FileCount) != len(c.Files) {
		return errors.New("invalid dependency result")
	}
	c.finished = true
	if r.Error != "" {
		return errors.New(r.Error)
	}
	return nil
}
func (c *Collector) Done() bool { return c.finished }

// checkCloneURL accepts an https URL, or an http one when allowHTTP is set;
// userinfo and an empty host are always refused.
func checkCloneURL(raw string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("clone URL must use HTTPS")
	}
	okScheme := u.Scheme == "https" || (u.Scheme == "http" && allowHTTP)
	if !okScheme || u.Host == "" || u.User != nil {
		return errors.New("clone URL must use HTTPS")
	}
	return nil
}
