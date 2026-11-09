package gitexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/behaviorengineering/olly/pkg/olly"
	"github.com/failsafe-go/failsafe-go/circuitbreaker"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

// TracerName is the instrumentation scope of the spans this package starts.
const TracerName = "github.com/behaviorengineering/gitvalet/pkg/gitexec"

// Span attribute keys owned by gitexec. Process keys come from semconv.
const (
	attrSubcommand    = attribute.Key("gitexec.subcommand")
	attrAttempt       = attribute.Key("gitexec.attempt")
	attrAttempts      = attribute.Key("gitexec.attempts")
	attrQueueWaitMS   = attribute.Key("gitexec.queue.wait_ms")
	attrMaxConcurrent = attribute.Key("gitexec.max_concurrent")
)

// Values for the error.type span attribute.
const (
	errTypeBreakerOpen = "breaker_open"
	errTypeQueueWait   = "queue_wait"
	errTypeCancelled   = "cancelled"
	errTypeTimeout     = "timeout"
	errTypeExitStatus  = "exit_status"
	errTypeNotFound    = "not_found"
	errTypeKilled      = "killed"
	errTypeOther       = "error"
)

const maxCommandWordLen = 32

// subcommandWords is how many leading command words each CLI may contribute
// to the span name. Unlisted executables contribute one.
var subcommandWords = map[string]int{
	"git":  1,
	"gh":   2,
	"glab": 2,
	"az":   3,
}

// valueFlags are options whose value is the next argv word. Their values
// are skipped, never recorded, because they can carry credentials
// (git -c http.extraHeader=Authorization: ...).
var valueFlags = map[string]bool{
	"-C":             true,
	"-c":             true,
	"--git-dir":      true,
	"--work-tree":    true,
	"--namespace":    true,
	"--config-env":   true,
	"--super-prefix": true,
	"-R":             true,
	"--repo":         true,
}

// command is the span-safe description of one CLI call. It never holds the
// full argv, environment, stdin or output.
type command struct {
	exe        string
	subcommand string
	dir        string
}

// describeCommand derives the executable name, leading subcommand words and
// working directory from argv without keeping any argument values.
func describeCommand(name string, args []string) command {
	exe := filepath.Base(strings.TrimSpace(name))
	if exe == "." || exe == string(filepath.Separator) {
		exe = "unknown"
	}
	limit := subcommandWords[exe]
	if limit <= 0 {
		limit = 1
	}
	words, gitDir := scanArgs(exe, args, limit)
	return command{exe: exe, subcommand: strings.Join(words, " "), dir: workingDir(gitDir)}
}

// scanArgs returns up to limit leading command words and the git -C
// directory. Flags and their values before the first word are skipped; the
// first flag after a word, or any word that is not a plain command word,
// ends the scan.
func scanArgs(exe string, args []string, limit int) ([]string, string) {
	var words []string
	gitDir := ""
	for i := 0; i < len(args) && len(words) < limit; i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			if !isCommandWord(arg) {
				break
			}
			words = append(words, arg)
			continue
		}
		if len(words) > 0 {
			break
		}
		if !valueFlags[arg] || i+1 >= len(args) {
			continue
		}
		i++
		if exe == "git" && arg == "-C" {
			gitDir = joinGitDir(gitDir, args[i])
		}
	}
	return words, gitDir
}

// joinGitDir applies one git -C value, which resolves against earlier ones.
func joinGitDir(current, next string) string {
	if current == "" || filepath.IsAbs(next) {
		return next
	}
	return filepath.Join(current, next)
}

// workingDir is the directory the process runs in: git -C when given,
// otherwise this process's working directory (Runner never sets cmd.Dir).
func workingDir(gitDir string) string {
	if filepath.IsAbs(gitDir) {
		return gitDir
	}
	cwd, err := os.Getwd()
	if err != nil {
		return gitDir
	}
	if gitDir == "" {
		return cwd
	}
	return filepath.Join(cwd, gitDir)
}

// isCommandWord accepts short lowercase words such as fetch, rev-parse or pr.
// Anything else (paths, URLs, refs, key=value pairs) stops the scan.
func isCommandWord(s string) bool {
	if s == "" || len(s) > maxCommandWordLen {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z':
		case i > 0 && (c >= '0' && c <= '9' || c == '-'):
		default:
			return false
		}
	}
	return true
}

func (c command) label() string {
	if c.subcommand == "" {
		return c.exe
	}
	return c.exe + " " + c.subcommand
}

func (c command) attributes() []attribute.KeyValue {
	attrs := []attribute.KeyValue{semconv.ProcessExecutableName(c.exe)}
	if c.subcommand != "" {
		attrs = append(attrs, attrSubcommand.String(c.subcommand))
	}
	if c.dir != "" {
		attrs = append(attrs, semconv.ProcessWorkingDirectory(c.dir))
	}
	return attrs
}

func tracer() trace.Tracer {
	return olly.Tracer(TracerName)
}

// startCall starts the span for one logical call that may span several attempts.
func startCall(ctx context.Context, c command) (context.Context, trace.Span) {
	return tracer().Start(ctx, c.label(),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(c.attributes()...),
	)
}

// startAttempt starts the client span for one process execution.
func startAttempt(ctx context.Context, c command, attempt, maxConcurrent int) (context.Context, trace.Span) {
	attrs := append(c.attributes(), attrAttempt.Int(attempt))
	if maxConcurrent > 0 {
		attrs = append(attrs, attrMaxConcurrent.Int(maxConcurrent))
	}
	return tracer().Start(ctx, c.label()+" attempt",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)
}

func recordQueueWait(span trace.Span, wait time.Duration) {
	span.SetAttributes(attrQueueWaitMS.Float64(float64(wait) / float64(time.Millisecond)))
}

// finishSpan records the outcome on span. The recorded error is rebuilt
// from the executable, subcommand and failure class only: err itself carries
// argv and stderr, which can hold credentials.
func finishSpan(ctx context.Context, span trace.Span, c command, err error) {
	if code, ok := exitCode(err); ok {
		span.SetAttributes(semconv.ProcessExitCode(code))
	}
	if err == nil {
		return
	}
	errType, detail := classify(ctx, err)
	safe := fmt.Errorf("%s: %s", c.label(), detail)
	span.SetAttributes(semconv.ErrorTypeKey.String(errType))
	span.RecordError(safe)
	span.SetStatus(codes.Error, safe.Error())
}

// exitCode reports the process exit code: zero on success, the code from
// an *exec.ExitError (or any error with ExitCode) on failure.
func exitCode(err error) (int, bool) {
	if err == nil {
		return 0, true
	}
	var coder interface{ ExitCode() int }
	if errors.As(err, &coder) {
		if code := coder.ExitCode(); code >= 0 {
			return code, true
		}
	}
	return 0, false
}

// classify maps err to an error.type value and a credential-free detail.
func classify(ctx context.Context, err error) (string, string) {
	ctxErr := ctx.Err()
	switch {
	case errors.Is(err, circuitbreaker.ErrOpen):
		return errTypeBreakerOpen, "circuit breaker open"
	case errors.Is(err, errSlotWait):
		return errTypeQueueWait, "gave up waiting for a process slot: " + causeText(err)
	case errors.Is(err, context.Canceled) || errors.Is(ctxErr, context.Canceled):
		return errTypeCancelled, "cancelled"
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctxErr, context.DeadlineExceeded) ||
		strings.Contains(err.Error(), "timed out after"):
		return errTypeTimeout, "timed out"
	case errors.Is(err, exec.ErrNotFound):
		return errTypeNotFound, "executable not found"
	}
	if code, ok := exitCode(err); ok {
		return errTypeExitStatus, fmt.Sprintf("exit status %d", code)
	}
	if isSignalKilled(err) || strings.Contains(err.Error(), "process killed") {
		return errTypeKilled, "process killed"
	}
	return errTypeOther, "process failed"
}

// causeText names the context error behind a queue wait without echoing err.
func causeText(err error) string {
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return "deadline exceeded"
}
