package gitexec

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// Credential-bearing argv values that must never reach a span.
const (
	secretHeader = "Authorization: Bearer ghp_s3cretT0ken"
	secretURL    = "https://alice:hunter2pass@example.com/org/repo.git" //nolint:gosec // fake credential fixture for the leak test
	secretValue  = "hunter2pass"
	secretToken  = "ghp_s3cretT0ken"
)

// installRecorder installs an in-memory span recorder as the global tracer
// provider and restores the previous provider when the test ends.
// Tests that call it MUST NOT run in parallel.
func installRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})
	return rec
}

func deadlineCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func spanAttr(s sdktrace.ReadOnlySpan, key attribute.Key) (attribute.Value, bool) {
	for _, kv := range s.Attributes() {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func spansNamed(spans []sdktrace.ReadOnlySpan, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

func oneSpan(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	got := spansNamed(spans, name)
	if len(got) != 1 {
		names := make([]string, 0, len(spans))
		for _, s := range spans {
			names = append(names, s.Name())
		}
		t.Fatalf("want one span %q, got %d (all spans: %v)", name, len(got), names)
	}
	return got[0]
}

// assertNoSecrets fails when any attribute, event attribute or status of
// any span mentions a credential-bearing argv value.
func assertNoSecrets(t *testing.T, spans []sdktrace.ReadOnlySpan, secrets ...string) {
	t.Helper()
	check := func(where, text string) {
		for _, secret := range secrets {
			if strings.Contains(text, secret) {
				t.Errorf("%s leaks %q: %q", where, secret, text)
			}
		}
	}
	for _, s := range spans {
		check("span name", s.Name())
		check("status of "+s.Name(), s.Status().Description)
		for _, kv := range s.Attributes() {
			check(fmt.Sprintf("%s attribute %s", s.Name(), kv.Key), kv.Value.String())
		}
		for _, ev := range s.Events() {
			for _, kv := range ev.Attributes {
				check(fmt.Sprintf("%s event %s attribute %s", s.Name(), ev.Name, kv.Key), kv.Value.String())
			}
		}
	}
}

type fakeExitError struct{ code int }

func (e fakeExitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e fakeExitError) ExitCode() int { return e.code }

func TestRunSpanSuccess(t *testing.T) {
	rec := installRecorder(t)
	r := New()
	r.attempt = func(context.Context, time.Duration, string, ...string) ([]byte, error) {
		return []byte("[]"), nil
	}
	ctx := deadlineCtx(t, time.Second)

	if _, err := r.RunJSON(ctx, "gh", "pr", "list", "--repo", "org/repo", "--json", "number"); err != nil {
		t.Fatalf("RunJSON: %v", err)
	}

	spans := rec.Ended()
	call := oneSpan(t, spans, "gh pr list")
	att := oneSpan(t, spans, "gh pr list attempt")
	if call.SpanKind() != trace.SpanKindInternal {
		t.Errorf("call span kind = %v, want internal", call.SpanKind())
	}
	if att.SpanKind() != trace.SpanKindClient {
		t.Errorf("attempt span kind = %v, want client", att.SpanKind())
	}
	if att.Parent().SpanID() != call.SpanContext().SpanID() {
		t.Errorf("attempt span is not a child of the call span")
	}
	if got := att.InstrumentationScope().Name; got != TracerName {
		t.Errorf("tracer name = %q, want %q", got, TracerName)
	}
	for key, want := range map[attribute.Key]string{
		"process.executable.name": "gh",
		attrSubcommand:            "pr list",
	} {
		if v, ok := spanAttr(att, key); !ok || v.AsString() != want {
			t.Errorf("attempt %s = %q, want %q", key, v.String(), want)
		}
	}
	if v, ok := spanAttr(att, "process.working_directory"); !ok || v.AsString() == "" {
		t.Errorf("attempt process.working_directory missing")
	}
	if v, ok := spanAttr(att, attrAttempt); !ok || v.AsInt64() != 1 {
		t.Errorf("attempt number = %v, want 1", v.String())
	}
	if _, ok := spanAttr(att, attrQueueWaitMS); !ok {
		t.Errorf("attempt queue wait missing")
	}
	if v, ok := spanAttr(att, "process.exit.code"); !ok || v.AsInt64() != 0 {
		t.Errorf("attempt exit code = %v, want 0", v.String())
	}
	if v, ok := spanAttr(call, attrAttempts); !ok || v.AsInt64() != 1 {
		t.Errorf("call attempts = %v, want 1", v.String())
	}
	if call.Status().Code == codes.Error || att.Status().Code == codes.Error {
		t.Errorf("unexpected error status")
	}
}

func TestRunSpanNonZeroExitWithoutCredentials(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	rec := installRecorder(t)
	// Offline: rev-parse fails locally with exit 128 and never contacts a remote.
	dir := t.TempDir()
	r := New()
	ctx := deadlineCtx(t, 10*time.Second)

	_, err = r.Run(ctx, gitPath,
		"-c", "http.extraHeader="+secretHeader,
		"-C", dir,
		"rev-parse", "--verify", secretURL, "--token="+secretToken,
	)
	if err == nil {
		t.Fatal("expected git rev-parse to fail")
	}

	spans := rec.Ended()
	att := oneSpan(t, spans, "git rev-parse attempt")
	call := oneSpan(t, spans, "git rev-parse")
	if v, _ := spanAttr(att, "process.working_directory"); v.AsString() != dir {
		t.Errorf("working directory = %q, want %q", v.AsString(), dir)
	}
	if v, ok := spanAttr(att, "process.exit.code"); !ok || v.AsInt64() == 0 {
		t.Errorf("exit code = %v, want non-zero", v.String())
	}
	if v, _ := spanAttr(att, "error.type"); v.AsString() != errTypeExitStatus {
		t.Errorf("error.type = %q, want %q", v.AsString(), errTypeExitStatus)
	}
	for _, s := range []sdktrace.ReadOnlySpan{att, call} {
		if s.Status().Code != codes.Error {
			t.Errorf("%s status = %v, want error", s.Name(), s.Status().Code)
		}
		if len(s.Events()) == 0 || s.Events()[0].Name != "exception" {
			t.Errorf("%s has no recorded error", s.Name())
		}
	}
	assertNoSecrets(t, spans, secretHeader, secretURL, secretValue, secretToken, "Authorization", "alice")
}

func TestRunSpanRetryAttemptsAreChildren(t *testing.T) {
	rec := installRecorder(t)
	r := New()
	var calls atomic.Int32
	r.attempt = func(context.Context, time.Duration, string, ...string) ([]byte, error) {
		if calls.Add(1) == 1 {
			return nil, fmt.Errorf("git fetch: timed out after 1s: %w", fakeExitError{code: -1})
		}
		return []byte("ok"), nil
	}
	ctx := deadlineCtx(t, 5*time.Second)

	if _, err := r.Run(ctx, "git", "fetch", "origin"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	spans := rec.Ended()
	call := oneSpan(t, spans, "git fetch")
	attempts := spansNamed(spans, "git fetch attempt")
	if len(attempts) != 2 {
		t.Fatalf("attempt spans = %d, want 2", len(attempts))
	}
	for i, att := range attempts {
		if att.Parent().SpanID() != call.SpanContext().SpanID() {
			t.Errorf("attempt %d is not a child of the call span", i+1)
		}
		if v, _ := spanAttr(att, attrAttempt); v.AsInt64() != int64(i+1) {
			t.Errorf("attempt %d number = %d", i+1, v.AsInt64())
		}
	}
	if attempts[0].Status().Code != codes.Error {
		t.Errorf("first attempt status = %v, want error", attempts[0].Status().Code)
	}
	if v, _ := spanAttr(attempts[0], "error.type"); v.AsString() != errTypeTimeout {
		t.Errorf("first attempt error.type = %q, want timeout", v.AsString())
	}
	if call.Status().Code == codes.Error {
		t.Errorf("call span status = error after a successful retry")
	}
	if v, _ := spanAttr(call, attrAttempts); v.AsInt64() != 2 {
		t.Errorf("call attempts = %d, want 2", v.AsInt64())
	}
}

func TestRunSpanCancelled(t *testing.T) {
	rec := installRecorder(t)
	r := New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.attempt = func(ctx context.Context, _ time.Duration, _ string, _ ...string) ([]byte, error) {
		cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	}

	_, err := r.Run(ctx, "glab", "mr", "list")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	call := oneSpan(t, rec.Ended(), "glab mr list")
	if call.Status().Code != codes.Error {
		t.Errorf("status = %v, want error", call.Status().Code)
	}
	if v, _ := spanAttr(call, "error.type"); v.AsString() != errTypeCancelled {
		t.Errorf("error.type = %q, want cancelled", v.AsString())
	}
}

func TestRunSpanTimeout(t *testing.T) {
	rec := installRecorder(t)
	r := New()
	r.attempt = func(ctx context.Context, _ time.Duration, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx := deadlineCtx(t, 50*time.Millisecond)

	_, err := r.Run(ctx, "az", "repos", "pr", "list", "--org", secretURL)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	spans := rec.Ended()
	call := oneSpan(t, spans, "az repos pr list")
	if v, _ := spanAttr(call, "error.type"); v.AsString() != errTypeTimeout {
		t.Errorf("error.type = %q, want timeout", v.AsString())
	}
	if call.Status().Code != codes.Error {
		t.Errorf("status = %v, want error", call.Status().Code)
	}
	assertNoSecrets(t, spans, secretURL, secretValue)
}

func TestRunSpanBreakerOpen(t *testing.T) {
	rec := installRecorder(t)
	r := New()
	var calls atomic.Int32
	r.attempt = func(context.Context, time.Duration, string, ...string) ([]byte, error) {
		calls.Add(1)
		return []byte("ok"), nil
	}
	rr, _ := r.internals()
	rr.breakerFor("gh").Open()
	ctx := deadlineCtx(t, time.Second)

	_, err := r.Run(ctx, "gh", "api", "repos/org/repo")
	if err == nil {
		t.Fatal("expected breaker-open error")
	}
	if calls.Load() != 0 {
		t.Errorf("process ran %d times with the breaker open", calls.Load())
	}
	spans := rec.Ended()
	call := oneSpan(t, spans, "gh api")
	if v, _ := spanAttr(call, "error.type"); v.AsString() != errTypeBreakerOpen {
		t.Errorf("error.type = %q, want breaker_open", v.AsString())
	}
	if call.Status().Code != codes.Error {
		t.Errorf("status = %v, want error", call.Status().Code)
	}
	if n := len(spansNamed(spans, "gh api attempt")); n != 0 {
		t.Errorf("attempt spans = %d, want 0", n)
	}
}

func TestDescribeCommand(t *testing.T) {
	cases := []struct {
		name string
		exe  string
		args []string
		want string
	}{
		{"git subcommand", "git", []string{"fetch", "origin"}, "fetch"},
		{"git globals skipped", "/usr/bin/git", []string{"-c", "http.extraHeader=" + secretHeader, "-C", "/tmp/x", "rev-parse", "HEAD"}, "rev-parse"},
		{"gh two words", "gh", []string{"pr", "list", "--json", "number"}, "pr list"},
		{"gh api path stops", "gh", []string{"api", "repos/org/repo"}, "api"},
		{"az three words", "az", []string{"repos", "pr", "list", "--org", secretURL}, "repos pr list"},
		{"url never a word", "git", []string{secretURL}, ""},
		{"unknown flag value stops", "git", []string{"--weird", "Secret:Value", "fetch"}, ""},
		{"flag after word stops", "gh", []string{"auth", "--with-token", "login"}, "auth"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := describeCommand(tc.exe, tc.args)
			if got.subcommand != tc.want {
				t.Errorf("subcommand = %q, want %q", got.subcommand, tc.want)
			}
		})
	}
}
