// SPDX-License-Identifier: MIT

package registryproxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// upstreamClients returns the metadata client (auth, manifests: a total
// timeout and no redirects) and the blob client (no total timeout, a response
// header timeout, and redirects that drop the bearer when leaving the host).
func upstreamClients(requestTimeout time.Duration) (metadata, blobs *http.Client) {
	metadata = &http.Client{
		Timeout:       requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	blobs = &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many registry redirects")
			}
			if len(via) > 0 && req.URL.Host != via[0].URL.Host {
				req.Header.Del("Authorization")
			}
			return nil
		},
	}
	if tr, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = tr.Clone()
		tr.ResponseHeaderTimeout = requestTimeout
		blobs.Transport = tr
	}
	return metadata, blobs
}

// idleBody serializes timer reset/stop against expiry. Cancellation interrupts
// even a blocked transport read; only completed, nonempty reads renew the lease.
type idleBody struct {
	io.ReadCloser
	mu      sync.Mutex
	timer   *time.Timer
	cancel  context.CancelFunc
	idle    time.Duration
	expired bool
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.mu.Lock()
		if !b.expired {
			b.timer.Reset(b.idle)
		}
		b.mu.Unlock()
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.mu.Lock()
	b.expired = true
	b.timer.Stop()
	b.mu.Unlock()
	b.cancel()
	return b.ReadCloser.Close()
}

// blob GETs a blob. Each attempt has only a header deadline (RequestTimeout)
// and then a body progress deadline (BlobIdleTimeout), and no total transfer
// deadline: the response body is an idleBody that owns the attempt's context.
func (u *upstream) blob(ctx context.Context, name, digest string) (*http.Response, error) {
	return u.authorized(ctx, name, func(tok string) (*http.Response, error) {
		bodyCtx, cancel := context.WithCancel(ctx)
		lease := &idleBody{cancel: cancel, idle: u.blobIdle}
		lease.timer = time.AfterFunc(u.requestTimeout, func() { lease.mu.Lock(); lease.expired = true; cancel(); lease.mu.Unlock() })
		req, err := http.NewRequestWithContext(bodyCtx, http.MethodGet, u.base+"/v2/"+name+"/"+kindBlob.dir()+"/"+digest, nil)
		if err != nil {
			lease.timer.Stop()
			cancel()
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := u.blobClient.Do(req)
		if err != nil {
			lease.timer.Stop()
			cancel()
			return nil, err
		}
		lease.ReadCloser = resp.Body
		resp.Body = lease
		if resp.StatusCode == http.StatusOK {
			lease.mu.Lock()
			if !lease.expired {
				lease.timer.Reset(u.blobIdle)
			}
			lease.mu.Unlock()
		}
		return resp, nil
	})
}
