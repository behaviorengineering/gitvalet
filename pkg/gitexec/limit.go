package gitexec

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sync/semaphore"
)

// errSlotWait marks a call that gave up while queued for a process slot.
// The wrapped cause is the caller's ctx.Err().
var errSlotWait = errors.New("gitexec: gave up waiting for a process slot")

// slotPool caps how many processes a Runner (and its copies) runs at once.
type slotPool struct {
	mu   sync.Mutex
	size int64
	sem  *semaphore.Weighted
}

// semaphoreFor returns the semaphore for limit, replacing it when the limit
// changed since the last call. Holders of an old semaphore release to it.
func (p *slotPool) semaphoreFor(limit int64) *semaphore.Weighted {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sem == nil || p.size != limit {
		p.sem = semaphore.NewWeighted(limit)
		p.size = limit
	}
	return p.sem
}

// acquire waits for a process slot with ctx. A limit of zero or less means
// unlimited and never blocks. The returned release func MUST be called once
// the process has exited. A ctx that ends while queued returns ctx.Err()
// wrapped in errSlotWait, and the caller MUST NOT start the process.
func (p *slotPool) acquire(ctx context.Context, name string, limit int) (func(), error) {
	if limit <= 0 {
		return func() {}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w for %s: %w", errSlotWait, name, err)
	}
	sem := p.semaphoreFor(int64(limit))
	if err := sem.Acquire(ctx, 1); err != nil {
		return nil, fmt.Errorf("%w for %s: %w", errSlotWait, name, err)
	}
	return func() { sem.Release(1) }, nil
}
