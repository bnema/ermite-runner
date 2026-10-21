// SPDX-License-Identifier: MIT

package registryproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type upstreamError struct{ status int }

func (e upstreamError) Error() string { return fmt.Sprintf("registry upstream status %d", e.status) }

// unavailable reports an upstream that cannot answer now (down, timing out,
// failing or rate limiting): a known tag may then be served stale.
func unavailable(err error) bool {
	var e upstreamError
	if errors.As(err, &e) {
		return e.status >= http.StatusInternalServerError || e.status == http.StatusTooManyRequests
	}
	var network net.Error
	return errors.As(err, &network) || errors.Is(err, context.DeadlineExceeded)
}

func proxyStatus(err error) int {
	var e upstreamError
	if errors.As(err, &e) && (e.status == http.StatusUnauthorized || e.status == http.StatusForbidden || e.status == http.StatusNotFound) {
		return http.StatusNotFound
	}
	return http.StatusBadGateway
}

type bearer struct {
	value string
	until time.Time
}

type upstream struct {
	base, auth               string
	client                   *http.Client
	blobClient               *http.Client
	requestTimeout, blobIdle time.Duration
	mu                       sync.Mutex
	tokens                   map[string]bearer
}

func (u *upstream) token(ctx context.Context, name string, refresh bool) (string, error) {
	// Do not hold the token map lock over network I/O: an unavailable auth
	// endpoint must not queue unrelated repositories behind its timeout.
	// No guest credentials or challenge-controlled URLs are accepted.
	u.mu.Lock()
	t, ok := u.tokens[name]
	u.mu.Unlock()
	if ok && !refresh && time.Now().Before(t.until) {
		return t.value, nil
	}
	a, err := url.Parse(u.auth)
	if err != nil {
		return "", err
	}
	q := a.Query()
	q.Set("service", "registry.docker.io")
	q.Set("scope", "repository:"+name+":pull")
	a.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", upstreamError{resp.StatusCode}
	}
	var data struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		Expires     int64  `json:"expires_in"`
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil {
		return "", err
	}
	if len(b) > 1<<20 {
		return "", errors.New("registry token response too large")
	}
	if err = json.Unmarshal(b, &data); err != nil {
		return "", err
	}
	if data.Token == "" {
		data.Token = data.AccessToken
	}
	if data.Token == "" {
		return "", errors.New("empty registry token")
	}
	if data.Expires <= 0 {
		data.Expires = 60
	}
	if data.Expires > 86400 {
		data.Expires = 86400
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.tokens) >= 1024 {
		clear(u.tokens)
	}
	u.tokens[name] = bearer{data.Token, time.Now().Add(time.Duration(data.Expires) * time.Second)}
	return data.Token, nil
}

// authorized calls send with a bearer token for name, and once more with a
// fresh token after a 401 or 403. Each token request is bounded by
// RequestTimeout (and ctx). It returns a 200 response; any other closes the
// body and is an upstreamError.
func (u *upstream) authorized(ctx context.Context, name string, send func(tok string) (*http.Response, error)) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		authCtx, cancel := context.WithTimeout(ctx, u.requestTimeout)
		tok, err := u.token(authCtx, name, attempt > 0)
		cancel()
		if err != nil {
			return nil, err
		}
		resp, err := send(tok)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		_ = resp.Body.Close()
		if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && attempt == 0 {
			continue
		}
		return nil, upstreamError{resp.StatusCode}
	}
	return nil, upstreamError{http.StatusUnauthorized}
}

// request is a metadata request (manifests, blob HEAD) bounded by ctx. Manifests
// use the client that never follows redirects; a blob HEAD uses the blob client,
// because Docker Hub answers it with a redirect to its CDN.
func (u *upstream) request(ctx context.Context, method, name string, k kind, ref, accept string) (*http.Response, error) {
	client := u.client
	if k == kindBlob {
		client = u.blobClient
	}
	return u.authorized(ctx, name, func(tok string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, u.base+"/v2/"+name+"/"+k.dir()+"/"+ref, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		// Do not forward guest Authorization, cookies, query parameters or Range.
		return client.Do(req)
	})
}
