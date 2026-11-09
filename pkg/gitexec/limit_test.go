package gitexec

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
)

// gatedProcess is a fake attempt that counts processes in flight and
// blocks until the gate closes.
type gatedProcess struct {
	gate     chan struct{}
	started  chan string
	inFlight atomic.Int32
	maxSeen  atomic.Int32
}

func newGatedProcess(buffer int) *gatedProcess {
	return &gatedProcess{gate: make(chan struct{}), started: make(chan string, buffer)}
}

func (g *gatedProcess) run(ctx context.Context, _ time.Duration, _ string, args ...string) ([]byte, error) {
	n := g.inFlight.Add(1)
	defer g.inFlight.Add(-1)
	for {
		prev := g.maxSeen.Load()
		if n <= prev || g.maxSeen.CompareAndSwap(prev, n) {
			break
		}
	}
	label := ""
	if len(args) > 0 {
		label = args[len(args)-1]
	}
	g.started <- label
	select {
	case <-g.gate:
		return []byte("ok"), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func waitStarted(t *testing.T, g *gatedProcess, n int) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case <-g.started:
		case <-timeout:
			t.Fatalf("only %d of %d processes started", i, n)
		}
	}
}

func runConcurrently(t *testing.T, r *Runner, n int) *sync.WaitGroup {
	t.Helper()
	ctx := deadlineCtx(t, 10*time.Second)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Run(ctx, "git", "status"); err != nil {
				t.Errorf("Run: %v", err)
			}
		}()
	}
	return &wg
}

func TestMaxConcurrentCapsProcesses(t *testing.T) {
	const callers = 12
	g := newGatedProcess(callers)
	r := New()
	r.MaxConcurrent = 2
	r.attempt = g.run

	wg := runConcurrently(t, r, callers)
	waitStarted(t, g, 2)
	// Give queued callers a chance to slip past the cap if it were broken.
	time.Sleep(50 * time.Millisecond)
	if got := g.inFlight.Load(); got != 2 {
		t.Errorf("in flight while gated = %d, want 2", got)
	}
	close(g.gate)
	wg.Wait()

	if got := g.maxSeen.Load(); got > 2 {
		t.Fatalf("max concurrent processes = %d, want at most 2", got)
	}
}

func TestMaxConcurrentZeroIsUnlimited(t *testing.T) {
	const callers = 8
	g := newGatedProcess(callers)
	r := New()
	r.attempt = g.run

	wg := runConcurrently(t, r, callers)
	waitStarted(t, g, callers)
	close(g.gate)
	wg.Wait()

	if got := g.maxSeen.Load(); got != callers {
		t.Fatalf("max concurrent processes = %d, want %d", got, callers)
	}
}

func TestMaxConcurrentQueuedCallExpires(t *testing.T) {
	rec := installRecorder(t)
	g := newGatedProcess(4)
	r := New()
	r.MaxConcurrent = 1
	r.attempt = g.run

	holderCtx := deadlineCtx(t, 10*time.Second)
	holderDone := make(chan error, 1)
	go func() {
		_, err := r.Run(holderCtx, "git", "fetch", "holder")
		holderDone <- err
	}()
	waitStarted(t, g, 1)

	queuedCtx := deadlineCtx(t, 50*time.Millisecond)
	_, err := r.Run(queuedCtx, "git", "fetch", "queued")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued err = %v, want context.DeadlineExceeded", err)
	}

	close(g.gate)
	if err := <-holderDone; err != nil {
		t.Fatalf("holder: %v", err)
	}
	close(g.started)
	for label := range g.started {
		if label == "queued" {
			t.Fatal("queued call started a process after its ctx expired")
		}
	}

	var queued bool
	for _, s := range spansNamed(rec.Ended(), "git fetch attempt") {
		v, _ := spanAttr(s, "error.type")
		if v.AsString() != errTypeQueueWait {
			continue
		}
		queued = true
		if s.Status().Code != codes.Error {
			t.Errorf("queued attempt status = %v, want error", s.Status().Code)
		}
		wait, ok := spanAttr(s, attrQueueWaitMS)
		if !ok || wait.AsFloat64() <= 0 {
			t.Errorf("queued attempt wait = %v, want > 0", wait.String())
		}
		if _, ok := spanAttr(s, "process.exit.code"); ok {
			t.Errorf("queued attempt records an exit code for a process that never ran")
		}
	}
	if !queued {
		t.Error("no attempt span recorded the queue wait failure")
	}
}
