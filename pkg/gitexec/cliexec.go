package gitexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/failsafe-go/failsafe-go"
)

// Exec is the injectable CLI surface used by forge and local git callers.
type Exec interface {
	LookPath(name string) (string, error)
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	RunJSON(ctx context.Context, name string, args ...string) ([]byte, error)
}

// DefaultTimeout bounds each process attempt when Runner.Timeout is unset.
const DefaultTimeout = 45 * time.Second

// Runner executes external CLIs with a timeout, an optional concurrency cap
// and failsafe-go resilience. Each call starts an OpenTelemetry span on the
// global tracer provider (see TracerName); the runner never installs one.
//
// Set the exported fields before the first Run. Copies of a runner built
// with New, or of one that has already run, share its process slots, retry
// policy and circuit breakers.
type Runner struct {
	// Timeout bounds each process attempt. Zero or negative uses DefaultTimeout.
	Timeout time.Duration
	// MaxConcurrent caps how many processes this runner runs at once. Calls
	// over the cap queue on the caller's ctx before the process starts.
	// Zero or negative means unlimited.
	MaxConcurrent int
	attempt       func(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error)
	resilience    *runnerResilience
	slots         *slotPool
}

var _ Exec = (*Runner)(nil)

// New returns a runner with DefaultTimeout and no concurrency cap.
func New() *Runner {
	return &Runner{Timeout: DefaultTimeout, resilience: newRunnerResilience(), slots: &slotPool{}}
}

// LookPath reports whether name is on PATH.
func (r *Runner) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}

// RunJSON runs argv and expects JSON on stdout.
func (r *Runner) RunJSON(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := r.Run(ctx, name, args...)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return []byte("[]"), nil
	}
	return []byte(trimmed), nil
}

// Run executes a command and returns stdout bytes.
// Requires ctx to have a deadline (fail closed). When MaxConcurrent is set,
// each attempt waits for a process slot with ctx; a ctx that ends while
// queued returns ctx.Err() wrapped, and the process never starts.
func (r *Runner) Run(ctx context.Context, name string, args ...string) (out []byte, err error) {
	if r == nil {
		r = New()
	}
	if ctx == nil {
		return nil, ErrMissingDeadline
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, ErrMissingDeadline
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	attempt := r.attempt
	if attempt == nil {
		attempt = runAttempt
	}
	limit := r.MaxConcurrent
	rr, slots := r.internals()
	cmd := describeCommand(name, args)

	callCtx, span := startCall(ctx, cmd)
	var attempts atomic.Int64
	defer func() {
		span.SetAttributes(attrAttempts.Int64(attempts.Load()))
		finishSpan(callCtx, span, cmd, err)
		span.End()
	}()

	out, err = rr.run(callCtx, name, func(ex failsafe.Execution[[]byte]) ([]byte, error) {
		attempts.Store(int64(ex.Attempts()))
		return runTracedAttempt(ex.Context(), tracedAttempt{
			cmd:     cmd,
			number:  ex.Attempts(),
			limit:   limit,
			slots:   slots,
			timeout: timeout,
			run:     attempt,
			name:    name,
			args:    args,
		})
	})
	return out, err
}

// tracedAttempt is one process execution inside a logical Run call.
type tracedAttempt struct {
	cmd     command
	number  int
	limit   int
	slots   *slotPool
	timeout time.Duration
	run     func(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error)
	name    string
	args    []string
}

// runTracedAttempt waits for a process slot, then runs the process under a
// client span that records the attempt number, queue wait and outcome.
func runTracedAttempt(ctx context.Context, a tracedAttempt) (out []byte, err error) {
	ctx, span := startAttempt(ctx, a.cmd, a.number, a.limit)
	defer func() {
		finishSpan(ctx, span, a.cmd, err)
		span.End()
	}()

	queued := time.Now()
	release, err := a.slots.acquire(ctx, a.name, a.limit)
	recordQueueWait(span, time.Since(queued))
	if err != nil {
		return nil, err
	}
	defer release()
	out, err = a.run(ctx, a.timeout, a.name, a.args...)
	return out, err
}

func runAttempt(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, name, args...) //nolint:gosec // argv are caller-controlled CLI tools
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		cause := clarifyProcessError(runCtx, timeout, err)
		if msg == "" || msg == err.Error() {
			return nil, fmt.Errorf("%s %s: %w", name, redactArgs(args), cause)
		}
		return nil, fmt.Errorf("%s %s: %s: %w", name, redactArgs(args), redactOutput(msg), cause)
	}
	return stdout.Bytes(), nil
}

func clarifyProcessError(runCtx context.Context, timeout time.Duration, err error) error {
	if err == nil {
		return nil
	}
	if runCtx != nil && runCtx.Err() != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("timed out after %s: %w", timeout, err)
		}
		return fmt.Errorf("canceled: %w", err)
	}
	if isSignalKilled(err) {
		return fmt.Errorf("process killed (timeout, cancel, or OOM): %w", err)
	}
	return err
}

func isSignalKilled(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "signal: killed")
}

func redactArgs(args []string) string {
	return strings.Join(args, " ")
}

func redactOutput(msg string) string {
	return msg
}
