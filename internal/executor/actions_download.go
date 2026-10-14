// SPDX-License-Identifier: MIT

package executor

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// Constants of ActionManager.DownloadRepositoryArchive (ActionManager.cs:1600-1760).
const (
	downloadAttempts = 3
	downloadTimeout  = 20 * time.Minute
	// maxDownloadBytes bounds an archive (the runner has no bound).
	maxDownloadBytes = 1 << 30
)

// noBackoffEnv is the runner's switch for tests (ActionManager.cs:1739).
const noBackoffEnv = "_GITHUB_ACTION_DOWNLOAD_NO_BACKOFF"

// errActionNotFound is ActionNotFoundException (ActionNotFoundException.cs:28).
type errActionNotFound struct{ url string }

func (e errActionNotFound) Error() string {
	return fmt.Sprintf("An action could not be found at the URI '%s'", e.url)
}

// errAccessDenied is AccessDeniedException; the request id is empty (ermite
// has none) and the runner still prints its parentheses.
type errAccessDenied struct{ url string }

func (e errAccessDenied) Error() string { return fmt.Sprintf("Access denied to '%s' ()", e.url) }

func (j *job) httpClient() *http.Client {
	if c := j.cfg.Actions.HTTPClient; c != nil {
		return c
	}
	return &http.Client{}
}

// sleepBackoff waits a random 10 to 30 seconds (BackoffTimerHelper.
// GetRandomBackoff), or what the test hook says.
func (j *job) sleepBackoff(ctx context.Context) time.Duration {
	if d := j.cfg.Actions.Backoff; d != nil {
		return d()
	}
	return time.Duration(10+rand.Int64N(21)) * time.Second
}

// downloadArchive saves the archive of an action in file, retrying like the
// runner: three attempts, a warning after each failure, a 404 or a 403 at once.
// The final error is the one of the last attempt, which is what the runner
// shows (PrepareActionsAsync prints the inner exception of
// FailedToDownloadActionException).
func (j *job) downloadArchive(ctx context.Context, num int, url, token, file string) error {
	var last error
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		err := j.fetch(ctx, url, token, file)
		if err == nil {
			return nil
		}
		last = err
		var nf errActionNotFound
		var ad errAccessDenied
		if ctx.Err() != nil || errors.As(err, &ad) {
			return err
		}
		if errors.As(err, &nf) {
			return err
		}
		if attempt == downloadAttempts {
			break
		}
		j.tagged(num, "##[warning]", fmt.Sprintf("Failed to download action '%s'. Error: %s ", url, err.Error()))
		if os.Getenv(noBackoffEnv) == "" {
			d := j.sleepBackoff(ctx)
			j.tagged(num, "##[warning]", fmt.Sprintf("Back off %v seconds before retry.", d.Seconds()))
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return last
}

func (j *job) fetch(ctx context.Context, url, token, file string) error {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)))
	}
	req.Header.Set("User-Agent", "ermite-runner")
	resp, err := j.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errActionNotFound{url}
	case resp.StatusCode == http.StatusForbidden:
		return errAccessDenied{url}
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		// HttpResponseMessage.EnsureSuccessStatusCode.
		return ghErrorf("Response status code does not indicate success: %d (%s).", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxDownloadBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxDownloadBytes {
		err = errors.New("the archive is larger than 1 GiB")
	}
	return err
}

// watermark is the file that says an action was downloaded
// (ActionManager.GetWatermarkFilePath).
func watermark(dir string) string { return dir + ".completed" }

// downloadAction downloads and unpacks one action into dir
// (ActionManager.DownloadRepositoryActionAsync): in a temporary directory, the
// archive's single top directory stripped, renamed to dir on success, and
// marked with the watermark.
func (j *job) downloadAction(ctx context.Context, num int, ra ResolvedAction, name, ref, dir string) error {
	if _, err := os.Stat(watermark(dir)); err == nil {
		j.debug(num, fmt.Sprintf("Action '%s@%s' already downloaded at '%s'.", name, ref, dir))
		return nil
	}
	j.out(num, fmt.Sprintf("Download action repository '%s@%s' (SHA:%s)", name, ref, ra.ResolvedSHA))

	actions := j.state.Layout().Actions
	tmp := filepath.Join(actions, "_temp_"+uuid.NewString())
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	archive := filepath.Join(tmp, uuid.NewString()+".tar.gz")
	if err := j.downloadArchive(ctx, num, ra.TarballURL, ra.Token, archive); err != nil {
		return err
	}
	staging := filepath.Join(tmp, "_staging")
	if err := os.Mkdir(staging, 0o755); err != nil {
		return err
	}
	af, err := os.Open(archive)
	if err != nil {
		return err
	}
	var owner *[2]int
	if c := j.cfg.Credential; c != nil {
		owner = &[2]int{int(c.Uid), int(c.Gid)}
	}
	// The top directory is stripped while extracting, so that links are checked
	// against the tree they will live in (see extractTarball).
	err = extractTarball(ctx, af, staging, owner)
	_ = af.Close()
	if err != nil {
		return ghErrorf("Can't extract the archive of action %s@%s: %v", name, ref, err)
	}
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if err := os.Rename(staging, dir); err != nil {
		return err
	}
	j.debug(num, fmt.Sprintf("Unwrap the archive to '%s'", dir))
	if err := os.WriteFile(watermark(dir), []byte(j.now().UTC().Format(time.DateTime)), 0o644); err != nil {
		return err
	}
	j.own(dir)
	_ = filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
		if err == nil {
			j.own(p)
		}
		return nil
	})
	return nil
}
