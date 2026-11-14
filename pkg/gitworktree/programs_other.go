//go:build !darwin

package gitworktree

import (
	"context"

	"github.com/shirou/gopsutil/v4/process"
)

// commandLines reads each program's command line with gopsutil. On Linux it comes from /proc/<pid>/cmdline, which
// every user can read by default, so no ps is needed.
func commandLines(ctx context.Context, procs []*process.Process) (map[int]string, error) {
	commands := make(map[int]string, len(procs))
	for _, p := range procs {
		// A program that exits while the list is read has no command line left; it cannot be using the worktree.
		if command, err := p.CmdlineWithContext(ctx); err == nil {
			commands[int(p.Pid)] = command
		}
	}
	return commands, nil
}
