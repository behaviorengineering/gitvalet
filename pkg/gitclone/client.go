// Package gitclone provides pinned local checkouts backed by git-pkgs/clone.
package gitclone

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/git-pkgs/clone"

	"github.com/behaviorengineering/gitvalet/pkg/auth"
)

// Client materializes exact commit SHAs into local directories.
type Client struct {
	Cred auth.Credential
	mu   sync.Mutex
}

// ResolveBranchHead resolves a remote branch to an immutable commit SHA.
func (c *Client) ResolveBranchHead(ctx context.Context, remoteURL, branch string) (string, error) {
	if err := requireDeadline(ctx); err != nil {
		return "", err
	}
	branch = strings.TrimPrefix(strings.TrimSpace(branch), "refs/heads/")
	if branch == "" {
		return "", fmt.Errorf("gitclone: branch required")
	}
	out, err := c.gitRunner().RunTrim(ctx, "", "ls-remote", "--heads", remoteURL, branch)
	if err != nil || out == "" {
		return "", fmt.Errorf("gitclone: resolve branch %q: %w", branch, err)
	}
	return parseLsRemoteHead(out), nil
}

// EnsureCheckout clones or updates dst to targetSHA (full commit or prefix).
func (c *Client) EnsureCheckout(ctx context.Context, remoteURL, dst, targetSHA string) error {
	if err := requireDeadline(ctx); err != nil {
		return err
	}
	targetSHA = strings.TrimSpace(targetSHA)
	if targetSHA == "" {
		return fmt.Errorf("gitclone: target SHA required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	retry := c.retryPolicy()
	if err := clone.Ensure(ctx, retry, remoteURL, dst, targetSHA, false); err != nil {
		return err
	}
	got := strings.TrimSpace(clone.Head(ctx, dst))
	if !shaMatch(got, targetSHA) {
		return &HeadDriftError{Expected: targetSHA, Observed: got, Destination: dst}
	}
	return nil
}

// Head returns the current commit at dst.
func (c *Client) Head(ctx context.Context, dst string) (string, error) {
	if err := requireDeadline(ctx); err != nil {
		return "", err
	}
	sha := strings.TrimSpace(clone.Head(ctx, dst))
	if sha == "" {
		return "", fmt.Errorf("gitclone: empty HEAD at %s", dst)
	}
	return sha, nil
}

// HeadDriftError reports checkout verification failure.
type HeadDriftError struct {
	Expected    string
	Observed    string
	Destination string
}

func (e *HeadDriftError) Error() string {
	return fmt.Sprintf("gitclone: HEAD drift at %s (want %s, got %s)", e.Destination, e.Expected, e.Observed)
}

func (c *Client) retryPolicy() clone.Retry {
	return clone.Retry{Run: c.cloneRunner()}
}

func (c *Client) cloneRunner() clone.Runner {
	return func(ctx context.Context, dir string, env []string, args ...string) (string, error) {
		if err := requireDeadline(ctx); err != nil {
			return "", err
		}
		gitArgs := append([]string{}, auth.GitConfigArgs(c.Cred)...)
		gitArgs = append(gitArgs, args...)
		cmd := exec.CommandContext(ctx, "git", gitArgs...) //nolint:gosec // git argv come from clone runner
		cmd.Dir = dir
		if len(env) > 0 {
			cmd.Env = append(os.Environ(), env...)
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
}

func (c *Client) gitRunner() runner {
	return runner{Cred: c.Cred}
}

type runner struct {
	Cred auth.Credential
}

func (r runner) RunTrim(ctx context.Context, dir string, args ...string) (string, error) {
	var cmdArgs []string
	cmdArgs = append(cmdArgs, auth.GitConfigArgs(r.Cred)...)
	if strings.TrimSpace(dir) != "" {
		cmdArgs = append(cmdArgs, "-C", dir)
	}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...) //nolint:gosec // git argv are validated git subcommands
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func parseLsRemoteHead(lsRemoteOut string) string {
	line := strings.TrimSpace(lsRemoteOut)
	if line == "" {
		return ""
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func shaMatch(got, want string) bool {
	got = strings.TrimSpace(got)
	want = strings.TrimSpace(want)
	if got == "" || want == "" {
		return false
	}
	if got == want {
		return true
	}
	if len(want) >= 7 && strings.HasPrefix(got, want) {
		return true
	}
	if len(got) >= 7 && strings.HasPrefix(want, got) {
		return true
	}
	return false
}

func requireDeadline(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("gitclone: nil context")
	}
	if _, ok := ctx.Deadline(); !ok {
		return fmt.Errorf("gitclone: missing deadline")
	}
	return nil
}

// DefaultContextCacheDir returns a sibling cache directory for a work tree.
func DefaultContextCacheDir(workDir, repoID string) string {
	parent := filepath.Dir(strings.TrimSpace(workDir))
	if parent == "" || parent == "." {
		parent = workDir
	}
	return filepath.Join(parent, "majordomo-context-"+strings.TrimSpace(repoID))
}
