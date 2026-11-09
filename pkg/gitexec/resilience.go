package gitexec

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/circuitbreaker"
	"github.com/failsafe-go/failsafe-go/retrypolicy"
)

const (
	retryMaxRetries   = 2
	retryBackoffMin   = 100 * time.Millisecond
	retryBackoffMax   = time.Second
	retryJitterFactor = 0.2
	breakerFailures   = 5
	breakerDelay      = 30 * time.Second
)

// ErrMissingDeadline is returned when Run is called with a context that has no deadline.
var ErrMissingDeadline = errors.New("gitexec: missing deadline")

type runnerResilience struct {
	retry    retrypolicy.RetryPolicy[[]byte]
	breakers sync.Map
}

func newRunnerResilience() *runnerResilience {
	return &runnerResilience{
		retry: retrypolicy.NewBuilder[[]byte]().
			HandleIf(func(_ []byte, err error) bool { return isTransientCLIError(err) }).
			AbortOnErrors(context.Canceled).
			AbortIf(func(_ []byte, err error) bool {
				return err != nil && errors.Is(err, circuitbreaker.ErrOpen)
			}).
			WithBackoff(retryBackoffMin, retryBackoffMax).
			WithJitterFactor(retryJitterFactor).
			WithMaxRetries(retryMaxRetries).
			ReturnLastFailure().
			Build(),
	}
}

func (rr *runnerResilience) breakerFor(name string) circuitbreaker.CircuitBreaker[[]byte] {
	key := strings.TrimSpace(name)
	if key == "" {
		key = "unknown"
	}
	if v, ok := rr.breakers.Load(key); ok {
		cb, ok := v.(circuitbreaker.CircuitBreaker[[]byte])
		if ok {
			return cb
		}
	}
	cb := circuitbreaker.NewBuilder[[]byte]().
		HandleIf(func(_ []byte, err error) bool { return isTransientCLIError(err) }).
		WithFailureThreshold(breakerFailures).
		WithDelay(breakerDelay).
		Build()
	actual, _ := rr.breakers.LoadOrStore(key, cb)
	out, ok := actual.(circuitbreaker.CircuitBreaker[[]byte])
	if !ok {
		return cb
	}
	return out
}

// lazyInit guards the lazy creation of Runner internals for zero-value
// runners shared across goroutines.
var lazyInit sync.Mutex

// internals returns the resilience policies and the process slot pool,
// creating them on first use for runners not built with New.
func (r *Runner) internals() (*runnerResilience, *slotPool) {
	lazyInit.Lock()
	defer lazyInit.Unlock()
	if r.resilience == nil {
		r.resilience = newRunnerResilience()
	}
	if r.slots == nil {
		r.slots = &slotPool{}
	}
	return r.resilience, r.slots
}

// run executes once under the breaker for name and the retry policy.
// The caller has already checked that ctx carries a deadline.
func (rr *runnerResilience) run(
	ctx context.Context,
	name string,
	once func(exec failsafe.Execution[[]byte]) ([]byte, error),
) ([]byte, error) {
	return failsafe.With(rr.breakerFor(name), rr.retry).
		WithContext(ctx).
		GetWithExecution(once)
}

func isTransientCLIError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, circuitbreaker.ErrOpen) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, ErrMissingDeadline) {
		return false
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "timed out after"):
		return true
	case strings.Contains(msg, "process killed"):
		return true
	case strings.Contains(msg, "signal: killed"):
		return true
	default:
		return false
	}
}
