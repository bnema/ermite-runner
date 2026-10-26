// SPDX-License-Identifier: MIT
package deps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"bnema.dev/ermite-runner/internal/actionscache"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

const cacheLimit int64 = 1 << 30
const cacheFiles = 200000
const cacheAPI = "/_apis/artifactcache/"

var cacheVersion = fmt.Sprintf("%x", sha256.Sum256([]byte("ermite-deps-gomod-download-v1")))

func cacheKey(r *execpb.DepsRequest) string { return "gomod-" + r.GoVersion + "-" + r.Sha }

type moduleCache struct {
	ctx                   context.Context
	client                *http.Client
	base, token, key, dir string
	exact                 bool
	log                   io.Writer
	warned                bool
}

func (c *moduleCache) warn() {
	if !c.warned {
		_, _ = fmt.Fprintln(c.log, "warning: Go module cache unavailable; continuing without cache")
		c.warned = true
	}
}

func (c *moduleCache) request(method, target string, body io.Reader, auth bool, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(c.ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, errors.New("cache request failed")
	}
	return resp, nil
}

func (c *moduleCache) restore(prefix string) error {
	q := url.Values{"keys": {c.key + "," + prefix}, "version": {cacheVersion}}
	resp, err := c.request("GET", c.base+cacheAPI+"cache?"+q.Encode(), nil, true, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	var hit struct {
		CacheKey        string `json:"cacheKey"`
		ArchiveLocation string `json:"archiveLocation"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&hit); err != nil {
		return err
	}
	// Signed archive URLs must remain on the local relay, even for a corrupt reply.
	u, err := url.Parse(hit.ArchiveLocation)
	base, _ := url.Parse(c.base)
	if err != nil || u.Scheme != base.Scheme || u.Host != base.Host || u.User != nil {
		return errors.New("invalid cache archive URL")
	}
	archive, err := c.request("GET", u.String(), nil, false, nil)
	if err != nil {
		return err
	}
	defer func() { _ = archive.Body.Close() }()
	if err := extractCache(io.LimitReader(archive.Body, cacheLimit+1), c.dir, cacheLimit, cacheFiles); err != nil {
		return err
	}
	c.exact = hit.CacheKey == c.key
	return nil
}

// extractCache accepts only a download tree of regular files/directories. The
// private, empty destination and os.Root prevent archive paths escaping it.
func extractCache(src io.Reader, dir string, limit int64, count int) error {
	gz, err := gzip.NewReader(src)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	expanded := &io.LimitedReader{R: gz, N: cacheLimit + 1}
	tr := tar.NewReader(expanded)
	var total int64
	for n := 0; ; n++ {
		h, err := tr.Next()
		if err == io.EOF {
			_, err = io.Copy(io.Discard, expanded)
			if expanded.N == 0 {
				return errors.New("cache archive exceeds expanded limit")
			}
			return err
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(h.Name, "/")
		if n >= count || h.Size < 0 || h.Size > limit-total || path.Clean(name) != name || strings.Contains(name, "\\") || (name != "cache/download" && !strings.HasPrefix(name, "cache/download/")) {
			return errors.New("invalid cache archive bounds or path")
		}
		total += h.Size
		switch h.Typeflag {
		case tar.TypeDir:
			if h.Size != 0 {
				return errors.New("invalid cache directory")
			}
			if err := root.MkdirAll(name, 0700); err != nil {
				return err
			}
		case tar.TypeReg:
			if name == "cache/download" {
				return errors.New("cache download root is not a directory")
			}
			if err := root.MkdirAll(path.Dir(name), 0700); err != nil {
				return err
			}
			f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			_, err = io.CopyN(f, tr, h.Size)
			closeErr := f.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return errors.New("unsupported cache archive entry")
		}
	}
}

type cappedWriter struct {
	w         io.Writer
	remaining int64
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("cache archive exceeds limit")
	}
	n, err := w.w.Write(p)
	w.remaining -= int64(n)
	return n, err
}

func archiveCache(dir string, dst io.Writer) error {
	gz := gzip.NewWriter(&cappedWriter{dst, cacheLimit})
	tw := tar.NewWriter(&cappedWriter{gz, cacheLimit})
	var total int64
	count := 0
	err := filepath.WalkDir(filepath.Join(dir, "cache", "download"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		count++
		if count > cacheFiles || (!info.IsDir() && !info.Mode().IsRegular()) || info.Size() > cacheLimit-total {
			return errors.New("cache tree exceeds limits or has special files")
		}
		if !info.IsDir() {
			total += info.Size()
		}
		name, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		h, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(name)
		h.Uid = 0
		h.Gid = 0
		h.Uname = ""
		h.Gname = ""
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		// OpenRoot also confines reads if the tree changes during archiving.
		root, err := os.OpenRoot(dir)
		if err != nil {
			return err
		}
		defer func() { _ = root.Close() }()
		f, err := root.Open(name)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.CopyN(tw, f, info.Size())
		return err
	})
	if err != nil {
		_ = tw.Close()
		_ = gz.Close()
		return err
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return err
	}
	return gz.Close()
}

func (c *moduleCache) save() error {
	f, err := os.CreateTemp("", "ermite-gomod-archive-")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if err := archiveCache(c.dir, f); err != nil {
		return err
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"key": c.key, "version": cacheVersion, "cacheSize": size})
	resp, err := c.request("POST", c.base+cacheAPI+"caches", bytes.NewReader(body), true, nil)
	if err != nil {
		return err
	}
	var reservation struct {
		CacheID int64 `json:"cacheId"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&reservation)
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	if reservation.CacheID <= 0 {
		return errors.New("invalid cache reservation")
	}
	target := fmt.Sprintf("%s%scaches/%d", c.base, cacheAPI, reservation.CacheID)
	resp, err = c.request("PATCH", target, f, true, map[string]string{"Content-Type": "application/octet-stream", "Content-Range": fmt.Sprintf("bytes 0-%d/*", size-1)})
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	body, _ = json.Marshal(map[string]int64{"size": size})
	resp, err = c.request("POST", target, bytes.NewReader(body), true, nil)
	if err == nil {
		_ = resp.Body.Close()
	}
	return err
}

func prepareCache(ctx context.Context, r *execpb.DepsRequest, log io.Writer) *moduleCache {
	if r.CacheToken == "" || r.RegistryPort == 0 || r.CacheMode == "" {
		return nil
	}
	c := &moduleCache{ctx: ctx, client: actionscache.NewClient(&http.Client{Timeout: 2 * time.Minute}), base: fmt.Sprintf("http://127.0.0.1:%d", r.RegistryPort), token: r.CacheToken, key: cacheKey(r), log: log}
	dir, err := os.MkdirTemp("", "ermite-gomod-")
	if err != nil {
		c.warn()
		return nil
	}
	c.dir = dir
	if err := c.restore("gomod-" + r.GoVersion + "-"); err != nil {
		c.warn()
		_ = os.RemoveAll(dir)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil
		}
	}
	return c
}

func (c *moduleCache) finish(success bool, mode string) {
	if c == nil {
		return
	}
	defer func() { _ = os.RemoveAll(c.dir) }()
	if success && !c.exact && mode == "write" {
		if err := c.save(); err != nil {
			c.warn()
		}
	}
}
