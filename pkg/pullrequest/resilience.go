package pullrequest

import (
	"context"
	"errors"
	"net"
	"net/url"
	"regexp"
	"sync"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/circuitbreaker"
	"github.com/failsafe-go/failsafe-go/retrypolicy"
	forge "github.com/git-pkgs/forge"
)

// The same budget gitexec gives a CLI call: two retries within about a second, and a breaker per forge domain.
const (
	retryMaxRetries   = 2
	retryBackoffMin   = 100 * time.Millisecond
	retryBackoffMax   = time.Second
	retryJitterFactor = 0.2
	breakerFailures   = 5
	breakerDelay      = 30 * time.Second
)

// breakers holds one circuit breaker per forge domain, shared by every Finder in the process.
var breakers sync.Map

// statusInText finds an HTTP status worth retrying in an error's text. The GitHub and GitLab backends return their
// client libraries' errors, which carry the status only in their message, such as "GET https://...: 502 Bad Gateway".
var statusInText = regexp.MustCompile(`(^|[^0-9])(429|50[0234])([^0-9]|$)`)

// withRetry runs call under the domain's breaker and a retry policy for transient failures.
func withRetry[T any](ctx context.Context, domain string, call func() (T, error)) (T, error) {
	retry := retrypolicy.NewBuilder[T]().
		HandleIf(func(_ T, err error) bool { return transient(err) }).
		AbortOnErrors(context.Canceled).
		WithBackoff(retryBackoffMin, retryBackoffMax).
		WithJitterFactor(retryJitterFactor).
		WithMaxRetries(retryMaxRetries).
		ReturnLastFailure().
		Build()
	return failsafe.With[T](breakerFor[T](domain), retry).WithContext(ctx).Get(call)
}

func breakerFor[T any](domain string) circuitbreaker.CircuitBreaker[T] {
	if v, ok := breakers.Load(domain); ok {
		if cb, ok := v.(circuitbreaker.CircuitBreaker[T]); ok {
			return cb
		}
	}
	cb := circuitbreaker.NewBuilder[T]().
		HandleIf(func(_ T, err error) bool { return transient(err) }).
		WithFailureThreshold(breakerFailures).
		WithDelay(breakerDelay).
		Build()
	actual, _ := breakers.LoadOrStore(domain, cb)
	if out, ok := actual.(circuitbreaker.CircuitBreaker[T]); ok {
		return out
	}
	return cb
}

// transient reports whether err is worth another try: a timeout, a transport failure, a rate limit or a 5xx from the
// forge. A missing repository, bad credentials and other 4xx answers are not.
func transient(err error) bool {
	switch {
	case err == nil, errors.Is(err, context.Canceled), errors.Is(err, circuitbreaker.ErrOpen),
		errors.Is(err, forge.ErrNotFound), errors.Is(err, ErrMissingDeadline):
		return false
	case errors.Is(err, context.DeadlineExceeded):
		return true
	}
	var httpErr *forge.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == 429 || httpErr.StatusCode >= 500
	}
	var netErr net.Error
	var urlErr *url.Error
	if errors.As(err, &netErr) || errors.As(err, &urlErr) {
		return true
	}
	return statusInText.MatchString(err.Error())
}
