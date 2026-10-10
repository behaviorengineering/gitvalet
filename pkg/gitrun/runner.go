// Package gitrun executes authenticated git subprocesses with context deadlines.
package gitrun

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/behaviorengineering/gitvalet/pkg/auth"
)

// Runner runs git with optional auth headers and working directory.
type Runner struct {
	Cred auth.Credential
}

// Run executes git in dir with auth configuration prepended.
func (r Runner) Run(ctx context.Context, dir string, args ...string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var cmdArgs []string
	cmdArgs = append(cmdArgs, auth.GitConfigArgs(r.Cred)...)
	if strings.TrimSpace(dir) != "" {
		cmdArgs = append(cmdArgs, "-C", dir)
	}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...) //nolint:gosec // git argv are validated git subcommands
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

// RunTrim is Run with trimmed stdout.
func (r Runner) RunTrim(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := r.Run(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// AllowFail runs git and returns stdout with exit code.
func (r Runner) AllowFail(ctx context.Context, dir string, args ...string) (string, int) {
	if ctx == nil {
		ctx = context.Background()
	}
	var cmdArgs []string
	cmdArgs = append(cmdArgs, auth.GitConfigArgs(r.Cred)...)
	if strings.TrimSpace(dir) != "" {
		cmdArgs = append(cmdArgs, "-C", dir)
	}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...) //nolint:gosec // git argv are validated git subcommands
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return strings.TrimSpace(stdout.String()), 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return stdout.String(), ee.ExitCode()
	}
	return stdout.String(), 1
}

// IsRepo reports whether dir is inside a git work tree.
func (r Runner) IsRepo(dir string) bool {
	_, code := r.AllowFail(context.Background(), dir, "rev-parse", "--is-inside-work-tree")
	return code == 0
}
