// SPDX-License-Identifier: MIT
package registryproxy

import (
	"context"
	"sync/atomic"
)

// Quota bounds the cold blob downloads started on behalf of one client, such as
// the channel of one sandbox, so that no client can take every download slot of
// the Proxy. A download counts against the quota of the request that started it
// until it really ends, even when only other clients still wait for it;
// requests that join a download in progress take none.
type Quota struct {
	max  int64
	used atomic.Int64
}

// NewQuota returns a quota of n concurrent downloads; n must be positive.
func NewQuota(n int) *Quota { return &Quota{max: int64(max(n, 1))} }

// WithQuota attaches q to the context of a request to the Proxy. A request
// without a quota is bound by the Proxy's global cap only.
func WithQuota(ctx context.Context, q *Quota) context.Context {
	return context.WithValue(ctx, quotaKey{}, q)
}

type quotaKey struct{}

// QuotaOf returns the quota attached by WithQuota, or nil.
func QuotaOf(ctx context.Context) *Quota { return quotaFrom(ctx) }

func quotaFrom(ctx context.Context) *Quota {
	q, _ := ctx.Value(quotaKey{}).(*Quota)
	return q
}

func (q *Quota) tryAcquire() bool {
	for {
		n := q.used.Load()
		if n >= q.max {
			return false
		}
		if q.used.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

func (q *Quota) release() { q.used.Add(-1) }
